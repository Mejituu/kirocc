package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Auth types. Social and IDC are read from Kiro CLI's SQLite database and are
// refreshable; APIKey is supplied directly via KIRO_API_KEY and is not.
const (
	AuthTypeSocial = "social"
	AuthTypeIDC    = "idc"
	AuthTypeAPIKey = "api_key"
)

// Credentials holds authentication credentials, either read from Kiro CLI's
// SQLite database or, for AuthTypeAPIKey, supplied directly by the user.
type Credentials struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	// ClientSecretExpiresAt is the device registration's client-secret
	// lifetime (auth_kv device-registration row, client_secret_expires_at);
	// 0 when the row carries none (social logins).
	ClientSecretExpiresAt int64
	Region                string // API region
	SSORegion             string // OIDC region (may differ from API region)
	ClientID              string
	ClientSecret          string
	ProfileARN            string // from state table, key "api.codewhisperer.profile"
	AuthType              string // "social", "idc", or "api_key"
}

// ErrNoCredentials is returned when no token key is found in the database.
var ErrNoCredentials = errors.New("no kiro credentials found")

// tokenAuthKVKeys lists the auth_kv keys that may hold a Kiro token, in
// priority order.
var tokenAuthKVKeys = []string{
	"kirocli:social:token",
	"kirocli:odic:token",
	"kirocli:oidc:token",
	"codewhisperer:odic:token",
	"codewhisperer:oidc:token",
}

// OpenDB opens the Kiro CLI SQLite database at path in read-only mode.
func OpenDB(path string) (*sql.DB, error) {
	// Opaque keeps the path literal (no percent-encoding of / or special chars).
	dsn := (&url.URL{Scheme: "file", Opaque: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite db: %w", err)
	}
	return db, nil
}

// ReadCredentials reads authentication credentials from the Kiro CLI SQLite database.
func ReadCredentials(db *sql.DB) (*Credentials, error) {
	creds, _, err := readCredentials(db)
	return creds, err
}

// readCredentials reads credentials together with the shape of the auth_kv row
// they came from, which a refresh write-back needs to preserve.
func readCredentials(db *sql.DB) (*Credentials, *credentialSource, error) {
	creds := &Credentials{}

	// Read token from auth_kv table.
	// Priority: kirocli:social → kirocli:odic/oidc → codewhisperer:odic/oidc
	tokenKey, tokenJSON, authType, err := findFirstAuthKV(db, tokenAuthKVKeys)
	if err != nil {
		return nil, nil, err
	}
	creds.AuthType = authType

	// Token JSON may use camelCase (accessToken) or snake_case (access_token).
	var tokenData struct {
		AccessToken   string `json:"accessToken"`
		RefreshToken  string `json:"refreshToken"`
		ExpiresAt     any    `json:"expiresAt"`
		AccessTokenS  string `json:"access_token"`
		RefreshTokenS string `json:"refresh_token"`
		ExpiresAtS    any    `json:"expires_at"`
		Region        string `json:"region"`
		ProfileARN    string `json:"profileArn"`  // social camelCase
		ProfileARNS   string `json:"profile_arn"` // social snake_case
	}
	if err := json.Unmarshal([]byte(tokenJSON), &tokenData); err != nil {
		return nil, nil, fmt.Errorf("parse token JSON: %w", err)
	}
	creds.AccessToken = coalesce(tokenData.AccessToken, tokenData.AccessTokenS)
	creds.RefreshToken = coalesce(tokenData.RefreshToken, tokenData.RefreshTokenS)
	creds.ExpiresAt = parseExpiresAt(tokenData.ExpiresAt, tokenData.ExpiresAtS)

	// Read device registration.
	// Try both hyphen and underscore variants, and both odic/oidc spellings.
	_, regJSON, _, err := findFirstAuthKV(db, []string{
		"kirocli:social:device-registration",
		"kirocli:odic:device-registration",
		"kirocli:oidc:device-registration",
		"codewhisperer:odic:device-registration",
		"codewhisperer:oidc:device-registration",
		"kirocli:social:device_registration",
		"kirocli:odic:device_registration",
		"kirocli:oidc:device_registration",
		"codewhisperer:odic:device_registration",
		"codewhisperer:oidc:device_registration",
	})
	if err != nil && !errors.Is(err, ErrNoCredentials) {
		return nil, nil, err
	}
	if regJSON != "" {
		var regData struct {
			ClientID      string `json:"clientId"`
			ClientSecret  string `json:"clientSecret"`
			ClientIDS     string `json:"client_id"`
			ClientSecretS string `json:"client_secret"`
			// Kiro-cli writes the registration snake_case; keep a camel
			// spelling for symmetry with the fields above.
			SecretExpiresAt  any `json:"client_secret_expires_at"`
			SecretExpiresAtC any `json:"clientSecretExpiresAt"`
		}
		if err := json.Unmarshal([]byte(regJSON), &regData); err != nil {
			return nil, nil, fmt.Errorf("parse device registration JSON: %w", err)
		}
		creds.ClientID = coalesce(regData.ClientID, regData.ClientIDS)
		creds.ClientSecret = coalesce(regData.ClientSecret, regData.ClientSecretS)
		creds.ClientSecretExpiresAt = parseExpiresAt(regData.SecretExpiresAt, regData.SecretExpiresAtC)
	}

	stateRegion, stateErr := readState(db, "auth.idc.region")
	if stateErr != nil && !errors.Is(stateErr, sql.ErrNoRows) {
		return nil, nil, stateErr
	}

	// State profile ARN may be a JSON object {"arn":"...","profile_name":"..."} or a plain string.
	profileRaw, profileErr := readState(db, "api.codewhisperer.profile")
	if profileErr != nil && !errors.Is(profileErr, sql.ErrNoRows) {
		return nil, nil, profileErr
	}
	stateProfileARN := extractProfileARN(profileRaw)

	// ProfileARN: token JSON wins (social login), state table is fallback (IDC).
	tokenProfileARN := coalesce(tokenData.ProfileARN, tokenData.ProfileARNS)
	creds.ProfileARN = coalesce(tokenProfileARN, stateProfileARN)

	// SSORegion must come from an explicitly stored region (token JSON or state).
	// Do not fall back to the ARN-derived region or "us-east-1" — refreshCredentials
	// relies on an empty SSORegion to fail fast for misconfigured IDC credentials.
	creds.SSORegion = coalesce(tokenData.Region, stateRegion)

	// Region (API region used to build https://runtime.<region>.kiro.dev/) can be
	// derived from the profile ARN and falls back to "us-east-1" when unavailable.
	creds.Region = resolveRegion(tokenData.Region, tokenProfileARN, stateRegion, stateProfileARN)

	return creds, newCredentialSource(tokenKey, tokenJSON), nil
}

// findFirstAuthKV fetches all matching keys in a single query, then returns
// the key and value of the highest-priority key (earliest in the keys slice).
func findFirstAuthKV(db *sql.DB, keys []string) (key, value, authType string, err error) {
	if len(keys) == 0 {
		return "", "", "", ErrNoCredentials
	}

	placeholders := make([]string, len(keys))
	args := make([]any, len(keys))
	for i, k := range keys {
		placeholders[i] = "?"
		args[i] = k
	}
	query := `SELECT key, value FROM auth_kv WHERE key IN (` + strings.Join(placeholders, ",") + `)`

	rows, err := db.Query(query, args...)
	if err != nil {
		return "", "", "", fmt.Errorf("query auth_kv: %w", err)
	}
	defer func() { _ = rows.Close() }()

	found := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return "", "", "", fmt.Errorf("scan auth_kv: %w", err)
		}
		found[k] = v
	}
	if err := rows.Err(); err != nil {
		return "", "", "", fmt.Errorf("iterate auth_kv: %w", err)
	}

	// Return the first match in priority order.
	for _, wanted := range keys {
		if stored, ok := found[wanted]; ok {
			kind := "idc"
			if strings.Contains(wanted, ":social:") {
				kind = "social"
			}
			return wanted, stored, kind, nil
		}
	}
	return "", "", "", ErrNoCredentials
}

// readState reads a single value from the state table by key.
// Values may be stored as JSON strings (with quotes), so we attempt to unquote them.
func readState(db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM state WHERE key = ?`, key).Scan(&value)
	if err != nil {
		return "", err
	}
	// Try to JSON-unmarshal as a string to strip quotes.
	var unquoted string
	if err := json.Unmarshal([]byte(value), &unquoted); err == nil {
		return unquoted, nil
	}
	return value, nil
}

// tokenFieldNames records the JSON field spellings used by one auth_kv token
// row, so a write-back can update the same fields without changing its shape.
type tokenFieldNames struct {
	Access  string
	Refresh string
	Expires string
}

// expiresKind is the JSON representation a token row uses for its expiry.
type expiresKind uint8

const (
	expiresUnix expiresKind = iota
	expiresUnixString
	expiresISO
)

// expiresFormat describes how to write a refreshed expiry back into a token
// row while keeping the representation the row already used.
type expiresFormat struct {
	kind   expiresKind
	layout string // ISO layout when kind is expiresISO
	utc    bool   // original ISO string ended in Z
	offset int    // original ISO zone offset in seconds
}

// credentialSource identifies the auth_kv row a credential came from and the
// shape a refresh write-back must preserve.
type credentialSource struct {
	Key     string
	Fields  tokenFieldNames
	Expires expiresFormat
}

// newCredentialSource inspects a token JSON value and records the key spellings
// and expiry representation a write-back has to keep.
func newCredentialSource(key, tokenJSON string) *credentialSource {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(tokenJSON), &fields); err != nil {
		return &credentialSource{
			Key: key,
			Fields: tokenFieldNames{
				Access:  "access_token",
				Refresh: "refresh_token",
				Expires: "expires_at",
			},
		}
	}
	names := detectTokenFieldNames(fields)
	return &credentialSource{
		Key:     key,
		Fields:  names,
		Expires: detectExpiresFormat(fields[names.Expires]),
	}
}

// detectTokenFieldNames picks the spelling already present for each managed
// field; when a field is absent it follows the row's dominant style.
func detectTokenFieldNames(fields map[string]jsontext.Value) tokenFieldNames {
	camel := false
	for _, key := range []string{"accessToken", "refreshToken", "expiresAt"} {
		if _, ok := fields[key]; ok {
			camel = true
		}
	}
	pick := func(camelKey, snakeKey string) string {
		if _, ok := fields[camelKey]; ok {
			return camelKey
		}
		if _, ok := fields[snakeKey]; ok {
			return snakeKey
		}
		if camel {
			return camelKey
		}
		return snakeKey
	}
	return tokenFieldNames{
		Access:  pick("accessToken", "access_token"),
		Refresh: pick("refreshToken", "refresh_token"),
		Expires: pick("expiresAt", "expires_at"),
	}
}

// detectExpiresFormat inspects the stored expiry value. Unsupported shapes fall
// back to a Unix number, the representation kiro-cli uses for IDC tokens.
func detectExpiresFormat(value jsontext.Value) expiresFormat {
	if len(value) == 0 {
		return expiresFormat{kind: expiresUnix}
	}
	switch value.Kind() {
	case jsontext.KindNumber:
		return expiresFormat{kind: expiresUnix}
	case jsontext.KindString:
		var raw string
		if err := json.Unmarshal(value, &raw); err != nil {
			return expiresFormat{kind: expiresUnix}
		}
		if _, err := strconv.ParseFloat(raw, 64); err == nil {
			return expiresFormat{kind: expiresUnixString}
		}
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return isoExpiresFormat(raw, parsed)
		}
	}
	return expiresFormat{kind: expiresUnix}
}

// isoExpiresFormat records the fractional-second width and zone style of an
// ISO 8601 expiry so the next write keeps them.
func isoExpiresFormat(raw string, parsed time.Time) expiresFormat {
	layout := "2006-01-02T15:04:05"
	if dot := strings.IndexByte(raw, '.'); dot >= 0 {
		digits := 0
		for i := dot + 1; i < len(raw) && raw[i] >= '0' && raw[i] <= '9'; i++ {
			digits++
		}
		if digits > 9 {
			digits = 9
		}
		if digits > 0 {
			layout += "." + strings.Repeat("0", digits)
		}
	}
	utc := strings.HasSuffix(raw, "Z")
	if utc {
		layout += "Z07:00"
	} else {
		layout += "-07:00"
	}
	_, offset := parsed.Zone()
	return expiresFormat{kind: expiresISO, layout: layout, utc: utc, offset: offset}
}

// value renders a Unix expiry using the row's stored representation.
func (f expiresFormat) value(unix int64) jsontext.Value {
	switch f.kind {
	case expiresUnixString:
		encoded, _ := json.Marshal(strconv.FormatInt(unix, 10))
		return jsontext.Value(encoded)
	case expiresISO:
		t := time.Unix(unix, 0)
		if f.utc {
			t = t.UTC()
		} else {
			t = t.In(time.FixedZone("", f.offset))
		}
		encoded, _ := json.Marshal(t.Format(f.layout))
		return jsontext.Value(encoded)
	default:
		return jsontext.Value(strconv.FormatInt(unix, 10))
	}
}

// openRWDB opens the Kiro CLI database for a short write-back transaction. The
// immediate transaction lock makes the read-compare-update sequence atomic
// against kiro-cli or another kirocc instance writing the same row.
func openRWDB(path string) (*sql.DB, error) {
	dsn := (&url.URL{
		Scheme:   "file",
		Opaque:   path,
		RawQuery: "mode=rw&_txlock=immediate",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db rw: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite db rw: %w", err)
	}
	return db, nil
}

// writeBackCredentials writes a rotated refresh token back into the auth_kv row
// the credential was read from. The row must still carry the refresh token the
// refresh used; otherwise another writer (kiro-cli or a concurrent kirocc) has
// replaced the login and the write is skipped.
func writeBackCredentials(ctx context.Context, db *sql.DB, source *credentialSource, oldRefreshToken string, creds *Credentials) error {
	if source == nil || source.Key == "" {
		return nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		return fmt.Errorf("set busy_timeout: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var current string
	err = tx.QueryRowContext(ctx, `SELECT value FROM auth_kv WHERE key = ?`, source.Key).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		slog.Debug("credential write-back skipped: auth_kv row missing", "key", source.Key)
		return nil
	}
	if err != nil {
		return fmt.Errorf("select auth_kv: %w", err)
	}

	stored, ok := refreshTokenFromJSON(current)
	if !ok {
		return fmt.Errorf("parse stored token row %q", source.Key)
	}
	if stored != oldRefreshToken {
		slog.Debug("credential write-back skipped: refresh token changed in database", "key", source.Key)
		return nil
	}

	updated, err := rewriteTokenJSON([]byte(current), newCredentialSource(source.Key, current), creds)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE auth_kv SET value = ? WHERE key = ?`, string(updated), source.Key)
	if err != nil {
		return fmt.Errorf("update auth_kv: %w", err)
	}
	if changed, err := result.RowsAffected(); err == nil && changed == 0 {
		slog.Warn("credential write-back: auth_kv row not updated", "key", source.Key)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	slog.Debug("credentials written back to kiro-cli database", "key", source.Key)
	return nil
}

// refreshTokenFromJSON returns the refresh token stored in a token row,
// accepting both field spellings.
func refreshTokenFromJSON(tokenJSON string) (string, bool) {
	var data struct {
		RefreshToken  string `json:"refreshToken"`
		RefreshTokenS string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(tokenJSON), &data); err != nil {
		return "", false
	}
	return coalesce(data.RefreshToken, data.RefreshTokenS), true
}

// rewriteTokenJSON replaces the managed fields in a token row while copying
// every other member — name, order and value — unchanged.
func rewriteTokenJSON(base []byte, source *credentialSource, creds *Credentials) ([]byte, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(base))
	tok, err := dec.ReadToken()
	if err != nil || tok.Kind() != jsontext.KindBeginObject {
		return nil, fmt.Errorf("token JSON is not an object")
	}

	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf)
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return nil, err
	}
	seenAccess, seenRefresh, seenExpires := false, false, false
	for {
		tok, err = dec.ReadToken()
		if err != nil {
			return nil, err
		}
		if tok.Kind() == jsontext.KindEndObject {
			break
		}
		if tok.Kind() != jsontext.KindString {
			return nil, fmt.Errorf("token JSON object has a non-string key")
		}
		key := tok.String()
		if err := enc.WriteToken(jsontext.String(key)); err != nil {
			return nil, err
		}
		switch key {
		case source.Fields.Access:
			seenAccess = true
			if err := dec.SkipValue(); err != nil {
				return nil, err
			}
			if err := enc.WriteValue(jsonStringValue(creds.AccessToken)); err != nil {
				return nil, err
			}
		case source.Fields.Refresh:
			seenRefresh = true
			if err := dec.SkipValue(); err != nil {
				return nil, err
			}
			if err := enc.WriteValue(jsonStringValue(creds.RefreshToken)); err != nil {
				return nil, err
			}
		case source.Fields.Expires:
			seenExpires = true
			if err := dec.SkipValue(); err != nil {
				return nil, err
			}
			if err := enc.WriteValue(source.Expires.value(creds.ExpiresAt)); err != nil {
				return nil, err
			}
		default:
			value, err := dec.ReadValue()
			if err != nil {
				return nil, err
			}
			if err := enc.WriteValue(value); err != nil {
				return nil, err
			}
		}
	}

	appendField := func(name string, value jsontext.Value) error {
		if err := enc.WriteToken(jsontext.String(name)); err != nil {
			return err
		}
		return enc.WriteValue(value)
	}
	if !seenAccess {
		if err := appendField(source.Fields.Access, jsonStringValue(creds.AccessToken)); err != nil {
			return nil, err
		}
	}
	if !seenRefresh {
		if err := appendField(source.Fields.Refresh, jsonStringValue(creds.RefreshToken)); err != nil {
			return nil, err
		}
	}
	if !seenExpires {
		if err := appendField(source.Fields.Expires, source.Expires.value(creds.ExpiresAt)); err != nil {
			return nil, err
		}
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return nil, err
	}
	// The encoder appends a newline after a top-level value; the database
	// stores the JSON object without it.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// jsonStringValue encodes s as a JSON string value.
func jsonStringValue(s string) jsontext.Value {
	encoded, _ := json.Marshal(s)
	return jsontext.Value(encoded)
}

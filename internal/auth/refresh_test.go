package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tu "github.com/d-kuro/kirocc/internal/testutil"
	_ "modernc.org/sqlite"
)

// newTCP4TestServer creates an httptest.Server bound to tcp4 to avoid IPv6 bind failures in sandboxed environments.
func newTCP4TestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	return tu.NewTCP4TestServer(t, handler)
}

// setupTestDBWithCreds creates an in-memory DB pre-populated with credentials.
func setupTestDBWithCreds(t *testing.T, accessToken, refreshToken string, expiresAt int64, region string) *sql.DB {
	t.Helper()
	db := setupTestDB(t)

	tokenJSON, _ := json.Marshal(map[string]any{
		"accessToken":  accessToken,
		"refreshToken": refreshToken,
		"expiresAt":    expiresAt,
	})
	_, err := db.Exec(`INSERT INTO auth_kv (key, value) VALUES (?, ?)`,
		"kirocli:odic:token", string(tokenJSON),
	)
	if err != nil {
		t.Fatalf("insert token: %v", err)
	}

	regJSON, _ := json.Marshal(map[string]string{
		"clientId":     "test-client-id",
		"clientSecret": "test-client-secret",
	})
	_, err = db.Exec(`INSERT INTO auth_kv (key, value) VALUES (?, ?)`,
		"kirocli:odic:device_registration", string(regJSON),
	)
	if err != nil {
		t.Fatalf("insert device reg: %v", err)
	}

	if region != "" {
		_, err = db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)`,
			"auth.idc.region", region,
		)
		if err != nil {
			t.Fatalf("insert region: %v", err)
		}
	}

	return db
}

func TestRefreshToken_Success(t *testing.T) {
	newExpiry := time.Now().Add(8 * time.Hour).Unix()
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/token" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body map[string]string
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body["grantType"] != "refresh_token" {
			http.Error(w, "bad grant type", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"accessToken":  "new-access-token",
			"refreshToken": "new-refresh-token",
			"expiresIn":    28800,
		})
		_, _ = w.Write([]byte("\n"))
		_ = newExpiry
	}))
	defer srv.Close()

	creds := &Credentials{
		RefreshToken: "old-refresh-token",
		ClientID:     "cid",
		ClientSecret: "csec",
		SSORegion:    "us-east-1",
	}

	result, _, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshOIDCToken(context.Background(), creds, srv.URL+"/token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.AccessToken != "new-access-token" {
		t.Errorf("AccessToken = %q, want %q", result.AccessToken, "new-access-token")
	}
	if result.RefreshToken != "new-refresh-token" {
		t.Errorf("RefreshToken = %q, want %q", result.RefreshToken, "new-refresh-token")
	}
	if result.ExpiresAt <= time.Now().Unix() {
		t.Errorf("ExpiresAt should be in the future, got %d", result.ExpiresAt)
	}
}

func TestRefreshToken_Failure(t *testing.T) {
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	creds := &Credentials{
		RefreshToken: "bad-token",
		ClientID:     "cid",
		ClientSecret: "csec",
		SSORegion:    "us-east-1",
	}

	_, _, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshOIDCToken(context.Background(), creds, srv.URL+"/token")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestAuthManager_CacheValid(t *testing.T) {
	futureExpiry := time.Now().Add(10 * time.Minute).Unix()
	db := setupTestDBWithCreds(t, "cached-token", "ref", futureExpiry, "us-east-1")
	defer func() { _ = db.Close() }()

	mgr := newAuthManagerWithDB(db)

	// Prime the cache
	creds1, err := mgr.GetToken(context.Background())
	if err != nil {
		t.Fatalf("first GetToken: %v", err)
	}

	// Second call should return cached value without hitting DB again
	creds2, err := mgr.GetToken(context.Background())
	if err != nil {
		t.Fatalf("second GetToken: %v", err)
	}

	if creds1.AccessToken != creds2.AccessToken {
		t.Error("expected same credentials (cache hit), got different values")
	}
	if creds2.AccessToken != "cached-token" {
		t.Errorf("AccessToken = %q, want %q", creds2.AccessToken, "cached-token")
	}
}

func TestAuthManager_CacheExpired_DBRefresh(t *testing.T) {
	// Token expired, but DB has a fresh one (simulating kiro-cli updating the DB)
	expiredExpiry := time.Now().Add(-1 * time.Minute).Unix()
	db := setupTestDBWithCreds(t, "stale-token", "ref", expiredExpiry, "us-east-1")
	defer func() { _ = db.Close() }()

	mgr := newAuthManagerWithDB(db)

	// Prime cache with expired token
	mgr.cached = &Credentials{
		AccessToken: "stale-token",
		ExpiresAt:   expiredExpiry,
	}

	// Update DB with fresh token
	freshExpiry := time.Now().Add(8 * time.Hour).Unix()
	freshJSON, _ := json.Marshal(map[string]any{
		"accessToken":  "fresh-from-db",
		"refreshToken": "ref2",
		"expiresAt":    freshExpiry,
	})
	_, err := db.Exec(`UPDATE auth_kv SET value = ? WHERE key = ?`, string(freshJSON), "kirocli:odic:token")
	if err != nil {
		t.Fatalf("update db: %v", err)
	}

	creds, err := mgr.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if creds.AccessToken != "fresh-from-db" {
		t.Errorf("AccessToken = %q, want %q", creds.AccessToken, "fresh-from-db")
	}
}

func TestAuthManager_DBAlsoExpired_OIDCRefresh(t *testing.T) {
	expiredExpiry := time.Now().Add(-1 * time.Minute).Unix()
	db := setupTestDBWithCreds(t, "expired-token", "old-refresh", expiredExpiry, "us-east-1")
	defer func() { _ = db.Close() }()

	callCount := 0
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"accessToken":  "oidc-refreshed-token",
			"refreshToken": "new-refresh",
			"expiresIn":    28800,
		})
		_, _ = w.Write([]byte("\n"))
	}))
	defer srv.Close()

	mgr := newAuthManagerWithDB(db)
	mgr.oidcEndpointFn = func(ssoRegion string) string {
		return srv.URL + "/token"
	}

	creds, err := mgr.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if creds.AccessToken != "oidc-refreshed-token" {
		t.Errorf("AccessToken = %q, want %q", creds.AccessToken, "oidc-refreshed-token")
	}
	if callCount != 1 {
		t.Errorf("OIDC call count = %d, want 1", callCount)
	}
}

func TestAuthManager_ConcurrentAccess(t *testing.T) {
	expiredExpiry := time.Now().Add(-1 * time.Minute).Unix()
	db := setupTestDBWithCreds(t, "expired-token", "old-refresh", expiredExpiry, "us-east-1")
	defer func() { _ = db.Close() }()

	callCount := 0
	var callMu sync.Mutex
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callMu.Lock()
		callCount++
		callMu.Unlock()
		// Simulate slow OIDC endpoint
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"accessToken":  "concurrent-token",
			"refreshToken": "new-refresh",
			"expiresIn":    28800,
		})
		_, _ = w.Write([]byte("\n"))
	}))
	defer srv.Close()

	mgr := newAuthManagerWithDB(db)
	mgr.oidcEndpointFn = func(ssoRegion string) string {
		return srv.URL + "/token"
	}

	const goroutines = 10
	var wg sync.WaitGroup
	errors := make([]error, goroutines)
	tokens := make([]string, goroutines)

	for i := range goroutines {
		wg.Go(func() {
			creds, err := mgr.GetToken(context.Background())
			errors[i] = err
			if creds != nil {
				tokens[i] = creds.AccessToken
			}
		})
	}
	wg.Wait()

	for i, err := range errors {
		if err != nil {
			t.Errorf("goroutine %d error: %v", i, err)
		}
	}
	for i, tok := range tokens {
		if tok != "concurrent-token" {
			t.Errorf("goroutine %d token = %q, want %q", i, tok, "concurrent-token")
		}
	}
	if callCount != 1 {
		t.Errorf("OIDC call count = %d, want 1 (mutex should prevent duplicate calls)", callCount)
	}
}

func TestRefreshSocialToken_Success(t *testing.T) {
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body["refreshToken"] != "social-ref" {
			http.Error(w, "wrong refresh token", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"accessToken":  "social-new-acc",
			"refreshToken": "social-new-ref",
			"expiresIn":    3600,
			"profileArn":   "arn:aws:codewhisperer:us-east-1:123:profile/social",
		})
		_, _ = w.Write([]byte("\n"))
	}))
	defer srv.Close()

	creds := &Credentials{
		RefreshToken: "social-ref",
		Region:       "us-east-1",
		AuthType:     "social",
	}

	result, _, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshSocialToken(context.Background(), creds, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.AccessToken != "social-new-acc" {
		t.Errorf("AccessToken = %q, want %q", result.AccessToken, "social-new-acc")
	}
	if result.RefreshToken != "social-new-ref" {
		t.Errorf("RefreshToken = %q, want %q", result.RefreshToken, "social-new-ref")
	}
	if result.ProfileARN != "arn:aws:codewhisperer:us-east-1:123:profile/social" {
		t.Errorf("ProfileARN = %q, want social ARN", result.ProfileARN)
	}
}

func TestRefreshSocialToken_Failure(t *testing.T) {
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	creds := &Credentials{RefreshToken: "bad"}
	_, _, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshSocialToken(context.Background(), creds, srv.URL)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestAuthManager_SocialRefreshPath(t *testing.T) {
	expiredExpiry := time.Now().Add(-1 * time.Minute).Unix()
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	// Insert social token (no device registration → social path)
	tokenJSON, _ := json.Marshal(map[string]any{
		"accessToken":  "expired-social",
		"refreshToken": "social-ref",
		"expiresAt":    expiredExpiry,
	})
	_, err := db.Exec(`INSERT INTO auth_kv (key, value) VALUES (?, ?)`,
		"kirocli:social:token", string(tokenJSON),
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"accessToken":  "social-refreshed",
			"refreshToken": "social-new-ref",
			"expiresIn":    3600,
		})
		_, _ = w.Write([]byte("\n"))
	}))
	defer srv.Close()

	mgr := newAuthManagerWithDB(db)
	mgr.socialEndpointFn = func(region string) string {
		return srv.URL
	}

	creds, err := mgr.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if creds.AccessToken != "social-refreshed" {
		t.Errorf("AccessToken = %q, want %q", creds.AccessToken, "social-refreshed")
	}
	if creds.AuthType != "social" {
		t.Errorf("AuthType = %q, want %q", creds.AuthType, "social")
	}
}

func TestRefreshOIDCToken_EmptyAccessToken(t *testing.T) {
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"accessToken":  "",
			"refreshToken": "ref",
			"expiresIn":    3600,
		})
		_, _ = w.Write([]byte("\n"))
	}))
	defer srv.Close()

	creds := &Credentials{RefreshToken: "ref", ClientID: "c", ClientSecret: "s"}
	_, _, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshOIDCToken(context.Background(), creds, srv.URL)
	if err == nil {
		t.Fatal("expected error for empty accessToken")
	}
}

func TestRefreshOIDCToken_ZeroExpiresIn(t *testing.T) {
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"accessToken":  "tok",
			"refreshToken": "ref",
			"expiresIn":    0,
		})
		_, _ = w.Write([]byte("\n"))
	}))
	defer srv.Close()

	creds := &Credentials{RefreshToken: "ref", ClientID: "c", ClientSecret: "s"}
	_, _, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshOIDCToken(context.Background(), creds, srv.URL)
	if err == nil {
		t.Fatal("expected error for zero expiresIn")
	}
}

func TestRefreshSocialToken_EmptyAccessToken(t *testing.T) {
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"accessToken":  "",
			"refreshToken": "ref",
			"expiresIn":    3600,
		})
		_, _ = w.Write([]byte("\n"))
	}))
	defer srv.Close()

	creds := &Credentials{RefreshToken: "ref"}
	_, _, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshSocialToken(context.Background(), creds, srv.URL)
	if err == nil {
		t.Fatal("expected error for empty accessToken")
	}
}

func TestRefreshOIDCToken_RespectsContext(t *testing.T) {
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow endpoint.
		time.Sleep(5 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"accessToken":  "tok",
			"refreshToken": "ref",
			"expiresIn":    3600,
		})
		_, _ = w.Write([]byte("\n"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	creds := &Credentials{RefreshToken: "ref", ClientID: "c", ClientSecret: "s"}
	_, _, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshOIDCToken(ctx, creds, srv.URL)
	if err == nil {
		t.Fatal("expected error from context timeout")
	}
}

func TestRefreshSocialToken_RespectsContext(t *testing.T) {
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"accessToken":  "tok",
			"refreshToken": "ref",
			"expiresIn":    3600,
		})
		_, _ = w.Write([]byte("\n"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	creds := &Credentials{RefreshToken: "ref"}
	_, _, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshSocialToken(ctx, creds, srv.URL)
	if err == nil {
		t.Fatal("expected error from context timeout")
	}
}

func TestDefaultEndpoints(t *testing.T) {
	oidc := defaultOIDCEndpoint("us-west-2")
	if oidc != "https://oidc.us-west-2.amazonaws.com/token" {
		t.Errorf("defaultOIDCEndpoint = %q", oidc)
	}

	social := defaultSocialEndpoint("eu-west-1")
	if social != "https://prod.eu-west-1.auth.desktop.kiro.dev/refreshToken" {
		t.Errorf("defaultSocialEndpoint = %q", social)
	}

	socialDefault := defaultSocialEndpoint("")
	if socialDefault != "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken" {
		t.Errorf("defaultSocialEndpoint('') = %q, want us-east-1 fallback", socialDefault)
	}
}

func TestNewAuthManager(t *testing.T) {
	mgr := NewAuthManager("/tmp/nonexistent-test.sqlite3")
	if mgr == nil {
		t.Fatal("NewAuthManager returned nil")
	}
	// GetToken should fail gracefully when DB doesn't exist
	_, err := mgr.GetToken(context.Background())
	if err == nil {
		t.Error("expected error for nonexistent DB, got nil")
	}
}

// captureLogs redirects the default slog logger for one test and returns the
// captured output. Debug is enabled so write-back skip decisions are visible.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func insertTokenRow(t *testing.T, db *sql.DB, key, value string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO auth_kv (key, value) VALUES (?, ?)`, key, value); err != nil {
		t.Fatalf("insert %s: %v", key, err)
	}
}

func insertStateRow(t *testing.T, db *sql.DB, key, value string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)`, key, value); err != nil {
		t.Fatalf("insert state %s: %v", key, err)
	}
}

func readTokenRow(t *testing.T, db *sql.DB, key string) string {
	t.Helper()
	var value string
	if err := db.QueryRow(`SELECT value FROM auth_kv WHERE key = ?`, key).Scan(&value); err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return value
}

func decodeTokenRow(t *testing.T, value string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		t.Fatalf("decode token row %q: %v", value, err)
	}
	return decoded
}

func assertExpiryNear(t *testing.T, got, want int64) {
	t.Helper()
	if diff := got - want; diff < -120 || diff > 120 {
		t.Fatalf("expiry = %d, want about %d (diff %d)", got, want, diff)
	}
}

// oidcRefreshServer answers every token request with the given values; an empty
// refreshToken omits the field, as AWS does when it does not rotate it.
func oidcRefreshServer(t *testing.T, accessToken, refreshToken string, expiresIn int64) *httptest.Server {
	t.Helper()
	return newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]any{
			"accessToken": accessToken,
			"expiresIn":   expiresIn,
		}
		if refreshToken != "" {
			response["refreshToken"] = refreshToken
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, response)
		_, _ = w.Write([]byte("\n"))
	}))
}

// createFileTestDB creates a schema-initialized SQLite file, exercising the
// production read-only/mode=rw open paths instead of the injected in-memory DB.
func createFileTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kiro-cli.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open file db: %v", err)
	}
	db.SetMaxOpenConns(1)
	createAuthSchema(t, db)
	return path, db
}

func TestAuthManager_WriteBack_OIDCCamelNumeric(t *testing.T) {
	expired := time.Now().Add(-time.Minute).Unix()
	db := setupTestDBWithCreds(t, "old-access", "old-refresh", expired, "us-east-1")
	defer func() { _ = db.Close() }()

	seed, _ := json.Marshal(map[string]any{
		"accessToken":  "old-access",
		"refreshToken": "old-refresh",
		"expiresAt":    expired,
		"region":       "us-east-1",
		"profile_arn":  "arn:aws:codewhisperer:us-east-1:123:profile/p",
		"scopes":       []string{"sso:account:access"},
		"start_url":    "https://example.invalid/start",
		"oauth_flow":   "device",
	})
	if _, err := db.Exec(`UPDATE auth_kv SET value = ? WHERE key = ?`, string(seed), "kirocli:odic:token"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	srv := oidcRefreshServer(t, "new-access", "new-refresh", 28800)
	defer srv.Close()
	mgr := newAuthManagerWithDB(db)
	mgr.oidcEndpointFn = func(string) string { return srv.URL }

	creds, err := mgr.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if creds.AccessToken != "new-access" {
		t.Fatalf("AccessToken = %q, want new-access", creds.AccessToken)
	}

	row := decodeTokenRow(t, readTokenRow(t, db, "kirocli:odic:token"))
	if strings.HasSuffix(readTokenRow(t, db, "kirocli:odic:token"), "\n") {
		t.Fatal("stored token JSON has a trailing newline")
	}
	if row["accessToken"] != "new-access" || row["refreshToken"] != "new-refresh" {
		t.Fatalf("managed fields not updated: %v", row)
	}
	if _, ok := row["access_token"]; ok {
		t.Fatalf("camelCase row gained snake_case fields: %v", row)
	}
	if _, ok := row["refresh_token"]; ok {
		t.Fatalf("camelCase row gained snake_case fields: %v", row)
	}
	expires, ok := row["expiresAt"].(float64)
	if !ok {
		t.Fatalf("expiresAt type = %T, want number: %v", row["expiresAt"], row)
	}
	assertExpiryNear(t, int64(expires), time.Now().Unix()+28800)
	for key, want := range map[string]any{
		"region":      "us-east-1",
		"profile_arn": "arn:aws:codewhisperer:us-east-1:123:profile/p",
		"start_url":   "https://example.invalid/start",
		"oauth_flow":  "device",
	} {
		if row[key] != want {
			t.Fatalf("extra field %s = %v, want %v", key, row[key], want)
		}
	}
	scopes, ok := row["scopes"].([]any)
	if !ok || len(scopes) != 1 || scopes[0] != "sso:account:access" {
		t.Fatalf("scopes = %v, want [sso:account:access]", row["scopes"])
	}
}

func TestAuthManager_WriteBack_SnakeTargetsHitKey(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	expired := time.Now().Add(-time.Minute).Unix()
	seed, _ := json.Marshal(map[string]any{
		"access_token":  "old-access",
		"refresh_token": "old-refresh",
		"expires_at":    expired,
		"oauth_flow":    "device",
		"start_url":     "https://example.invalid/start",
	})
	insertTokenRow(t, db, "codewhisperer:odic:token", string(seed))
	insertTokenRow(t, db, "kirocli:odic:device-registration", `{"clientId":"cid","clientSecret":"csec"}`)
	insertStateRow(t, db, "auth.idc.region", "us-east-1")

	srv := oidcRefreshServer(t, "new-access", "new-refresh", 28800)
	defer srv.Close()
	mgr := newAuthManagerWithDB(db)
	mgr.oidcEndpointFn = func(string) string { return srv.URL }

	if _, err := mgr.GetToken(context.Background()); err != nil {
		t.Fatalf("GetToken: %v", err)
	}

	row := decodeTokenRow(t, readTokenRow(t, db, "codewhisperer:odic:token"))
	if row["access_token"] != "new-access" || row["refresh_token"] != "new-refresh" {
		t.Fatalf("managed fields not updated: %v", row)
	}
	if _, ok := row["accessToken"]; ok {
		t.Fatalf("snake_case row gained camelCase fields: %v", row)
	}
	expires, ok := row["expires_at"].(float64)
	if !ok {
		t.Fatalf("expires_at type = %T, want number: %v", row["expires_at"], row)
	}
	assertExpiryNear(t, int64(expires), time.Now().Unix()+28800)
	if row["oauth_flow"] != "device" || row["start_url"] != "https://example.invalid/start" {
		t.Fatalf("extra fields changed: %v", row)
	}

	var created int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_kv WHERE key = ?`, "kirocli:odic:token").Scan(&created); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if created != 0 {
		t.Fatalf("write-back created kirocli:odic:token instead of updating the hit key")
	}
}

func TestAuthManager_WriteBack_SocialISOExpiry(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	oldISO := time.Now().Add(-time.Minute).UTC().Format("2006-01-02T15:04:05.000Z")
	seed, _ := json.Marshal(map[string]any{
		"accessToken":  "old-social-access",
		"refreshToken": "old-social-refresh",
		"expiresAt":    oldISO,
		"region":       "us-east-1",
		"profileArn":   "arn:aws:codewhisperer:us-east-1:123:profile/social",
		"start_url":    "https://example.invalid/social",
	})
	insertTokenRow(t, db, "kirocli:social:token", string(seed))

	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"accessToken":  "social-new-access",
			"refreshToken": "social-new-refresh",
			"expiresIn":    7200,
			"profileArn":   "arn:aws:codewhisperer:us-east-1:123:profile/other",
		})
		_, _ = w.Write([]byte("\n"))
	}))
	defer srv.Close()

	mgr := newAuthManagerWithDB(db)
	mgr.socialEndpointFn = func(string) string { return srv.URL }

	creds, err := mgr.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if creds.AuthType != "social" {
		t.Fatalf("AuthType = %q, want social", creds.AuthType)
	}

	row := decodeTokenRow(t, readTokenRow(t, db, "kirocli:social:token"))
	if row["accessToken"] != "social-new-access" || row["refreshToken"] != "social-new-refresh" {
		t.Fatalf("managed fields not updated: %v", row)
	}
	iso, ok := row["expiresAt"].(string)
	if !ok {
		t.Fatalf("expiresAt type = %T, want ISO string: %v", row["expiresAt"], row)
	}
	if !strings.HasSuffix(iso, "Z") || !strings.Contains(iso, ".") {
		t.Fatalf("expiresAt lost its ISO shape: %q", iso)
	}
	fraction := strings.TrimSuffix(iso[strings.IndexByte(iso, '.')+1:], "Z")
	if len(fraction) != 3 {
		t.Fatalf("expiresAt fraction = %q, want 3 digits", fraction)
	}
	parsed, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		t.Fatalf("parse expiresAt %q: %v", iso, err)
	}
	assertExpiryNear(t, parsed.Unix(), time.Now().Unix()+7200)
	// profileArn is not a managed field: the row keeps its old value even
	// though the refresh response carried a new one.
	if row["profileArn"] != "arn:aws:codewhisperer:us-east-1:123:profile/social" {
		t.Fatalf("extra profileArn changed: %v", row["profileArn"])
	}
	if row["start_url"] != "https://example.invalid/social" {
		t.Fatalf("extra start_url changed: %v", row["start_url"])
	}
}

func TestAuthManager_WriteBack_SkippedWithoutRotatedRefreshToken(t *testing.T) {
	expired := time.Now().Add(-time.Minute).Unix()
	db := setupTestDBWithCreds(t, "old-access", "old-refresh", expired, "us-east-1")
	defer func() { _ = db.Close() }()
	before := readTokenRow(t, db, "kirocli:odic:token")

	srv := oidcRefreshServer(t, "new-access", "", 28800)
	defer srv.Close()
	mgr := newAuthManagerWithDB(db)
	mgr.oidcEndpointFn = func(string) string { return srv.URL }

	if _, err := mgr.GetToken(context.Background()); err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if after := readTokenRow(t, db, "kirocli:odic:token"); after != before {
		t.Fatalf("database changed without a rotated refresh token:\n before %s\n after  %s", before, after)
	}
}

func TestAuthManager_WriteBack_GuardSkipsConcurrentLogin(t *testing.T) {
	expired := time.Now().Add(-time.Minute).Unix()
	db := setupTestDBWithCreds(t, "old-access", "old-refresh", expired, "us-east-1")
	defer func() { _ = db.Close() }()

	replacement, _ := json.Marshal(map[string]any{
		"accessToken":  "kiro-cli-access",
		"refreshToken": "kiro-cli-refresh",
		"expiresAt":    time.Now().Add(8 * time.Hour).Unix(),
	})
	srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Another writer replaces the login while the refresh is in flight.
		if _, err := db.Exec(`UPDATE auth_kv SET value = ? WHERE key = ?`, string(replacement), "kirocli:odic:token"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{
			"accessToken":  "new-access",
			"refreshToken": "new-refresh",
			"expiresIn":    28800,
		})
		_, _ = w.Write([]byte("\n"))
	}))
	defer srv.Close()

	logs := captureLogs(t)
	mgr := newAuthManagerWithDB(db)
	mgr.oidcEndpointFn = func(string) string { return srv.URL }

	creds, err := mgr.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if creds.AccessToken != "new-access" {
		t.Fatalf("AccessToken = %q, want new-access", creds.AccessToken)
	}

	row := decodeTokenRow(t, readTokenRow(t, db, "kirocli:odic:token"))
	if row["refreshToken"] != "kiro-cli-refresh" || row["accessToken"] != "kiro-cli-access" {
		t.Fatalf("write-back overwrote the concurrent login: %v", row)
	}
	if !strings.Contains(logs.String(), "refresh token changed in database") {
		t.Fatalf("guard skip not logged:\n%s", logs.String())
	}
}

func TestAuthManager_WriteBack_FailureKeepsRefreshedCredentials(t *testing.T) {
	expired := time.Now().Add(-time.Minute).Unix()
	db := setupTestDBWithCreds(t, "old-access", "old-refresh", expired, "us-east-1")
	defer func() { _ = db.Close() }()
	before := readTokenRow(t, db, "kirocli:odic:token")

	if _, err := db.Exec(`CREATE TRIGGER fail_auth_kv_update BEFORE UPDATE ON auth_kv BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	logs := captureLogs(t)

	srv := oidcRefreshServer(t, "new-access", "new-refresh", 28800)
	defer srv.Close()
	mgr := newAuthManagerWithDB(db)
	mgr.oidcEndpointFn = func(string) string { return srv.URL }

	creds, err := mgr.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken must succeed when write-back fails: %v", err)
	}
	if creds.AccessToken != "new-access" {
		t.Fatalf("AccessToken = %q, want new-access", creds.AccessToken)
	}
	if after := readTokenRow(t, db, "kirocli:odic:token"); after != before {
		t.Fatalf("failed write-back changed the row")
	}
	if !strings.Contains(logs.String(), "credential write-back failed") {
		t.Fatalf("write failure not logged:\n%s", logs.String())
	}
}

func TestAuthManager_PersistRefreshedToken_BadPathWarns(t *testing.T) {
	logs := captureLogs(t)
	mgr := &AuthManager{dbPath: filepath.Join(t.TempDir(), "missing", "kiro-cli.sqlite3")}
	source := &credentialSource{
		Key:    "kirocli:odic:token",
		Fields: tokenFieldNames{Access: "accessToken", Refresh: "refreshToken", Expires: "expiresAt"},
	}
	mgr.persistRefreshedToken(context.Background(), source, "old", &Credentials{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	})
	if !strings.Contains(logs.String(), "credential write-back failed") {
		t.Fatalf("bad path not logged:\n%s", logs.String())
	}
}

func TestAuthManager_WriteBack_FileDatabase(t *testing.T) {
	path, db := createFileTestDB(t)
	expired := time.Now().Add(-time.Minute).Unix()
	seed, _ := json.Marshal(map[string]any{
		"accessToken":  "old-access",
		"refreshToken": "old-refresh",
		"expiresAt":    expired,
		"start_url":    "https://example.invalid/start",
	})
	insertTokenRow(t, db, "kirocli:odic:token", string(seed))
	insertTokenRow(t, db, "kirocli:odic:device-registration", `{"clientId":"cid","clientSecret":"csec"}`)
	insertStateRow(t, db, "auth.idc.region", "us-east-1")
	_ = db.Close()

	srv := oidcRefreshServer(t, "new-access", "new-refresh", 28800)
	defer srv.Close()
	mgr := NewAuthManager(path)
	mgr.oidcEndpointFn = func(string) string { return srv.URL }

	if _, err := mgr.GetToken(context.Background()); err != nil {
		t.Fatalf("GetToken: %v", err)
	}

	ro, err := OpenDB(path)
	if err != nil {
		t.Fatalf("open read-only db: %v", err)
	}
	defer func() { _ = ro.Close() }()
	row := decodeTokenRow(t, readTokenRow(t, ro, "kirocli:odic:token"))
	if row["accessToken"] != "new-access" || row["refreshToken"] != "new-refresh" {
		t.Fatalf("file database not updated: %v", row)
	}
	if row["start_url"] != "https://example.invalid/start" {
		t.Fatalf("extra field changed: %v", row)
	}
}

func TestDoTokenRefresh_LogsRefreshTokenCarried(t *testing.T) {
	for _, carried := range []bool{true, false} {
		name := "absent"
		logged := "carried=false"
		if carried {
			name = "carried"
			logged = "carried=true"
		}
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			refreshToken := ""
			if carried {
				refreshToken = "rotated-token-value"
			}
			srv := oidcRefreshServer(t, "new-access", refreshToken, 3600)
			defer srv.Close()

			creds := &Credentials{RefreshToken: "old", ClientID: "cid", ClientSecret: "csec", SSORegion: "us-east-1"}
			_, got, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshOIDCToken(context.Background(), creds, srv.URL)
			if err != nil {
				t.Fatalf("refreshOIDCToken: %v", err)
			}
			if got != carried {
				t.Fatalf("carried = %v, want %v", got, carried)
			}
			out := logs.String()
			if !strings.Contains(out, "refresh response carried refresh_token") {
				t.Fatalf("observation log missing:\n%s", out)
			}
			if !strings.Contains(out, logged) {
				t.Fatalf("observation log does not say %s:\n%s", logged, out)
			}
			if strings.Contains(out, "rotated-token-value") {
				t.Fatalf("token value leaked into logs:\n%s", out)
			}
		})
	}
}

func TestDoTokenRefresh_ExpiredRefreshTokenGuidance(t *testing.T) {
	for _, body := range []string{
		`{"error":"invalid_grant"}`,
		`{"error":"expired_token"}`,
		`{"error":"INVALID_GRANT"}`,
	} {
		t.Run(body, func(t *testing.T) {
			logs := captureLogs(t)
			srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, body, http.StatusBadRequest)
			}))
			defer srv.Close()

			creds := &Credentials{RefreshToken: "old", ClientID: "cid", ClientSecret: "csec", SSORegion: "us-east-1"}
			_, _, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshOIDCToken(context.Background(), creds, srv.URL)
			if !errors.Is(err, ErrRefreshTokenExpired) {
				t.Fatalf("err = %v, want ErrRefreshTokenExpired", err)
			}
			message := err.Error()
			if !strings.Contains(message, "kiro-cli 重新登录") || !strings.Contains(message, "KIRO_API_KEY") {
				t.Fatalf("guidance missing from %q", message)
			}
			out := logs.String()
			if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "invalid refresh token") {
				t.Fatalf("invalid_grant body not logged at warn:\n%s", out)
			}
		})
	}

	t.Run("other_400_keeps_generic_error", func(t *testing.T) {
		logs := captureLogs(t)
		srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusBadRequest)
		}))
		defer srv.Close()

		creds := &Credentials{RefreshToken: "old", ClientID: "cid", ClientSecret: "csec", SSORegion: "us-east-1"}
		_, _, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshOIDCToken(context.Background(), creds, srv.URL)
		if err == nil {
			t.Fatal("expected error")
		}
		if errors.Is(err, ErrRefreshTokenExpired) {
			t.Fatalf("plain 400 classified as expired refresh token: %v", err)
		}
		if strings.Contains(err.Error(), "KIRO_API_KEY") {
			t.Fatalf("plain 400 carries guidance: %v", err)
		}
		if !strings.Contains(logs.String(), "level=DEBUG") {
			t.Fatalf("plain error body should stay at debug:\n%s", logs.String())
		}
	})

	t.Run("other_401_keeps_generic_error", func(t *testing.T) {
		srv := newTCP4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}))
		defer srv.Close()

		creds := &Credentials{RefreshToken: "old", ClientID: "cid", ClientSecret: "csec", SSORegion: "us-east-1"}
		_, _, err := (&AuthManager{httpClient: newDefaultHTTPClient()}).refreshOIDCToken(context.Background(), creds, srv.URL)
		if err == nil {
			t.Fatal("expected error")
		}
		if errors.Is(err, ErrRefreshTokenExpired) {
			t.Fatalf("401 classified as expired refresh token: %v", err)
		}
	})
}

// TestReadCredentials_ClientSecretExpiry checks the registration's
// client_secret_expires_at lands on Credentials.
func TestReadCredentials_ClientSecretExpiry(t *testing.T) {
	expiry := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	db := setupTestDB(t)
	insertTokenRow(t, db, "kirocli:odic:token", `{"access_token":"a","refresh_token":"r","expires_at":1}`)
	regJSON, _ := json.Marshal(map[string]any{
		"client_id":                "cid",
		"client_secret":            "csec",
		"client_secret_expires_at": expiry,
	})
	insertTokenRow(t, db, "kirocli:odic:device-registration", string(regJSON))

	creds, err := ReadCredentials(db)
	if err != nil {
		t.Fatalf("ReadCredentials: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, expiry)
	if creds.ClientSecretExpiresAt != want.Unix() {
		t.Fatalf("ClientSecretExpiresAt = %d, want %d", creds.ClientSecretExpiresAt, want.Unix())
	}
}

func TestWarnClientSecretExpiry(t *testing.T) {
	cases := []struct {
		name   string
		expiry time.Time
		want   string // "" means no warning expected
	}{
		{"absent", time.Time{}, ""},
		{"far", time.Now().Add(100 * 24 * time.Hour), ""},
		{"soon", time.Now().Add(3 * 24 * time.Hour), "expiring soon"},
		{"lapsed", time.Now().Add(-20 * 24 * time.Hour), "expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			creds := &Credentials{ClientSecretExpiresAt: tc.expiry.Unix()}
			if tc.expiry.IsZero() {
				creds.ClientSecretExpiresAt = 0
			}
			warnClientSecretExpiry(creds)
			out := logs.String()
			if tc.want == "" {
				if strings.Contains(out, "device registration") {
					t.Fatalf("unexpected warning:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tc.want) || !strings.Contains(out, "kiro-cli") {
				t.Fatalf("warning missing guidance:\n%s", out)
			}
		})
	}
}

// TestCredentialLogAttrs_ClientSecretExpiry keeps the expiry visible on every
// credentials log line.
func TestCredentialLogAttrs_ClientSecretExpiry(t *testing.T) {
	creds := &Credentials{ClientSecretExpiresAt: time.Now().Add(48 * time.Hour).Unix()}
	var parts []string
	for _, a := range credentialLogAttrs(creds) {
		parts = append(parts, fmt.Sprint(a))
	}
	joined := strings.Join(parts, " ")
	if !strings.Contains(joined, "client_secret_expires_at") {
		t.Fatalf("attr missing: %s", joined)
	}
}

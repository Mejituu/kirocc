package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const tokenValidityBuffer = 5 * time.Minute

// ErrRefreshTokenExpired marks a refresh rejected because the stored refresh
// token is past its absolute lifetime. The wrapped error text tells the
// operator how to recover.
var ErrRefreshTokenExpired = errors.New("refresh token expired or invalid")

// Option configures an AuthManager.
type Option func(*AuthManager)

// WithHTTPClient sets a custom HTTP client for token refresh requests.
func WithHTTPClient(c *http.Client) Option {
	return func(m *AuthManager) { m.httpClient = c }
}

// WithAPIKey configures a Kiro API key ("ksk_…", normally from KIRO_API_KEY) as
// the credential source. Such a key is long-lived and presented directly to the
// API, so the SQLite database is never opened and no refresh ever happens —
// which is what lets kirocc run with no Kiro CLI login, in CI or a container.
// An empty key is ignored, leaving the database path in effect.
func WithAPIKey(key, region string) Option {
	return func(m *AuthManager) {
		if key == "" {
			return
		}
		if region == "" {
			region = defaultAPIKeyRegion
		}
		m.apiKey = key
		m.apiKeyRegion = region
	}
}

// defaultAPIKeyRegion is the region to target for API-key auth when none is
// configured. An API key carries no region of its own, unlike a credential read
// from the database.
const defaultAPIKeyRegion = "us-east-1"

// AuthManager manages Kiro credentials with caching and automatic refresh.
type AuthManager struct {
	dbPath           string
	db               *sql.DB // non-nil only in tests via newAuthManagerWithDB
	apiKey           string  // when set, short-circuits both the DB and refresh
	apiKeyRegion     string
	httpClient       *http.Client
	mu               sync.Mutex
	cached           *Credentials
	refreshGroup     singleflight.Group
	oidcEndpointFn   func(ssoRegion string) string
	socialEndpointFn func(region string) string
}

// UsesAPIKey reports whether credentials come from a Kiro API key rather than
// from the Kiro CLI database.
func (m *AuthManager) UsesAPIKey() bool { return m.apiKey != "" }

func newDefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// NewAuthManager creates an AuthManager that reads credentials from the given SQLite DB path.
func NewAuthManager(dbPath string, opts ...Option) *AuthManager {
	m := &AuthManager{
		dbPath:           dbPath,
		httpClient:       newDefaultHTTPClient(),
		oidcEndpointFn:   defaultOIDCEndpoint,
		socialEndpointFn: defaultSocialEndpoint,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// newAuthManagerWithDB creates an AuthManager backed by an existing *sql.DB (for testing).
func newAuthManagerWithDB(db *sql.DB) *AuthManager {
	return &AuthManager{
		db:               db,
		httpClient:       newDefaultHTTPClient(),
		oidcEndpointFn:   defaultOIDCEndpoint,
		socialEndpointFn: defaultSocialEndpoint,
	}
}

func defaultOIDCEndpoint(ssoRegion string) string {
	return fmt.Sprintf("https://oidc.%s.amazonaws.com/token", ssoRegion)
}

func defaultSocialEndpoint(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return fmt.Sprintf("https://prod.%s.auth.desktop.kiro.dev/refreshToken", region)
}

// GetToken returns valid credentials, refreshing if necessary.
// It is safe for concurrent use. Concurrent refresh requests are deduplicated via singleflight.
func (m *AuthManager) GetToken(ctx context.Context) (*Credentials, error) {
	// An API key is static: nothing to read, nothing to expire, nothing to
	// refresh. Checked before the cache so the DB and refresh paths below are
	// unreachable in this mode — a revoked key therefore surfaces as a 401 from
	// the API rather than as a refresh failure here.
	if m.apiKey != "" {
		return &Credentials{
			AccessToken: m.apiKey,
			Region:      m.apiKeyRegion,
			AuthType:    AuthTypeAPIKey,
		}, nil
	}

	m.mu.Lock()
	// Return cached credentials if still valid (copy to prevent external mutation).
	if m.cached != nil && isTokenValid(m.cached.ExpiresAt) {
		c := *m.cached
		m.mu.Unlock()
		return &c, nil
	}
	m.mu.Unlock()

	// Use singleflight to deduplicate concurrent refresh attempts.
	// Detach the caller's deadline so that one short-lived request
	// cannot cancel the shared refresh for all waiters.
	// Apply a bounded timeout to prevent indefinite goroutine retention.
	v, err, _ := m.refreshGroup.Do("refresh", func() (any, error) {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 35*time.Second)
		defer cancel()
		return m.refreshCredentials(refreshCtx)
	})
	if err != nil {
		return nil, err
	}
	// Copy to prevent callers from mutating the shared cached credentials.
	creds := *v.(*Credentials)
	return &creds, nil
}

// InvalidateCache clears the cached credentials, forcing the next GetToken call
// to re-read from DB and potentially refresh. Used when a 403 indicates the
// cached token is rejected by the upstream API.
func (m *AuthManager) InvalidateCache() {
	m.mu.Lock()
	m.cached = nil
	m.mu.Unlock()
}

// refreshCredentials re-reads from DB and refreshes if needed. Called under singleflight.
func (m *AuthManager) refreshCredentials(ctx context.Context) (*Credentials, error) {
	// Re-check cache under lock — another goroutine may have refreshed while we waited.
	m.mu.Lock()
	if m.cached != nil && isTokenValid(m.cached.ExpiresAt) {
		c := *m.cached
		m.mu.Unlock()
		return &c, nil
	}
	m.mu.Unlock()

	creds, source, err := m.readFromDB()
	if err != nil {
		return nil, err
	}

	if isTokenValid(creds.ExpiresAt) {
		slog.Info("credentials loaded", credentialLogAttrs(creds)...)
		warnClientSecretExpiry(creds)
		m.mu.Lock()
		m.cached = creds
		m.mu.Unlock()
		return creds, nil
	}

	// DB token also expired — refresh (no lock held during HTTP call).
	slog.Info("credentials expired, refreshing", "auth_type", creds.AuthType, "region", creds.Region)
	var refreshed *Credentials
	var carriedRefreshToken bool
	if creds.AuthType == "social" {
		endpoint := m.socialEndpointFn(creds.Region)
		refreshed, carriedRefreshToken, err = m.refreshSocialToken(ctx, creds, endpoint)
	} else {
		// IDC/OIDC: require device registration (ClientID + ClientSecret).
		if creds.ClientID == "" || creds.ClientSecret == "" {
			return nil, fmt.Errorf("token refresh: idc credentials missing device registration (clientId/clientSecret)")
		}
		if creds.SSORegion == "" {
			return nil, fmt.Errorf("token refresh: idc credentials missing region (check kiro-cli configuration)")
		}
		endpoint := m.oidcEndpointFn(creds.SSORegion)
		refreshed, carriedRefreshToken, err = m.refreshOIDCToken(ctx, creds, endpoint)
	}
	if err != nil {
		slog.Error("token refresh failed", "auth_type", creds.AuthType, "err", err)
		// A lapsed device registration is the usual cause of a permanent
		// refresh failure (invalid_request upstream); surface it here so the
		// bare 400 is not the only thing the operator sees.
		warnClientSecretExpiry(creds)
		return nil, fmt.Errorf("token refresh: %w", err)
	}

	// Carry over fields not returned by the refresh endpoint.
	refreshed.Region = creds.Region
	refreshed.SSORegion = creds.SSORegion
	refreshed.ClientID = creds.ClientID
	refreshed.ClientSecret = creds.ClientSecret
	refreshed.AuthType = creds.AuthType
	if refreshed.ProfileARN == "" {
		refreshed.ProfileARN = creds.ProfileARN
	}

	// A rotated refresh token would be lost on exit unless it goes back into
	// the kiro-cli database; without one the database stays untouched.
	if carriedRefreshToken {
		m.persistRefreshedToken(ctx, source, creds.RefreshToken, refreshed)
	}

	slog.Info("token refreshed", credentialLogAttrs(refreshed)...)
	warnClientSecretExpiry(refreshed)

	m.mu.Lock()
	m.cached = refreshed
	m.mu.Unlock()
	return refreshed, nil
}

// readFromDB opens (or reuses) the SQLite DB and reads credentials together
// with the source row description.
func (m *AuthManager) readFromDB() (*Credentials, *credentialSource, error) {
	if m.db != nil {
		return readCredentials(m.db)
	}
	db, err := OpenDB(m.dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()
	return readCredentials(db)
}

// persistRefreshedToken writes a rotated refresh token back into the kiro-cli
// row the credential came from. Any failure is logged and swallowed: the
// refreshed credentials are already valid in memory and must serve requests.
func (m *AuthManager) persistRefreshedToken(ctx context.Context, source *credentialSource, oldRefreshToken string, refreshed *Credentials) {
	if source == nil {
		return
	}
	var err error
	if m.db != nil {
		err = writeBackCredentials(ctx, m.db, source, oldRefreshToken, refreshed)
	} else {
		var db *sql.DB
		db, err = openRWDB(m.dbPath)
		if err == nil {
			defer func() { _ = db.Close() }()
			err = writeBackCredentials(ctx, db, source, oldRefreshToken, refreshed)
		}
	}
	if err != nil {
		slog.Warn("credential write-back failed", "key", source.Key, "err", err)
	}
}

// isTokenValid reports whether the token expires more than tokenValidityBuffer from now.
func isTokenValid(expiresAt int64) bool {
	return time.Unix(expiresAt, 0).After(time.Now().Add(tokenValidityBuffer))
}

// tokenResponse holds the common fields from a token refresh response.
type tokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	ProfileArn   string `json:"profileArn"` // social only
}

// doTokenRefresh posts a JSON body to the given endpoint and decodes the token response.
func (m *AuthManager) doTokenRefresh(ctx context.Context, endpoint string, body []byte, label string) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if isExpiredRefreshTokenBody(errBody) {
			slog.Warn("token refresh: invalid refresh token", "label", label, "status", resp.StatusCode, "body", string(errBody))
			return nil, fmt.Errorf("%w: refreshToken 已到绝对有效期，请用 kiro-cli 重新登录，或改用 KIRO_API_KEY", ErrRefreshTokenExpired)
		}
		slog.Debug("token refresh: error response body", "label", label, "status", resp.StatusCode, "body", string(errBody))
		return nil, fmt.Errorf("%s token endpoint returned %d", label, resp.StatusCode)
	}

	var result tokenResponse
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if result.AccessToken == "" {
		return nil, fmt.Errorf("%s token response: empty access token", label)
	}
	if result.ExpiresIn <= 0 {
		return nil, fmt.Errorf("%s token response: invalid expiresIn %d", label, result.ExpiresIn)
	}

	// Never log the token itself, only whether a rotated one came back.
	slog.Info(label+" refresh response carried refresh_token", "carried", result.RefreshToken != "")
	return &result, nil
}

// isExpiredRefreshTokenBody reports whether an OIDC/social error body says the
// refresh token itself is invalid or past its lifetime.
func isExpiredRefreshTokenBody(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("invalid_grant")) ||
		bytes.Contains(lower, []byte("expired_token"))
}

// refreshOIDCToken calls the AWS SSO OIDC token endpoint. The second result
// reports whether the response carried a rotated refresh token.
func (m *AuthManager) refreshOIDCToken(ctx context.Context, creds *Credentials, endpoint string) (*Credentials, bool, error) {
	body, err := json.Marshal(map[string]string{
		"grantType":    "refresh_token",
		"clientId":     creds.ClientID,
		"clientSecret": creds.ClientSecret,
		"refreshToken": creds.RefreshToken,
	})
	if err != nil {
		return nil, false, fmt.Errorf("marshal request: %w", err)
	}

	result, err := m.doTokenRefresh(ctx, endpoint, body, "oidc")
	if err != nil {
		return nil, false, err
	}

	return &Credentials{
		AccessToken:  result.AccessToken,
		RefreshToken: coalesce(result.RefreshToken, creds.RefreshToken),
		ExpiresAt:    time.Now().Unix() + result.ExpiresIn,
	}, result.RefreshToken != "", nil
}

// refreshSocialToken calls the Kiro Desktop social auth refresh endpoint. The
// second result reports whether the response carried a rotated refresh token.
func (m *AuthManager) refreshSocialToken(ctx context.Context, creds *Credentials, endpoint string) (*Credentials, bool, error) {
	body, err := json.Marshal(map[string]string{
		"refreshToken": creds.RefreshToken,
	})
	if err != nil {
		return nil, false, fmt.Errorf("marshal request: %w", err)
	}

	result, err := m.doTokenRefresh(ctx, endpoint, body, "social")
	if err != nil {
		return nil, false, err
	}

	return &Credentials{
		AccessToken:  result.AccessToken,
		RefreshToken: coalesce(result.RefreshToken, creds.RefreshToken),
		ExpiresAt:    time.Now().Unix() + result.ExpiresIn,
		ProfileARN:   result.ProfileArn,
	}, result.RefreshToken != "", nil
}

// credentialLogAttrs describes a loaded credential for the startup log: which
// profile it belongs to, where requests go and when the token lapses.
func credentialLogAttrs(creds *Credentials) []any {
	attrs := []any{"auth_type", creds.AuthType, "region", creds.Region}
	if creds.SSORegion != "" && creds.SSORegion != creds.Region {
		attrs = append(attrs, "sso_region", creds.SSORegion)
	}
	if creds.ProfileARN != "" {
		attrs = append(attrs, "profile_arn", creds.ProfileARN)
	}
	if creds.ExpiresAt > 0 {
		attrs = append(attrs, "token_expires_at", time.Unix(creds.ExpiresAt, 0).Format(time.DateTime))
	}
	if creds.ClientSecretExpiresAt > 0 {
		attrs = append(attrs, "client_secret_expires_at", time.Unix(creds.ClientSecretExpiresAt, 0).Format(time.DateTime))
	}
	return attrs
}

// clientSecretWarnDays is how close to the device registration's client-secret
// expiry kirocc starts warning. The secret's lifetime is roughly 90 days; a
// lapsed registration makes every refresh fail upstream (invalid_request /
// invalid_grant) and only a kiro-cli re-login recovers it.
const clientSecretWarnDays = 7

// warnClientSecretExpiry surfaces the device registration's client-secret
// lifetime once it is expired or nearly so. The expiry date itself rides on
// every credentials log line via credentialLogAttrs.
func warnClientSecretExpiry(creds *Credentials) {
	if creds == nil || creds.ClientSecretExpiresAt == 0 {
		return
	}
	days := time.Until(time.Unix(creds.ClientSecretExpiresAt, 0)).Hours() / 24
	if days < 0 {
		slog.Warn("device registration expired: client secret lapsed",
			"days_past", int(-days),
			"action", "re-login with kiro-cli")
		return
	}
	if days < clientSecretWarnDays {
		slog.Warn("device registration client secret expiring soon",
			"days_left", int(days),
			"action", "re-login with kiro-cli before it lapses")
	}
}

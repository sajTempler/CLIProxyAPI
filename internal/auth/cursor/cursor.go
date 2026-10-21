// Package cursor provides authentication for the Cursor agent backend (api2.cursor.sh).
//
// Three ways to obtain a session JWT are supported, all mirroring the official
// `agent` CLI (see docs/cursor/cursor-research.md, section 1):
//   - browser login: cursor.com/loginDeepControl with a PKCE challenge, then poll
//     api2.cursor.sh/auth/poll until the user approves;
//   - user API key: exchanged for a session JWT at auth/exchange_user_api_key, and
//     re-exchangeable at any time, so it is the only refreshable credential;
//   - import of the agent CLI's own login from the macOS keychain or its auth.json.
package cursor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultAPIBaseURL is the Cursor backend that serves auth and aiserver.v1 RPCs.
	DefaultAPIBaseURL = "https://api2.cursor.sh"
	// DefaultWebsiteURL hosts the browser login page.
	DefaultWebsiteURL = "https://cursor.com"
	// MaxPollAttempts matches the agent CLI's poll budget.
	MaxPollAttempts = 150
	// MaxPollDuration bounds a browser login (150 polls at <=10 s each is ~24 min).
	MaxPollDuration = 25 * time.Minute
	// RefreshLead is how long before JWT expiry a refresh (or a re-login warning) starts.
	RefreshLead = 7 * 24 * time.Hour

	// AuthKindOAuth marks a browser (loginDeepControl + auth/poll) login.
	AuthKindOAuth = "oauth"
	// AuthKindAPIKey marks a credential obtained from a user API key.
	AuthKindAPIKey = "api_key"
	// AuthKindAgentImport marks a credential imported from the agent CLI's store.
	AuthKindAgentImport = "agent_import"

	pollInitialDelay  = time.Second
	pollMaxDelay      = 10 * time.Second
	pollMaxErrors     = 3
	httpClientTimeout = 30 * time.Second
)

// ErrReloginRequired reports a credential that cannot be refreshed without user interaction.
var ErrReloginRequired = errors.New("cursor: credential cannot be refreshed; run --cursor-login (or --cursor-import-agent after `agent login`)")

// TokenPair is the JSON body returned by auth/poll and auth/exchange_user_api_key.
type TokenPair struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// LoginSession is one pending browser login.
type LoginSession struct {
	UUID      string
	Verifier  string
	Challenge string
	LoginURL  string
}

// CursorAuth performs Cursor credential acquisition calls.
type CursorAuth struct {
	httpClient *http.Client
	apiBaseURL string
	websiteURL string
	// sleep waits between polls; tests replace it to avoid wall-clock waits.
	sleep func(ctx context.Context, d time.Duration) error
}

// NewCursorAuth creates a CursorAuth honoring the configured proxy.
func NewCursorAuth(cfg *config.Config) *CursorAuth {
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
	}
	return &CursorAuth{
		httpClient: util.SetProxy(&sdkCfg, &http.Client{Timeout: httpClientTimeout}),
		apiBaseURL: DefaultAPIBaseURL,
		websiteURL: DefaultWebsiteURL,
		sleep:      sleepContext,
	}
}

// SetBaseURLs overrides the backend and website URLs (tests and self-hosted mirrors).
func (a *CursorAuth) SetBaseURLs(apiBaseURL, websiteURL string) {
	if v := strings.TrimRight(strings.TrimSpace(apiBaseURL), "/"); v != "" {
		a.apiBaseURL = v
	}
	if v := strings.TrimRight(strings.TrimSpace(websiteURL), "/"); v != "" {
		a.websiteURL = v
	}
}

// SetHTTPClient replaces the HTTP client (tests).
func (a *CursorAuth) SetHTTPClient(client *http.Client) {
	if client != nil {
		a.httpClient = client
	}
}

// SetSleep replaces the poll wait function (tests).
func (a *CursorAuth) SetSleep(sleep func(ctx context.Context, d time.Duration) error) {
	if sleep != nil {
		a.sleep = sleep
	}
}

// StartLogin creates a PKCE challenge and the browser login URL.
func (a *CursorAuth) StartLogin() (*LoginSession, error) {
	raw := make([]byte, 32)
	if _, errRead := rand.Read(raw); errRead != nil {
		return nil, fmt.Errorf("cursor login: generate verifier: %w", errRead)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	id := uuid.NewString()

	loginURL, errParse := url.Parse(a.websiteURL + "/loginDeepControl")
	if errParse != nil {
		return nil, fmt.Errorf("cursor login: build login url: %w", errParse)
	}
	q := loginURL.Query()
	q.Set("challenge", challenge)
	q.Set("uuid", id)
	q.Set("mode", "login")
	q.Set("redirectTarget", "cli")
	loginURL.RawQuery = q.Encode()

	return &LoginSession{UUID: id, Verifier: verifier, Challenge: challenge, LoginURL: loginURL.String()}, nil
}

// WaitForTokens polls auth/poll until the browser login completes.
// 404 means "not yet"; three consecutive other failures abort, like the agent CLI.
func (a *CursorAuth) WaitForTokens(ctx context.Context, session *LoginSession) (*TokenPair, error) {
	if session == nil || session.UUID == "" || session.Verifier == "" {
		return nil, fmt.Errorf("cursor login: missing login session")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, MaxPollDuration)
	defer cancel()

	pollURL := fmt.Sprintf("%s/auth/poll?uuid=%s&verifier=%s", a.apiBaseURL, url.QueryEscape(session.UUID), url.QueryEscape(session.Verifier))
	consecutiveErrors := 0
	delay := pollInitialDelay
	for attempt := 0; attempt < MaxPollAttempts; attempt++ {
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, fmt.Errorf("cursor login: canceled or timed out: %w", errCtx)
		}
		status, body, errDo := a.do(ctx, http.MethodGet, pollURL, "", nil)
		switch {
		case errDo != nil:
			consecutiveErrors++
			log.Debugf("cursor login: poll request failed: %v", errDo)
		case status == http.StatusNotFound:
			consecutiveErrors = 0
		case status == http.StatusOK:
			tokens, errParse := parseTokenPair(body)
			if errParse != nil {
				return nil, fmt.Errorf("cursor login: %w", errParse)
			}
			return tokens, nil
		case status == http.StatusForbidden:
			return nil, fmt.Errorf("cursor login: rejected by server (HTTP 403): %s", errorCode(body))
		default:
			consecutiveErrors++
			log.Debugf("cursor login: poll returned HTTP %d", status)
		}
		if consecutiveErrors >= pollMaxErrors {
			return nil, fmt.Errorf("cursor login: polling failed %d times in a row", consecutiveErrors)
		}
		if errSleep := a.sleep(ctx, delay); errSleep != nil {
			return nil, fmt.Errorf("cursor login: canceled or timed out: %w", errSleep)
		}
		delay = time.Duration(float64(delay) * 1.2)
		if delay > pollMaxDelay {
			delay = pollMaxDelay
		}
	}
	return nil, fmt.Errorf("cursor login: gave up after %d polls", MaxPollAttempts)
}

// ExchangeAPIKey trades a user API key for a session JWT pair.
func (a *CursorAuth) ExchangeAPIKey(ctx context.Context, apiKey string) (*TokenPair, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("cursor api key exchange: api key is empty")
	}
	status, body, errDo := a.do(ctx, http.MethodPost, a.apiBaseURL+"/auth/exchange_user_api_key", apiKey, []byte("{}"))
	if errDo != nil {
		return nil, fmt.Errorf("cursor api key exchange: %w", errDo)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("cursor api key exchange: HTTP %d %s", status, errorCode(body))
	}
	tokens, errParse := parseTokenPair(body)
	if errParse != nil {
		return nil, fmt.Errorf("cursor api key exchange: %w", errParse)
	}
	return tokens, nil
}

// FetchEmail asks DashboardService/GetMe for the account email. Best effort: callers
// treat an error as "email unknown".
func (a *CursorAuth) FetchEmail(ctx context.Context, accessToken string) (string, error) {
	status, body, errDo := a.do(ctx, http.MethodPost, a.apiBaseURL+"/aiserver.v1.DashboardService/GetMe", accessToken, []byte("{}"))
	if errDo != nil {
		return "", fmt.Errorf("cursor GetMe: %w", errDo)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("cursor GetMe: HTTP %d", status)
	}
	var me struct {
		Email string `json:"email"`
	}
	if errJSON := json.Unmarshal(body, &me); errJSON != nil {
		return "", fmt.Errorf("cursor GetMe: decode: %w", errJSON)
	}
	return strings.TrimSpace(me.Email), nil
}

func (a *CursorAuth) do(ctx context.Context, method, target, bearer string, body []byte) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, errReq := http.NewRequestWithContext(ctx, method, target, reader)
	if errReq != nil {
		return 0, nil, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-cursor-client-type", "cli")
	if strings.Contains(target, "/aiserver.v1.") {
		req.Header.Set("Connect-Protocol-Version", "1")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, errDo := a.httpClient.Do(req)
	if errDo != nil {
		return 0, nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("cursor auth: close response body: %v", errClose)
		}
	}()
	data, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return resp.StatusCode, nil, errRead
	}
	return resp.StatusCode, data, nil
}

func parseTokenPair(body []byte) (*TokenPair, error) {
	var tokens TokenPair
	if errJSON := json.Unmarshal(body, &tokens); errJSON != nil {
		return nil, fmt.Errorf("decode token response: %w", errJSON)
	}
	tokens.AccessToken = strings.TrimSpace(tokens.AccessToken)
	tokens.RefreshToken = strings.TrimSpace(tokens.RefreshToken)
	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("token response has no accessToken")
	}
	return &tokens, nil
}

// errorCode extracts a short error code from a JSON error body without echoing arbitrary content.
func errorCode(body []byte) string {
	var payload struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(body, &payload) == nil {
		if payload.Error != "" {
			return payload.Error
		}
		return payload.Code
	}
	return ""
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// JWTClaims holds the session JWT claims the proxy uses.
type JWTClaims struct {
	Subject   string
	ExpiresAt time.Time
}

// ParseJWTClaims decodes (without verifying) the payload of a Cursor session JWT.
func ParseJWTClaims(token string) (JWTClaims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return JWTClaims{}, fmt.Errorf("cursor jwt: not a JWT")
	}
	raw, errDecode := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if errDecode != nil {
		return JWTClaims{}, fmt.Errorf("cursor jwt: decode payload: %w", errDecode)
	}
	var payload struct {
		Sub string  `json:"sub"`
		Exp float64 `json:"exp"`
	}
	if errJSON := json.Unmarshal(raw, &payload); errJSON != nil {
		return JWTClaims{}, fmt.Errorf("cursor jwt: parse payload: %w", errJSON)
	}
	claims := JWTClaims{Subject: payload.Sub}
	if payload.Exp > 0 {
		claims.ExpiresAt = time.Unix(int64(payload.Exp), 0).UTC()
	}
	return claims, nil
}

// RecordInput describes a freshly obtained credential.
type RecordInput struct {
	Tokens   *TokenPair
	APIKey   string
	Email    string
	AuthKind string
	Now      time.Time
}

// BuildAuthRecord creates the `type: "cursor"` auth record persisted by the token store.
func BuildAuthRecord(in RecordInput) (*coreauth.Auth, error) {
	if in.Tokens == nil || strings.TrimSpace(in.Tokens.AccessToken) == "" {
		return nil, fmt.Errorf("cursor: access token is required")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	kind := in.AuthKind
	if kind == "" {
		kind = AuthKindOAuth
	}
	claims, errClaims := ParseJWTClaims(in.Tokens.AccessToken)
	if errClaims != nil {
		log.Warnf("cursor: could not read access token expiry: %v", errClaims)
	}
	email := strings.TrimSpace(in.Email)
	fileName := CredentialFileName(email, claims.Subject, in.Tokens.AccessToken)

	metadata := map[string]any{
		"type":          "cursor",
		"auth_kind":     kind,
		"access_token":  in.Tokens.AccessToken,
		"refresh_token": in.Tokens.RefreshToken,
		"last_refresh":  now.UTC().Format(time.RFC3339),
	}
	attrs := map[string]string{"auth_kind": kind}
	if !claims.ExpiresAt.IsZero() {
		metadata["expired"] = claims.ExpiresAt.Format(time.RFC3339)
	}
	if apiKey := strings.TrimSpace(in.APIKey); apiKey != "" {
		metadata["api_key"] = apiKey
	}
	if email != "" {
		metadata["email"] = email
		attrs["email"] = email
	}
	label := "Cursor"
	if email != "" {
		label = "Cursor (" + email + ")"
	}
	return &coreauth.Auth{
		ID:         fileName,
		Provider:   "cursor",
		FileName:   fileName,
		Label:      label,
		Status:     coreauth.StatusActive,
		Metadata:   metadata,
		Attributes: attrs,
	}, nil
}

// CredentialFileName returns cursor-<email>-<hash>.json, or cursor-<hash>.json when
// no email is known. The hash keys on the JWT subject so re-logins of one account
// overwrite the same file.
func CredentialFileName(email, subject, accessToken string) string {
	identity := strings.TrimSpace(subject)
	if identity == "" {
		identity = strings.TrimSpace(email)
	}
	if identity == "" {
		identity = accessToken
	}
	sum := sha256.Sum256([]byte(identity))
	hash := hex.EncodeToString(sum[:6])
	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' || r == '@' {
			return r
		}
		return '_'
	}, strings.TrimSpace(email))
	if len(clean) > 120 {
		clean = clean[:120]
	}
	if clean == "" {
		return fmt.Sprintf("cursor-%s.json", hash)
	}
	return fmt.Sprintf("cursor-%s-%s.json", clean, hash)
}

// Refresh renews a cursor auth record. Only credentials carrying an api_key can be
// refreshed (by re-exchanging it); others return ErrReloginRequired after logging a
// rate-limited re-login warning. The executor's Refresh (Phase 4) delegates here.
func (a *CursorAuth) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("cursor refresh: auth is nil")
	}
	apiKey, _ := auth.Metadata["api_key"].(string)
	if strings.TrimSpace(apiKey) == "" {
		WarnReloginNearExpiry(auth, time.Now())
		return nil, ErrReloginRequired
	}
	tokens, errExchange := a.ExchangeAPIKey(ctx, apiKey)
	if errExchange != nil {
		return nil, errExchange
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = map[string]any{}
	}
	updated.Metadata["type"] = "cursor"
	updated.Metadata["access_token"] = tokens.AccessToken
	updated.Metadata["refresh_token"] = tokens.RefreshToken
	updated.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if claims, errClaims := ParseJWTClaims(tokens.AccessToken); errClaims == nil && !claims.ExpiresAt.IsZero() {
		updated.Metadata["expired"] = claims.ExpiresAt.Format(time.RFC3339)
	}
	return updated, nil
}

var (
	reloginWarnMu   sync.Mutex
	reloginWarnedAt = map[string]time.Time{}
)

const reloginWarnInterval = 6 * time.Hour

// WarnReloginNearExpiry logs a re-login warning for a non-refreshable credential that
// expires within RefreshLead. It logs at most once per reloginWarnInterval per auth.
// It reports whether a warning was emitted.
func WarnReloginNearExpiry(auth *coreauth.Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	expiry, ok := auth.ExpirationTime()
	if !ok || expiry.IsZero() || expiry.Sub(now) > RefreshLead {
		return false
	}
	reloginWarnMu.Lock()
	last, seen := reloginWarnedAt[auth.ID]
	if seen && now.Sub(last) < reloginWarnInterval {
		reloginWarnMu.Unlock()
		return false
	}
	reloginWarnedAt[auth.ID] = now
	reloginWarnMu.Unlock()

	if expiry.After(now) {
		log.Warnf("cursor credential %s expires %s (in %s) and cannot be refreshed; re-run --cursor-login or --cursor-import-agent", auth.ID, expiry.Format(time.RFC3339), expiry.Sub(now).Round(time.Hour))
	} else {
		log.Warnf("cursor credential %s expired %s; re-run --cursor-login or --cursor-import-agent", auth.ID, expiry.Format(time.RFC3339))
	}
	return true
}

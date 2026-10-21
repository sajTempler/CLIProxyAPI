package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func fakeJWT(t *testing.T, sub string, exp time.Time) string {
	t.Helper()
	payload, errJSON := json.Marshal(map[string]any{"sub": sub, "exp": exp.Unix(), "type": "session"})
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString(payload) + ".sig"
}

// fakeServer emulates api2.cursor.sh auth endpoints.
type fakeServer struct {
	mu          sync.Mutex
	pollHits    int
	pendingFor  int
	pollStatus  int
	exchangeKey string
	token       string
	email       string
	lastBearer  string
}

func (f *fakeServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/poll", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pollHits++
		if r.URL.Query().Get("uuid") == "" || r.URL.Query().Get("verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.pollStatus != 0 {
			w.WriteHeader(f.pollStatus)
			_, _ = w.Write([]byte(`{"error":"forbidden_login"}`))
			return
		}
		if f.pollHits <= f.pendingFor {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": f.token, "refreshToken": f.token})
	})
	mux.HandleFunc("/auth/exchange_user_api_key", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+f.exchangeKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_api_key"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": f.token, "refreshToken": f.token})
	})
	mux.HandleFunc("/aiserver.v1.DashboardService/GetMe", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastBearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Header.Get("Connect-Protocol-Version") != "1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"authId": "auth0|x", "userId": 1, "email": f.email})
	})
	return mux
}

func newTestAuth(t *testing.T, f *fakeServer) *CursorAuth {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	a := NewCursorAuth(nil)
	a.SetBaseURLs(srv.URL, "https://cursor.example")
	a.SetSleep(func(ctx context.Context, _ time.Duration) error { return ctx.Err() })
	return a
}

func TestStartLoginBuildsPKCEURL(t *testing.T) {
	a := NewCursorAuth(nil)
	session, errStart := a.StartLogin()
	if errStart != nil {
		t.Fatal(errStart)
	}
	if !strings.HasPrefix(session.LoginURL, DefaultWebsiteURL+"/loginDeepControl?") {
		t.Fatalf("login url = %q", session.LoginURL)
	}
	for _, want := range []string{"challenge=" + session.Challenge, "uuid=" + session.UUID, "mode=login", "redirectTarget=cli"} {
		if !strings.Contains(session.LoginURL, want) {
			t.Errorf("login url missing %q", want)
		}
	}
	if strings.Contains(session.LoginURL, session.Verifier) {
		t.Error("login url leaks the verifier")
	}
}

func TestWaitForTokensPollsUntilApproved(t *testing.T) {
	token := fakeJWT(t, "auth0|user", time.Now().Add(60*24*time.Hour))
	f := &fakeServer{pendingFor: 3, token: token}
	a := newTestAuth(t, f)
	session, _ := a.StartLogin()
	tokens, errWait := a.WaitForTokens(context.Background(), session)
	if errWait != nil {
		t.Fatal(errWait)
	}
	if tokens.AccessToken != token || f.pollHits != 4 {
		t.Fatalf("got token match=%t after %d polls", tokens.AccessToken == token, f.pollHits)
	}
}

func TestWaitForTokensStopsOnForbidden(t *testing.T) {
	f := &fakeServer{pollStatus: http.StatusForbidden}
	a := newTestAuth(t, f)
	session, _ := a.StartLogin()
	_, errWait := a.WaitForTokens(context.Background(), session)
	if errWait == nil || !strings.Contains(errWait.Error(), "forbidden_login") || f.pollHits != 1 {
		t.Fatalf("err=%v hits=%d", errWait, f.pollHits)
	}
}

func TestWaitForTokensAbortsAfterRepeatedErrors(t *testing.T) {
	f := &fakeServer{pollStatus: http.StatusInternalServerError}
	a := newTestAuth(t, f)
	session, _ := a.StartLogin()
	if _, errWait := a.WaitForTokens(context.Background(), session); errWait == nil || f.pollHits != pollMaxErrors {
		t.Fatalf("err=%v hits=%d", errWait, f.pollHits)
	}
}

func TestExchangeAPIKey(t *testing.T) {
	token := fakeJWT(t, "auth0|user", time.Now().Add(time.Hour))
	f := &fakeServer{exchangeKey: "key_good", token: token}
	a := newTestAuth(t, f)
	tokens, errEx := a.ExchangeAPIKey(context.Background(), "key_good")
	if errEx != nil || tokens.AccessToken != token {
		t.Fatalf("exchange failed: %v", errEx)
	}
	_, errBad := a.ExchangeAPIKey(context.Background(), "key_bad")
	if errBad == nil || !strings.Contains(errBad.Error(), "401") || strings.Contains(errBad.Error(), "key_bad") {
		t.Fatalf("bad key error = %v", errBad)
	}
}

func TestFetchEmail(t *testing.T) {
	f := &fakeServer{email: "dev@example.com"}
	a := newTestAuth(t, f)
	email, errEmail := a.FetchEmail(context.Background(), "jwt-value")
	if errEmail != nil || email != "dev@example.com" || f.lastBearer != "jwt-value" {
		t.Fatalf("email=%q err=%v", email, errEmail)
	}
}

func TestBuildAuthRecord(t *testing.T) {
	exp := time.Date(2026, 12, 20, 10, 0, 0, 0, time.UTC)
	token := fakeJWT(t, "auth0|user", exp)
	record, errBuild := BuildAuthRecord(RecordInput{
		Tokens:   &TokenPair{AccessToken: token, RefreshToken: token},
		APIKey:   "key_x",
		Email:    "dev@example.com",
		AuthKind: AuthKindAPIKey,
	})
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	if record.Provider != "cursor" || record.Metadata["type"] != "cursor" {
		t.Fatalf("provider=%q type=%v", record.Provider, record.Metadata["type"])
	}
	if !strings.HasPrefix(record.FileName, "cursor-dev@example.com-") || !strings.HasSuffix(record.FileName, ".json") {
		t.Fatalf("file name = %q", record.FileName)
	}
	if record.Metadata["expired"] != exp.Format(time.RFC3339) || record.Metadata["api_key"] != "key_x" || record.Metadata["email"] != "dev@example.com" {
		t.Fatalf("metadata = %v", record.Metadata)
	}
	if got, ok := record.ExpirationTime(); !ok || !got.Equal(exp) {
		t.Fatalf("ExpirationTime() = %v, %t", got, ok)
	}
	// Same subject, different email spelling still maps to one hash suffix.
	again := CredentialFileName("", "auth0|user", "other")
	if !strings.HasSuffix(record.FileName, strings.TrimPrefix(again, "cursor-")) {
		t.Fatalf("hash differs: %q vs %q", record.FileName, again)
	}
}

func TestBuildAuthRecordRequiresToken(t *testing.T) {
	if _, errBuild := BuildAuthRecord(RecordInput{Tokens: &TokenPair{}}); errBuild == nil {
		t.Fatal("expected error for empty access token")
	}
}

func TestRefreshWithAPIKey(t *testing.T) {
	newToken := fakeJWT(t, "auth0|user", time.Now().Add(60*24*time.Hour))
	f := &fakeServer{exchangeKey: "key_good", token: newToken}
	a := newTestAuth(t, f)
	auth := &coreauth.Auth{ID: "cursor-a.json", Provider: "cursor", Metadata: map[string]any{"api_key": "key_good", "access_token": "old"}}
	updated, errRefresh := a.Refresh(context.Background(), auth)
	if errRefresh != nil {
		t.Fatal(errRefresh)
	}
	if updated.Metadata["access_token"] != newToken || auth.Metadata["access_token"] != "old" {
		t.Fatal("refresh did not update a clone")
	}
	if _, ok := updated.Metadata["expired"].(string); !ok {
		t.Fatal("refresh did not set expired")
	}
}

func TestRefreshWithoutAPIKeyNeedsRelogin(t *testing.T) {
	a := NewCursorAuth(nil)
	auth := &coreauth.Auth{ID: "cursor-b.json", Provider: "cursor", Metadata: map[string]any{"access_token": "x"}}
	if _, errRefresh := a.Refresh(context.Background(), auth); !errors.Is(errRefresh, ErrReloginRequired) {
		t.Fatalf("err = %v", errRefresh)
	}
}

func TestWarnReloginNearExpiryRateLimited(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{ID: "cursor-warn-test.json", Metadata: map[string]any{"expired": now.Add(48 * time.Hour).Format(time.RFC3339)}}
	if !WarnReloginNearExpiry(auth, now) {
		t.Fatal("expected first warning")
	}
	if WarnReloginNearExpiry(auth, now.Add(time.Hour)) {
		t.Fatal("expected rate-limited second warning")
	}
	if !WarnReloginNearExpiry(auth, now.Add(reloginWarnInterval+time.Minute)) {
		t.Fatal("expected warning after interval")
	}
	far := &coreauth.Auth{ID: "cursor-far.json", Metadata: map[string]any{"expired": now.Add(30 * 24 * time.Hour).Format(time.RFC3339)}}
	if WarnReloginNearExpiry(far, now) {
		t.Fatal("no warning expected outside the refresh lead")
	}
}

func fakeKeychain(items map[string]string) KeychainReader {
	return func(_ context.Context, service, account string) (string, error) {
		if account != KeychainAccount {
			return "", nil
		}
		return items[service], nil
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	return path
}

func TestImportAgentLoginFromKeychain(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, dir, "cli-config.json", `{"authInfo":{"email":"dev@example.com","userId":1}}`)
	creds, errImport := ImportAgentLogin(context.Background(), AgentImportOptions{
		Keychain:   fakeKeychain(map[string]string{KeychainServiceAccessToken: "jwt-a", KeychainServiceRefreshToken: "jwt-r"}),
		AuthFile:   filepath.Join(dir, "missing.json"),
		ConfigFile: cfgPath,
	})
	if errImport != nil {
		t.Fatal(errImport)
	}
	if creds.AccessToken != "jwt-a" || creds.RefreshToken != "jwt-r" || creds.APIKey != "" || creds.Source != "keychain" || creds.Email != "dev@example.com" {
		t.Fatalf("creds source=%q email=%q", creds.Source, creds.Email)
	}
}

func TestImportAgentLoginFallsBackToAuthFile(t *testing.T) {
	dir := t.TempDir()
	authPath := writeFile(t, dir, "auth.json", `{"accessToken":"jwt-f","refreshToken":"jwt-f","apiKey":"key_f"}`)
	creds, errImport := ImportAgentLogin(context.Background(), AgentImportOptions{
		Keychain:   fakeKeychain(nil),
		AuthFile:   authPath,
		ConfigFile: filepath.Join(dir, "none.json"),
	})
	if errImport != nil {
		t.Fatal(errImport)
	}
	if creds.AccessToken != "jwt-f" || creds.APIKey != "key_f" || creds.Source != authPath || creds.Email != "" {
		t.Fatalf("source=%q", creds.Source)
	}
}

func TestImportAgentLoginNotFound(t *testing.T) {
	dir := t.TempDir()
	_, errImport := ImportAgentLogin(context.Background(), AgentImportOptions{
		Keychain: fakeKeychain(nil),
		AuthFile: filepath.Join(dir, "missing.json"),
	})
	if !errors.Is(errImport, ErrAgentLoginNotFound) {
		t.Fatalf("err = %v", errImport)
	}
}

func TestImportAgentRecordExchangesAPIKeyOnlyLogin(t *testing.T) {
	token := fakeJWT(t, "auth0|user", time.Now().Add(time.Hour))
	f := &fakeServer{exchangeKey: "key_only", token: token}
	a := newTestAuth(t, f)
	creds, tokens, errImport := a.ImportAgentRecord(context.Background(), AgentImportOptions{
		Keychain: fakeKeychain(map[string]string{KeychainServiceAPIKey: "key_only"}),
		AuthFile: filepath.Join(t.TempDir(), "missing.json"),
	})
	if errImport != nil || tokens.AccessToken != token || creds.Source != "keychain" {
		t.Fatalf("keychain api-key-only: err=%v", errImport)
	}

	authPath := writeFile(t, t.TempDir(), "auth.json", `{"apiKey":"key_only"}`)
	creds, tokens, errImport = a.ImportAgentRecord(context.Background(), AgentImportOptions{Keychain: fakeKeychain(nil), AuthFile: authPath})
	if errImport != nil {
		t.Fatal(errImport)
	}
	if tokens.AccessToken != token || creds.APIKey != "key_only" {
		t.Fatal("api key was not exchanged")
	}
}

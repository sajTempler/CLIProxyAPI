package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCursorOAuthPollFlow(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"sub": "auth0|cursor-mgmt", "exp": time.Now().Add(60 * 24 * time.Hour).Unix()})
	token := "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"

	var polls atomic.Int32
	var gotVerifier atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/poll", func(w http.ResponseWriter, r *http.Request) {
		gotVerifier.Store(r.URL.Query().Get("verifier"))
		if polls.Add(1) < 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": token, "refreshToken": token})
	})
	mux.HandleFunc("/aiserver.v1.DashboardService/GetMe", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"email": "dev@example.com"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	original := newCursorAuth
	newCursorAuth = func(cfg *config.Config) *cursorauth.CursorAuth {
		a := cursorauth.NewCursorAuth(cfg)
		a.SetBaseURLs(srv.URL, "https://cursor.example")
		a.SetSleep(func(ctx context.Context, _ time.Duration) error { return ctx.Err() })
		return a
	}
	t.Cleanup(func() { newCursorAuth = original })

	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	router := gin.New()
	router.GET("/v8/oauth/auth-url", h.StartOAuthV8)
	router.GET("/get-auth-status", h.GetAuthStatus)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v8/oauth/auth-url?provider=cursor", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var start struct {
		Status, State, URL, Flow string
	}
	if errDecode := json.Unmarshal(w.Body.Bytes(), &start); errDecode != nil {
		t.Fatal(errDecode)
	}
	t.Cleanup(func() { CancelOAuthSession(start.State) })
	loginURL, errParse := url.Parse(start.URL)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if start.Status != "ok" || start.Flow != "device" || !strings.HasPrefix(start.State, "cursor-") || loginURL.Path != "/loginDeepControl" {
		t.Fatalf("start response: %s", w.Body.String())
	}

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		w = httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/get-auth-status?state="+start.State, nil))
		var status map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &status)
		if status["status"] == "ok" {
			break
		}
		if status["status"] == "error" {
			t.Fatalf("login failed: %v", status)
		}
		select {
		case <-deadline.C:
			t.Fatal("login did not complete")
		case <-ticker.C:
		}
	}

	if v, _ := gotVerifier.Load().(string); v == "" || strings.Contains(start.URL, v) {
		t.Fatal("verifier missing from poll or exposed in the login URL")
	}
	matches, _ := filepath.Glob(filepath.Join(authDir, "cursor-dev@example.com-*.json"))
	if len(matches) != 1 {
		t.Fatalf("credential files = %v", matches)
	}
	data, errRead := os.ReadFile(matches[0])
	if errRead != nil {
		t.Fatal(errRead)
	}
	var record map[string]any
	if errDecode := json.Unmarshal(data, &record); errDecode != nil {
		t.Fatal(errDecode)
	}
	if record["type"] != "cursor" || record["access_token"] != token || record["auth_kind"] != "oauth" || record["expired"] == nil {
		t.Fatalf("unexpected credential keys: %v", len(record))
	}
}

func TestNormalizeOAuthProviderCursor(t *testing.T) {
	if got, errNormalize := NormalizeOAuthProvider(" Cursor "); errNormalize != nil || got != "cursor" {
		t.Fatalf("NormalizeOAuthProvider = %q, %v", got, errNormalize)
	}
}

package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func cursorTestJWT(exp time.Time) string {
	payload, _ := json.Marshal(map[string]any{"sub": "auth0|cursor-test", "exp": exp.Unix()})
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + ".sig"
}

func newCursorTestAuthenticator(t *testing.T, handler http.Handler) *CursorAuthenticator {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &CursorAuthenticator{newAuth: func(cfg *config.Config) *cursorauth.CursorAuth {
		a := cursorauth.NewCursorAuth(cfg)
		a.SetBaseURLs(srv.URL, "https://cursor.example")
		a.SetSleep(func(ctx context.Context, _ time.Duration) error { return ctx.Err() })
		return a
	}}
}

func TestCursorAuthenticatorProviderAndLead(t *testing.T) {
	a := NewCursorAuthenticator()
	if a.Provider() != "cursor" {
		t.Fatalf("Provider() = %q", a.Provider())
	}
	if lead := a.RefreshLead(); lead == nil || *lead != cursorauth.RefreshLead {
		t.Fatalf("RefreshLead() = %v", lead)
	}
}

func TestCursorBrowserLogin(t *testing.T) {
	token := cursorTestJWT(time.Now().Add(60 * 24 * time.Hour))
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/poll", func(w http.ResponseWriter, _ *http.Request) {
		if polls.Add(1) < 3 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": token, "refreshToken": token})
	})
	mux.HandleFunc("/aiserver.v1.DashboardService/GetMe", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"email": "dev@example.com"})
	})
	a := newCursorTestAuthenticator(t, mux)
	record, errLogin := a.Login(context.Background(), &config.Config{}, &LoginOptions{NoBrowser: true})
	if errLogin != nil {
		t.Fatal(errLogin)
	}
	if record.Provider != "cursor" || record.Metadata["auth_kind"] != cursorauth.AuthKindOAuth || record.Metadata["email"] != "dev@example.com" {
		t.Fatalf("record = %+v", record.Metadata["auth_kind"])
	}
	if _, hasKey := record.Metadata["api_key"]; hasKey {
		t.Fatal("browser login must not store an api_key")
	}
	if !strings.HasPrefix(record.FileName, "cursor-dev@example.com-") {
		t.Fatalf("file name = %q", record.FileName)
	}
}

func TestCursorAPIKeyLogin(t *testing.T) {
	token := cursorTestJWT(time.Now().Add(60 * 24 * time.Hour))
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/exchange_user_api_key", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": token, "refreshToken": token})
	})
	mux.HandleFunc("/aiserver.v1.DashboardService/GetMe", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	a := newCursorTestAuthenticator(t, mux)
	record, errLogin := a.Login(context.Background(), &config.Config{}, &LoginOptions{Metadata: map[string]string{CursorMetadataAPIKey: "key_test"}})
	if errLogin != nil {
		t.Fatal(errLogin)
	}
	if record.Metadata["api_key"] != "key_test" || record.Metadata["auth_kind"] != cursorauth.AuthKindAPIKey {
		t.Fatal("api key login did not keep the key")
	}
	if _, hasEmail := record.Metadata["email"]; hasEmail {
		t.Fatal("email must be absent when GetMe fails")
	}
	if !strings.HasPrefix(record.FileName, "cursor-") || strings.Contains(record.FileName, "@") {
		t.Fatalf("file name = %q", record.FileName)
	}
}

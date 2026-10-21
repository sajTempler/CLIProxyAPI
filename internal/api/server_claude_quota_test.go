package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestManagementV8ClaudeQuotaRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\nremote-management: {secret-key: test-password}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthDir = t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetClaudeQuotaTracker(coreauth.NewClaudeQuotaTracker())
	manager.SetCursorQuotaTracker(coreauth.NewCursorQuotaTracker())
	h := management.NewHandler(cfg, path, manager)
	h.SetLocalPassword("test-password")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()

	for _, tc := range []struct {
		method, path string
		auth         bool
		want         int
	}{
		{http.MethodGet, "/v8/management/quota/claude", true, http.StatusOK},
		{http.MethodGet, "/v8/management/quota/claude", false, http.StatusUnauthorized},
		{http.MethodPost, "/v8/management/quota/claude/refresh", true, http.StatusServiceUnavailable},
		{http.MethodGet, "/v0/management/quota/claude", true, http.StatusNotFound},
		{http.MethodGet, "/v8/management/quota/cursor", true, http.StatusOK},
		{http.MethodGet, "/v8/management/quota/cursor", false, http.StatusUnauthorized},
		{http.MethodPost, "/v8/management/quota/cursor/refresh", true, http.StatusServiceUnavailable},
		{http.MethodGet, "/v0/management/quota/cursor", true, http.StatusNotFound},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if tc.auth {
			req.Header.Set("Authorization", "Bearer test-password")
		}
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s %s: status=%d want=%d body=%s", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
}

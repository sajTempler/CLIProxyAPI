package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeAuth(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAuthExpiry(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		expired string
		want    int
		reason  string
	}{
		{"far", "2026-12-19T13:03:29Z", ExitOK, ""},
		{"within a week", "2026-10-24T13:00:00Z", ExitAuthExpired, "Cursor login expires in 3 days"},
		{"expired", "2026-10-20T00:00:00Z", ExitAuthExpired, "Cursor login expired on 2026-10-20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAuth(t, dir, "cursor-a.json", `{"type":"cursor","access_token":"SECRET-TOKEN","expired":"`+tc.expired+`"}`)
			res := checkAuthExpiry(dir, now)
			if res.Code != tc.want || !strings.Contains(res.Reason, tc.reason) {
				t.Fatalf("result = %v, want exit %d with %q", res, tc.want, tc.reason)
			}
			if strings.Contains(res.String(), "SECRET") {
				t.Fatal("result leaks the token")
			}
		})
	}
}

func TestCheckAuthExpiryIgnoresDisabledOtherAndMissing(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	if res := checkAuthExpiry(dir, now); res.Code != ExitOK {
		t.Fatalf("no files: %v", res)
	}
	writeAuth(t, dir, "cursor-off.json", `{"type":"cursor","disabled":true,"expired":"2026-10-22T00:00:00Z"}`)
	writeAuth(t, dir, "claude-x.json", `{"type":"claude","expired":"2026-10-22T00:00:00Z"}`)
	writeAuth(t, dir, "cursor-bad.json", `{"type":"cursor","expired":"soon"}`)
	if res := checkAuthExpiry(dir, now); res.Code != ExitOK {
		t.Fatalf("disabled/other/unparsable files must be ignored: %v", res)
	}
}

func TestRunSendsOneSessionIDAndChecksExpiry(t *testing.T) {
	var sessions []string
	p := &fakeProxy{models: modelsWithCursor}
	p.turns = []func(http.ResponseWriter, []byte){stream(toolUseStream("toolu_1")), stream(textStream("PURPLE-42", "end_turn"))}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			sessions = append(sessions, r.Header.Get("X-Claude-Code-Session-Id"))
		}
		p.ServeHTTP(w, r)
	}))
	defer srv.Close()
	dir := t.TempDir()
	soon := time.Now().Add(50 * time.Hour).UTC().Format(time.RFC3339)
	writeAuth(t, dir, "cursor-a.json", `{"type":"cursor","expired":"`+soon+`"}`)
	env := func(k string) string {
		if k == "CPA_API_KEY" {
			return "k"
		}
		return ""
	}
	res := run([]string{"--base-url", srv.URL, "--auth-dir", dir}, env)
	if res.Code != ExitAuthExpired || !strings.Contains(res.Reason, "Cursor login expires in 2 days") || !strings.Contains(res.Reason, "round-trip ok") {
		t.Fatalf("result = %v", res)
	}
	if len(sessions) != 2 || sessions[0] == "" || sessions[0] != sessions[1] {
		t.Fatalf("session ids = %q, want one non-empty id on both turns", sessions)
	}
}

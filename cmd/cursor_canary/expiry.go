package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// authExpiryWarn is how early the canary reports an expiring Cursor login.
// Only API-key logins refresh themselves; the others need a re-login.
const authExpiryWarn = 7 * 24 * time.Hour

// cursorAuthExpiry is the only part of a cursor auth file the canary reads.
// Decoding into it leaves every token field untouched.
type cursorAuthExpiry struct {
	Type     string `json:"type"`
	Expired  string `json:"expired"`
	Disabled bool   `json:"disabled"`
}

// checkAuthExpiry returns ExitAuthExpired when an enabled cursor auth file in
// dir expires within authExpiryWarn (or has expired), else ok.
func checkAuthExpiry(dir string, now time.Time) Result {
	paths, _ := filepath.Glob(filepath.Join(dir, "cursor-*.json"))
	var soonest time.Time
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var f cursorAuthExpiry
		if json.Unmarshal(raw, &f) != nil || f.Type != "cursor" || f.Disabled {
			continue
		}
		exp, err := time.Parse(time.RFC3339, strings.TrimSpace(f.Expired))
		if err != nil {
			continue
		}
		if soonest.IsZero() || exp.Before(soonest) {
			soonest = exp
		}
	}
	if soonest.IsZero() {
		return ok("")
	}
	left := soonest.Sub(now)
	switch {
	case left > authExpiryWarn:
		return ok("")
	case left <= 0:
		return fail(ExitAuthExpired, "Cursor login expired on %s; run `agent login` then --cursor-import-agent, or --cursor-login", soonest.Format("2006-01-02"))
	default:
		return fail(ExitAuthExpired, "Cursor login expires in %d days (%s); run `agent login` then --cursor-import-agent, or --cursor-login", int(left.Hours()/24), soonest.Format("2006-01-02"))
	}
}

// defaultAuthDir is the proxy's auth directory (config auth-dir).
func defaultAuthDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".cli-proxy-api"
	}
	return filepath.Join(home, ".cli-proxy-api")
}

func expandHome(path string) string {
	if rest, found := strings.CutPrefix(path, "~/"); found {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

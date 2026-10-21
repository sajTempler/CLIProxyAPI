package config

import (
	"testing"
	"time"
)

func TestCursorQuotaEffectiveRefreshInterval(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"":      15 * time.Minute,
		"0":     0,
		"0s":    0,
		"-5m":   0,
		"30s":   time.Minute,
		"20m":   20 * time.Minute,
		"bogus": 15 * time.Minute,
	} {
		if got := (CursorQuotaConfig{RefreshInterval: raw}).EffectiveRefreshInterval(); got != want {
			t.Errorf("%q: got %v, want %v", raw, got, want)
		}
	}
}

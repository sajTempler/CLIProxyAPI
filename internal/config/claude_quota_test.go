package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeQuotaConfigEffectiveRefreshInterval(t *testing.T) {
	for _, tc := range []struct {
		raw, strategy string
		want          time.Duration
	}{
		{"", "soonest-reset", 10 * time.Minute},
		{"", "sr", 10 * time.Minute},
		{"", "round-robin", 0},
		{"0", "soonest-reset", 0},
		{"0s", "soonest-reset", 0},
		{"5m", "round-robin", 5 * time.Minute},
		{"10s", "soonest-reset", time.Minute},
		{"bogus", "soonest-reset", 10 * time.Minute},
	} {
		got := ClaudeQuotaConfig{RefreshInterval: tc.raw}.EffectiveRefreshInterval(tc.strategy)
		if got != tc.want {
			t.Fatalf("interval(%q, %q) = %v, want %v", tc.raw, tc.strategy, got, tc.want)
		}
	}
}

func TestClaudeQuotaConfigEffectiveSkipFiveHourAt(t *testing.T) {
	if got := (ClaudeQuotaConfig{}).EffectiveSkipFiveHourAt(); got != 100 {
		t.Fatalf("default = %v", got)
	}
	v := 90.0
	if got := (ClaudeQuotaConfig{SkipFiveHourAt: &v}).EffectiveSkipFiveHourAt(); got != 90 {
		t.Fatalf("configured = %v", got)
	}
	zero := 0.0
	if got := (ClaudeQuotaConfig{SkipFiveHourAt: &zero}).EffectiveSkipFiveHourAt(); got != 100 {
		t.Fatalf("zero = %v, want default", got)
	}
}

func TestNormalizeSoonestResetStrategy(t *testing.T) {
	for _, in := range []string{"soonest-reset", " SoonestReset ", "sr"} {
		if got, ok := NormalizeSoonestResetStrategy(in); !ok || got != "soonest-reset" {
			t.Fatalf("normalize(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := NormalizeSoonestResetStrategy("fill-first"); ok {
		t.Fatal("fill-first accepted as soonest-reset")
	}
}

func TestClaudeQuotaConfigLoadSaveRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	raw := "routing:\n  strategy: soonest-reset\n  claude-quota:\n    # poll interval comment\n    refresh-interval: \"2m\"\n    skip-five-hour-at: 95\n"
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.Strategy != "soonest-reset" || cfg.Routing.ClaudeQuota.RefreshInterval != "2m" ||
		cfg.Routing.ClaudeQuota.SkipFiveHourAt == nil || *cfg.Routing.ClaudeQuota.SkipFiveHourAt != 95 {
		t.Fatalf("loaded routing = %+v", cfg.Routing)
	}
	clone := cfg.CloneForRuntime()
	*clone.Routing.ClaudeQuota.SkipFiveHourAt = 50
	if *cfg.Routing.ClaudeQuota.SkipFiveHourAt != 95 {
		t.Fatal("CloneForRuntime shares skip-five-hour-at")
	}

	cfg.Routing.ClaudeQuota.RefreshInterval = "15m"
	if err = SaveConfigPreserveComments(file, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# poll interval comment") {
		t.Fatalf("save lost comment:\n%s", data)
	}
	reloaded, err := LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Routing.ClaudeQuota.RefreshInterval != "15m" || reloaded.Routing.ClaudeQuota.SkipFiveHourAt == nil || *reloaded.Routing.ClaudeQuota.SkipFiveHourAt != 95 {
		t.Fatalf("reloaded routing = %+v\n%s", reloaded.Routing, data)
	}
}

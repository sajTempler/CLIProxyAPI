package config

import (
	"strings"
	"time"
)

const (
	// DefaultCursorQuotaRefreshInterval is the Cursor usage poll interval when none is configured.
	DefaultCursorQuotaRefreshInterval = 15 * time.Minute
	// MinCursorQuotaRefreshInterval is the lowest accepted Cursor usage poll interval.
	MinCursorQuotaRefreshInterval = time.Minute
)

// CursorQuotaConfig configures Cursor usage tracking (routing.cursor-quota). Display only.
type CursorQuotaConfig struct {
	// RefreshInterval is the DashboardService usage poll interval per Cursor account.
	// Empty means 15m; "0" disables background polling (manual refresh still works).
	// Values below 1m are clamped to 1m; invalid values fall back to the default.
	RefreshInterval string `yaml:"refresh-interval,omitempty" json:"refresh-interval,omitempty"`
}

// EffectiveRefreshInterval resolves the poll interval. Zero means no background polling.
func (c CursorQuotaConfig) EffectiveRefreshInterval() time.Duration {
	raw := strings.TrimSpace(c.RefreshInterval)
	if raw == "" {
		return DefaultCursorQuotaRefreshInterval
	}
	parsed, errParse := time.ParseDuration(raw)
	if errParse != nil {
		return DefaultCursorQuotaRefreshInterval
	}
	if parsed <= 0 {
		return 0
	}
	return max(parsed, MinCursorQuotaRefreshInterval)
}

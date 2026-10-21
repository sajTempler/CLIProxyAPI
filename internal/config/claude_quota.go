package config

import (
	"strings"
	"time"
)

const (
	// RoutingStrategySoonestReset routes new sessions to the Claude account whose weekly window resets soonest.
	RoutingStrategySoonestReset = "soonest-reset"
	// DefaultClaudeQuotaRefreshInterval is the usage poll interval used when soonest-reset is active.
	DefaultClaudeQuotaRefreshInterval = 10 * time.Minute
	// MinClaudeQuotaRefreshInterval is the lowest accepted usage poll interval.
	MinClaudeQuotaRefreshInterval = time.Minute
	// DefaultClaudeQuotaSkipFiveHourAt is the 5-hour used percent at which soonest-reset skips an account.
	DefaultClaudeQuotaSkipFiveHourAt = 100.0
)

// ClaudeQuotaConfig configures Claude subscription quota tracking (routing.claude-quota).
type ClaudeQuotaConfig struct {
	// RefreshInterval is the OAuth usage poll interval per Claude account.
	// Empty means 10m when routing.strategy is soonest-reset, otherwise off. "0" disables polling.
	// Values below 1m are clamped to 1m; invalid values fall back to the default.
	RefreshInterval string `yaml:"refresh-interval,omitempty" json:"refresh-interval,omitempty"`

	// SkipFiveHourAt is the 5-hour used percent at which soonest-reset skips an account
	// for new sessions. Default: 100.
	SkipFiveHourAt *float64 `yaml:"skip-five-hour-at,omitempty" json:"skip-five-hour-at,omitempty"`
}

// NormalizeSoonestResetStrategy maps "soonest-reset" aliases to the canonical name.
// It reports false for any other value.
func NormalizeSoonestResetStrategy(strategy string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case RoutingStrategySoonestReset, "soonestreset", "sr":
		return RoutingStrategySoonestReset, true
	default:
		return "", false
	}
}

// EffectiveRefreshInterval resolves the usage poll interval for the given routing strategy.
// Zero means no background polling.
func (c ClaudeQuotaConfig) EffectiveRefreshInterval(strategy string) time.Duration {
	_, soonest := NormalizeSoonestResetStrategy(strategy)
	raw := strings.TrimSpace(c.RefreshInterval)
	if raw == "" {
		if soonest {
			return DefaultClaudeQuotaRefreshInterval
		}
		return 0
	}
	parsed, errParse := time.ParseDuration(raw)
	if errParse != nil {
		return DefaultClaudeQuotaRefreshInterval
	}
	if parsed <= 0 {
		return 0
	}
	if parsed < MinClaudeQuotaRefreshInterval {
		return MinClaudeQuotaRefreshInterval
	}
	return parsed
}

// EffectiveSkipFiveHourAt returns the configured 5-hour skip threshold or the default.
func (c ClaudeQuotaConfig) EffectiveSkipFiveHourAt() float64 {
	if c.SkipFiveHourAt == nil || *c.SkipFiveHourAt <= 0 {
		return DefaultClaudeQuotaSkipFiveHourAt
	}
	return *c.SkipFiveHourAt
}

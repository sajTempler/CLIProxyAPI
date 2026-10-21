package cliproxy

import (
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// applySoonestResetRoutingState records the soonest-reset strategy and its
// selector inputs. The tracker is attached by the caller.
func applySoonestResetRoutingState(state *routingRuntimeState, cfg *config.Config) {
	if state == nil || cfg == nil {
		return
	}
	strategy, ok := internalconfig.NormalizeSoonestResetStrategy(cfg.Routing.Strategy)
	if !ok {
		return
	}
	state.strategy = strategy
	state.skipFiveHourAt = cfg.Routing.ClaudeQuota.EffectiveSkipFiveHourAt()
}

func newSoonestResetSelector(state routingRuntimeState) coreauth.Selector {
	return coreauth.NewSoonestResetSelector(state.claudeQuota, coreauth.SoonestResetConfig{
		SkipFiveHourAtPercent: state.skipFiveHourAt,
	})
}

// ensureClaudeQuotaTracker returns the manager's Claude quota tracker, attaching one if missing.
func ensureClaudeQuotaTracker(m *coreauth.Manager) *coreauth.ClaudeQuotaTracker {
	if m == nil {
		return nil
	}
	if t := m.ClaudeQuotaTracker(); t != nil {
		return t
	}
	t := coreauth.NewClaudeQuotaTracker()
	m.SetClaudeQuotaTracker(t)
	return t
}

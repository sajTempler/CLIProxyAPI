package cliproxy

import (
	"context"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
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

// claudeQuotaPollInterval resolves routing.claude-quota.refresh-interval for cfg.
func claudeQuotaPollInterval(cfg *config.Config) time.Duration {
	if cfg == nil {
		return 0
	}
	return cfg.Routing.ClaudeQuota.EffectiveRefreshInterval(cfg.Routing.Strategy)
}

// claudeQuotaPoller returns the running poller. It is registered as the
// tracker's refresher, so the Service needs no extra field.
func (s *Service) claudeQuotaPoller() *claudequota.Poller {
	if s == nil || s.coreManager == nil {
		return nil
	}
	poller, _ := s.coreManager.ClaudeQuotaTracker().Refresher().(*claudequota.Poller)
	return poller
}

// startClaudeQuotaPoller starts the OAuth usage poller. Background polling is
// off unless soonest-reset is active or refresh-interval is set; manual
// refreshes through the management API always work.
func (s *Service) startClaudeQuotaPoller(ctx context.Context) {
	if s == nil || s.coreManager == nil || s.claudeQuotaPoller() != nil {
		return
	}
	tracker := ensureClaudeQuotaTracker(s.coreManager)
	interval := claudeQuotaPollInterval(s.currentConfig())
	poller := claudequota.New(claudequota.Options{
		Tracker: tracker,
		Auths:   s.coreManager,
		Client: func(auth *coreauth.Auth) *http.Client {
			return helps.NewProxyAwareHTTPClient(ctx, s.currentConfig(), auth, 0)
		},
		UserAgent: func() string {
			return "claude-cli/" + helps.DefaultClaudeVersion(s.currentConfig()) + " (external, cli)"
		},
		Interval: interval,
	})
	poller.Start(ctx)
	log.Infof("claude quota poller started (interval=%s)", interval)
}

func (s *Service) stopClaudeQuotaPoller() {
	if poller := s.claudeQuotaPoller(); poller != nil {
		poller.Stop()
	}
}

func (s *Service) applyClaudeQuotaPollerConfig(cfg *config.Config) {
	if poller := s.claudeQuotaPoller(); poller != nil && cfg != nil {
		poller.SetInterval(claudeQuotaPollInterval(cfg))
	}
}

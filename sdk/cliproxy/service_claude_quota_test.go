package cliproxy

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSoonestResetRoutingSelector(t *testing.T) {
	skip := 90.0
	for _, alias := range []string{"soonest-reset", "SoonestReset", "sr"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
			Strategy:    alias,
			ClaudeQuota: internalconfig.ClaudeQuotaConfig{SkipFiveHourAt: &skip},
		}})
		if state.strategy != "soonest-reset" || state.skipFiveHourAt != 90 {
			t.Fatalf("%s: state = %+v", alias, state)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.SoonestResetSelector); !ok {
			t.Fatalf("%s: selector type = %T", alias, newRoutingSelector(state))
		}
	}
	other := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
		Strategy:    "fill-first",
		ClaudeQuota: internalconfig.ClaudeQuotaConfig{SkipFiveHourAt: &skip},
	}})
	if other.skipFiveHourAt != 0 {
		t.Fatal("skip threshold must not affect other strategies (no needless selector rebuilds)")
	}
}

func TestSoonestResetRoutingSelectorWrappedByAffinity(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
		Strategy:        "soonest-reset",
		SessionAffinity: true,
	}})
	if _, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector); !ok {
		t.Fatalf("selector type = %T, want session affinity wrapper", newRoutingSelector(state))
	}
}

func TestApplyManagerConfigAttachesClaudeQuotaTracker(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{coreManager: manager}
	cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset"}}
	if !service.applyManagerConfig(nil, configCommit{cfg: cfg, sequence: 1}) {
		t.Fatal("applyManagerConfig failed")
	}
	tracker := manager.ClaudeQuotaTracker()
	if tracker == nil || service.appliedRoutingState == nil || service.appliedRoutingState.claudeQuota != tracker {
		t.Fatal("tracker not attached to manager and routing state")
	}
	if _, ok := manager.Selector().(*coreauth.SoonestResetSelector); !ok {
		t.Fatalf("selector = %T", manager.Selector())
	}
	// Re-applying the same config keeps the selector (same tracker pointer -> equal state).
	before := manager.Selector()
	service.applyManagerConfig(nil, configCommit{cfg: cfg, sequence: 2})
	if manager.Selector() != before {
		t.Fatal("unchanged config rebuilt the selector")
	}
}

func TestServiceClaudeQuotaPollerLifecycle(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil) // no credentials: the poller never calls upstream
	service := &Service{coreManager: manager, cfg: &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset"}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service.startClaudeQuotaPoller(ctx)
	poller := service.claudeQuotaPoller()
	if poller == nil || poller.Interval() != 10*time.Minute {
		t.Fatalf("poller = %v", poller)
	}
	service.startClaudeQuotaPoller(ctx)
	if service.claudeQuotaPoller() != poller {
		t.Fatal("second start replaced the running poller")
	}

	// Hot reload: explicit interval, then off.
	service.applyClaudeQuotaPollerConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
		Strategy: "round-robin", ClaudeQuota: internalconfig.ClaudeQuotaConfig{RefreshInterval: "3m"},
	}})
	if poller.Interval() != 3*time.Minute {
		t.Fatalf("interval after reload = %v", poller.Interval())
	}
	service.applyClaudeQuotaPollerConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "round-robin"}})
	if poller.Interval() != 0 {
		t.Fatalf("interval for round-robin without refresh-interval = %v, want 0", poller.Interval())
	}

	service.stopClaudeQuotaPoller()
	if service.claudeQuotaPoller() != nil {
		t.Fatal("stop did not unregister the poller")
	}
}

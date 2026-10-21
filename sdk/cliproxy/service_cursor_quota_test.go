package cliproxy

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestServiceCursorQuotaPollerLifecycle(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil) // no credentials: the poller never calls upstream
	service := &Service{coreManager: manager, cfg: &internalconfig.Config{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service.startCursorQuotaPoller(ctx)
	poller := service.cursorQuotaPoller()
	if poller == nil || poller.Interval() != 15*time.Minute {
		t.Fatalf("poller = %v", poller)
	}
	if manager.CursorQuotaTracker().Refresher() == nil {
		t.Fatal("poller not registered on the tracker")
	}
	service.startCursorQuotaPoller(ctx)
	if service.cursorQuotaPoller() != poller {
		t.Fatal("second start replaced the running poller")
	}
	service.applyCursorQuotaPollerConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
		CursorQuota: internalconfig.CursorQuotaConfig{RefreshInterval: "0"},
	}})
	if poller.Interval() != 0 {
		t.Fatalf("interval after reload = %v, want 0", poller.Interval())
	}
	service.stopCursorQuotaPoller()
	if service.cursorQuotaPoller() != nil {
		t.Fatal("stop did not unregister the poller")
	}
}

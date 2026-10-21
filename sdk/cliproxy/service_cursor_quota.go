package cliproxy

import (
	"context"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cursorquota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

// ensureCursorQuotaTracker returns the manager's Cursor quota tracker, attaching one if missing.
func ensureCursorQuotaTracker(m *coreauth.Manager) *coreauth.CursorQuotaTracker {
	if m == nil {
		return nil
	}
	if t := m.CursorQuotaTracker(); t != nil {
		return t
	}
	t := coreauth.NewCursorQuotaTracker()
	m.SetCursorQuotaTracker(t)
	return t
}

func cursorQuotaPollInterval(cfg *config.Config) time.Duration {
	if cfg == nil {
		return 0
	}
	return cfg.Routing.CursorQuota.EffectiveRefreshInterval()
}

// cursorQuotaPoller returns the running poller (registered as the tracker's refresher).
func (s *Service) cursorQuotaPoller() *cursorquota.Poller {
	if s == nil || s.coreManager == nil {
		return nil
	}
	poller, _ := s.coreManager.CursorQuotaTracker().Refresher().(*cursorquota.Poller)
	return poller
}

// startCursorQuotaPoller starts the Cursor usage poller. It only ever polls
// `type: "cursor"` credentials, so it is idle when none are loaded.
func (s *Service) startCursorQuotaPoller(ctx context.Context) {
	if s == nil || s.coreManager == nil || s.cursorQuotaPoller() != nil {
		return
	}
	tracker := ensureCursorQuotaTracker(s.coreManager)
	interval := cursorQuotaPollInterval(s.currentConfig())
	poller := cursorquota.New(cursorquota.Options{
		Tracker: tracker,
		Auths:   s.coreManager,
		Client: func(auth *coreauth.Auth) *http.Client {
			return helps.NewProxyAwareHTTPClient(ctx, s.currentConfig(), auth, 0)
		},
		ClientVersion: helps.GetCursorClientVersion,
		Interval:      interval,
	})
	poller.Start(ctx)
	log.Debugf("cursor quota poller started (interval=%s)", interval)
}

func (s *Service) stopCursorQuotaPoller() {
	if poller := s.cursorQuotaPoller(); poller != nil {
		poller.Stop()
	}
}

func (s *Service) applyCursorQuotaPollerConfig(cfg *config.Config) {
	if poller := s.cursorQuotaPoller(); poller != nil && cfg != nil {
		poller.SetInterval(cursorQuotaPollInterval(cfg))
	}
}

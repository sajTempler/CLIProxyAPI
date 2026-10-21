package cliproxy

import (
	"context"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cursorquota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// cursorModelsMaxAge is how long a fetched Cursor catalog is reused.
	cursorModelsMaxAge = 6 * time.Hour
	// cursorModelsRetry spaces fetch attempts after a failure; the timer
	// doubles it per consecutive failure up to cursorModelsMaxBackoff.
	cursorModelsRetry      = 10 * time.Minute
	cursorModelsMaxBackoff = time.Hour
	// cursorModelsMinDelay keeps the timer from spinning on a stale clock.
	cursorModelsMinDelay = time.Minute
)

// fetchCursorModels is replaced in tests so registration never calls Cursor.
var fetchCursorModels = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]*registry.ModelInfo, error) {
	return executor.FetchCursorModels(ctx, cfg, auth)
}

var cursorModelsRefresh struct {
	mu      sync.Mutex
	running bool
	lastTry time.Time
}

// tryStartCursorModelsFetch claims the single fetch slot. respectRetry also
// skips the claim within cursorModelsRetry of the last attempt (registration
// path); the timer does its own backoff.
func tryStartCursorModelsFetch(now time.Time, respectRetry bool) bool {
	cursorModelsRefresh.mu.Lock()
	defer cursorModelsRefresh.mu.Unlock()
	if cursorModelsRefresh.running {
		return false
	}
	if respectRetry && !cursorModelsRefresh.lastTry.IsZero() && now.Sub(cursorModelsRefresh.lastTry) < cursorModelsRetry {
		return false
	}
	cursorModelsRefresh.running, cursorModelsRefresh.lastTry = true, now
	return true
}

func endCursorModelsFetch() {
	cursorModelsRefresh.mu.Lock()
	cursorModelsRefresh.running = false
	cursorModelsRefresh.mu.Unlock()
}

// refreshCursorModelsAsync fetches the account's Cursor catalog in the
// background when none is cached or it is stale. Registration meanwhile
// serves the cached or static list; when the fetched model ids differ, every
// cursor auth is registered again.
func (s *Service) refreshCursorModelsAsync(auth *coreauth.Auth) {
	if s == nil || auth == nil {
		return
	}
	now := time.Now()
	if fetched := registry.CursorModelsFetchedAt(); !fetched.IsZero() && now.Sub(fetched) < cursorModelsMaxAge {
		return
	}
	if !tryStartCursorModelsFetch(now, true) {
		return
	}
	cfg := s.currentConfig()
	snapshot := auth.Clone()
	go func() {
		defer endCursorModelsFetch()
		_ = s.installCursorModels(context.Background(), cfg, snapshot)
	}()
}

// installCursorModels fetches the catalog with auth and installs it,
// re-registering every cursor auth when the model ids changed. The caller
// holds the fetch slot.
func (s *Service) installCursorModels(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) error {
	models, err := fetchCursorModels(ctx, cfg, auth)
	if err != nil {
		log.Warnf("cursor: model list unavailable, serving the cached or static list: %v", err)
		return err
	}
	if !registry.SetCursorModels(models, time.Now()) {
		return nil
	}
	log.Infof("cursor: %d models from GetUsableModels", len(models))
	if s.coreManager == nil {
		return nil
	}
	for _, a := range s.coreManager.List() {
		if a != nil && strings.EqualFold(strings.TrimSpace(a.Provider), "cursor") {
			s.registerModelsForAuth(ctx, a)
		}
	}
	return nil
}

// cursorModelsAuth picks an enabled cursor credential to fetch the catalog with.
func (s *Service) cursorModelsAuth() *coreauth.Auth {
	if s == nil || s.coreManager == nil {
		return nil
	}
	for _, a := range s.coreManager.List() {
		if a != nil && !a.Disabled && strings.EqualFold(strings.TrimSpace(a.Provider), "cursor") {
			return a
		}
	}
	return nil
}

// cursorModelsNextDelay is the wait before the timer's next catalog check:
// the time left until the catalog is cursorModelsMaxAge old (jittered by
// ±10 %, as the quota pollers do), or after failures an exponential backoff
// from cursorModelsRetry capped at cursorModelsMaxBackoff. r is in [0,1).
func cursorModelsNextDelay(now, fetchedAt time.Time, failures int, r float64) time.Duration {
	if failures > 0 {
		d := cursorModelsRetry
		for i := 1; i < failures && d < cursorModelsMaxBackoff; i++ {
			d *= 2
		}
		return min(d, cursorModelsMaxBackoff)
	}
	if fetchedAt.IsZero() {
		return cursorModelsRetry
	}
	d := cursorquota.Jitter(cursorModelsMaxAge, r) - now.Sub(fetchedAt)
	return max(d, cursorModelsMinDelay)
}

// startCursorModelsRefresher re-fetches the Cursor catalog on a timer, so it
// stays fresh without an auth re-registering. It is idle (one cheap check per
// interval) when no cursor credential is loaded.
func (s *Service) startCursorModelsRefresher(ctx context.Context) {
	if s == nil {
		return
	}
	go func() {
		failures := 0
		for {
			timer := time.NewTimer(cursorModelsNextDelay(time.Now(), registry.CursorModelsFetchedAt(), failures, rand.Float64()))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			failures = s.cursorModelsTick(ctx, failures)
		}
	}()
}

// cursorModelsTick runs one timer check and returns the new failure count.
func (s *Service) cursorModelsTick(ctx context.Context, failures int) int {
	now := time.Now()
	if fetched := registry.CursorModelsFetchedAt(); failures == 0 && !fetched.IsZero() && now.Sub(fetched) < cursorModelsMaxAge*9/10 {
		return 0 // refreshed meanwhile (registration path)
	}
	auth := s.cursorModelsAuth()
	if auth == nil {
		return 0
	}
	if !tryStartCursorModelsFetch(now, false) {
		return failures
	}
	defer endCursorModelsFetch()
	if err := s.installCursorModels(ctx, s.currentConfig(), auth.Clone()); err != nil {
		return failures + 1
	}
	return 0
}

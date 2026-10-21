package cliproxy

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// cursorModelsMaxAge is how long a fetched Cursor catalog is reused.
	cursorModelsMaxAge = 6 * time.Hour
	// cursorModelsRetry spaces fetch attempts after a failure.
	cursorModelsRetry = 10 * time.Minute
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
	cursorModelsRefresh.mu.Lock()
	if cursorModelsRefresh.running || (!cursorModelsRefresh.lastTry.IsZero() && now.Sub(cursorModelsRefresh.lastTry) < cursorModelsRetry) {
		cursorModelsRefresh.mu.Unlock()
		return
	}
	cursorModelsRefresh.running, cursorModelsRefresh.lastTry = true, now
	cursorModelsRefresh.mu.Unlock()

	cfg := s.currentConfig()
	snapshot := auth.Clone()
	go func() {
		defer func() {
			cursorModelsRefresh.mu.Lock()
			cursorModelsRefresh.running = false
			cursorModelsRefresh.mu.Unlock()
		}()
		ctx := context.Background()
		models, err := fetchCursorModels(ctx, cfg, snapshot)
		if err != nil {
			log.Warnf("cursor: model list unavailable, serving the static fallback: %v", err)
			return
		}
		if !registry.SetCursorModels(models, time.Now()) {
			return
		}
		log.Infof("cursor: %d models from GetUsableModels", len(models))
		if s.coreManager == nil {
			return
		}
		for _, a := range s.coreManager.List() {
			if a != nil && strings.EqualFold(strings.TrimSpace(a.Provider), "cursor") {
				s.registerModelsForAuth(ctx, a)
			}
		}
	}()
}

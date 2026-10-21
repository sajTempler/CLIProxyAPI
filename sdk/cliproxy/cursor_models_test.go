package cliproxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestRegisterModelsForAuth_CursorRegistersPrefixedModelsAndFetchesCatalog(t *testing.T) {
	fetched := make(chan struct{})
	prev := fetchCursorModels
	fetchCursorModels = func(_ context.Context, _ *config.Config, auth *coreauth.Auth) ([]*internalregistry.ModelInfo, error) {
		defer close(fetched)
		if auth == nil || auth.Provider != "cursor" {
			t.Errorf("fetch got auth %+v", auth)
		}
		return []*internalregistry.ModelInfo{internalregistry.NewCursorModelInfo("composer-2.5", "Composer 2.5"), internalregistry.NewCursorModelInfo("live-only", "Live")}, nil
	}
	t.Cleanup(func() { fetchCursorModels = prev })

	service := &Service{}
	auth := &coreauth.Auth{ID: "auth-cursor-test", Provider: "cursor", Status: coreauth.StatusActive, Metadata: map[string]any{"type": "cursor"}}
	reg := GlobalModelRegistry()
	reg.UnregisterClient(auth.ID)
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(context.Background(), auth)
	models := reg.GetModelsForClient(auth.ID)
	if len(models) == 0 {
		t.Fatal("no models registered for the cursor auth")
	}
	for _, m := range models {
		if !strings.HasPrefix(m.ID, "cursor/") {
			t.Fatalf("model %q is not namespaced with cursor/", m.ID)
		}
	}

	select {
	case <-fetched:
	case <-time.After(5 * time.Second):
		t.Fatal("the live catalog was never fetched")
	}
	// The catalog is installed right after the fetch returns.
	deadline := time.Now().Add(5 * time.Second)
	for internalregistry.CursorModelsFetchedAt().IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("fetched catalog was not installed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var live bool
	for _, m := range internalregistry.GetCursorModels() {
		live = live || m.ID == "cursor/live-only"
	}
	if !live {
		t.Fatal("GetCursorModels must serve the fetched catalog")
	}
}

func TestCursorModelsNextDelay(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		fetchedAt time.Time
		failures  int
		r         float64
		want      time.Duration
	}{
		{"never fetched", time.Time{}, 0, 0.5, cursorModelsRetry},
		{"fresh catalog waits the max age", now, 0, 0.5, cursorModelsMaxAge},
		{"jitter low", now, 0, 0, cursorModelsMaxAge - cursorModelsMaxAge/10},
		{"jitter high", now, 0, 0.75, cursorModelsMaxAge + cursorModelsMaxAge/20},
		{"partly aged", now.Add(-2 * time.Hour), 0, 0.5, cursorModelsMaxAge - 2*time.Hour},
		{"overdue clamps", now.Add(-24 * time.Hour), 0, 0.5, cursorModelsMinDelay},
		{"first failure", now, 1, 0.5, cursorModelsRetry},
		{"second failure doubles", now, 2, 0.5, 2 * cursorModelsRetry},
		{"third failure doubles again", now, 3, 0.5, 4 * cursorModelsRetry},
		{"backoff caps", now, 10, 0.5, cursorModelsMaxBackoff},
	}
	for _, tc := range cases {
		got := cursorModelsNextDelay(now, tc.fetchedAt, tc.failures, tc.r)
		if diff := got - tc.want; diff > time.Millisecond || diff < -time.Millisecond {
			t.Errorf("%s: delay = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestCursorModelsTickFetchesOnTimerAndBacksOff(t *testing.T) {
	var calls int
	fail := true
	prev := fetchCursorModels
	fetchCursorModels = func(_ context.Context, _ *config.Config, auth *coreauth.Auth) ([]*internalregistry.ModelInfo, error) {
		calls++
		if auth == nil || auth.Provider != "cursor" {
			t.Errorf("fetch got auth %+v", auth)
		}
		if fail {
			return nil, context.DeadlineExceeded
		}
		return []*internalregistry.ModelInfo{internalregistry.NewCursorModelInfo("timer-only", "Timer")}, nil
	}
	t.Cleanup(func() { fetchCursorModels = prev })

	empty := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
	if got := empty.cursorModelsTick(context.Background(), 2); got != 0 || calls != 0 {
		t.Fatalf("no cursor auth: failures=%d calls=%d, want 0 and no fetch", got, calls)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "auth-cursor-timer", Provider: "cursor", Status: coreauth.StatusActive, Metadata: map[string]any{"type": "cursor"}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := &Service{cfg: &config.Config{}, coreManager: manager}
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })

	if got := svc.cursorModelsTick(context.Background(), 1); got != 2 || calls != 1 {
		t.Fatalf("failed fetch: failures=%d calls=%d, want 2 and 1", got, calls)
	}
	fail = false
	if got := svc.cursorModelsTick(context.Background(), 2); got != 0 || calls != 2 {
		t.Fatalf("successful fetch: failures=%d calls=%d, want 0 and 2", got, calls)
	}
	var live bool
	for _, m := range internalregistry.GetCursorModels() {
		live = live || m.ID == "cursor/timer-only"
	}
	if !live {
		t.Fatal("the timer fetch must install the catalog")
	}
	// Fresh catalog: the next tick does not call Cursor.
	if got := svc.cursorModelsTick(context.Background(), 0); got != 0 || calls != 2 {
		t.Fatalf("fresh catalog: failures=%d calls=%d, want 0 and 2", got, calls)
	}
}

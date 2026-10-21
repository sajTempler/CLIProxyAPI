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

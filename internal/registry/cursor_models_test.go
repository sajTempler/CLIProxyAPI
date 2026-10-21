package registry

import (
	"strings"
	"testing"
	"time"
)

func TestGetCursorModelsStaticFallbackIsPrefixed(t *testing.T) {
	resetCursorModelsForTest()
	t.Cleanup(resetCursorModelsForTest)
	models := GetCursorModels()
	if len(models) == 0 {
		t.Fatal("static fallback is empty")
	}
	var composer bool
	for _, m := range models {
		if !strings.HasPrefix(m.ID, CursorModelPrefix) || m.OwnedBy != "cursor" || m.Thinking == nil {
			t.Fatalf("bad cursor model %+v", m)
		}
		composer = composer || m.ID == "cursor/composer-2.5"
	}
	if !composer {
		t.Fatal("static fallback must include cursor/composer-2.5 (the canary model)")
	}
	if got := GetStaticModelDefinitionsByChannel("cursor"); len(got) != len(models) {
		t.Fatalf("channel lookup returned %d models, want %d", len(got), len(models))
	}
	if LookupStaticModelInfo("cursor/composer-2.5") == nil {
		t.Fatal("LookupStaticModelInfo must find cursor models")
	}
}

func TestSetCursorModelsReplacesFallback(t *testing.T) {
	resetCursorModelsForTest()
	t.Cleanup(resetCursorModelsForTest)
	now := time.Unix(1_800_000_000, 0)
	fetched := []*ModelInfo{NewCursorModelInfo("composer-9", "Composer 9"), NewCursorModelInfo("cursor/gpt-x", "")}
	if !SetCursorModels(fetched, now) {
		t.Fatal("first fetch must report a change")
	}
	if SetCursorModels(fetched, now) {
		t.Fatal("same ids must not report a change")
	}
	if SetCursorModels(nil, now) {
		t.Fatal("an empty fetch must be ignored")
	}
	got := GetCursorModels()
	if len(got) != 2 || got[0].ID != "cursor/composer-9" || got[1].ID != "cursor/gpt-x" {
		t.Fatalf("catalog = %v", got)
	}
	if !CursorModelsFetchedAt().Equal(now) {
		t.Fatalf("fetchedAt = %v", CursorModelsFetchedAt())
	}
}

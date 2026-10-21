package registry

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// CursorModelPrefix namespaces Cursor models ("cursor/composer-2.5") so their
// names never collide with the claude provider's (Cursor also serves
// claude-* models).
const CursorModelPrefix = "cursor/"

// staticCursorModels is the fallback catalog used until the live
// AgentService/GetUsableModels list has been fetched (or when it fails).
var staticCursorModels = []struct{ id, name string }{
	{"composer-2.5", "Composer 2.5"},
	{"composer-2.5-fast", "Composer 2.5 Fast"},
	{"default", "Auto"},
	{"claude-sonnet-5-5-high", "Claude Sonnet 5.5 High"},
	{"claude-opus-5-5-high", "Claude Opus 5.5 1M High"},
	{"claude-haiku-5-5-thinking-high", "Claude Haiku 5.5 High"},
	{"gpt-5.5-medium", "GPT-5.5 1M"},
	{"gpt-5.4-mini-medium", "GPT-5.4 Mini"},
	{"gemini-3.1-pro", "Gemini 3.1 Pro"},
}

var cursorCatalog struct {
	mu        sync.RWMutex
	models    []*ModelInfo
	fetchedAt time.Time
}

// NewCursorModelInfo describes one Cursor model; id is Cursor's model id
// without the prefix. Thinking is budget-shaped only so Claude requests keep
// their thinking setting (Cursor's thinking deltas are surfaced only then);
// Cursor itself picks the reasoning level from the model id.
func NewCursorModelInfo(id, displayName string) *ModelInfo {
	id = strings.TrimPrefix(strings.TrimSpace(id), CursorModelPrefix)
	if displayName == "" {
		displayName = id
	}
	return &ModelInfo{
		ID:                        CursorModelPrefix + id,
		Object:                    "model",
		OwnedBy:                   "cursor",
		Type:                      "cursor",
		DisplayName:               displayName + " (Cursor)",
		ContextLength:             cursorContextLength(displayName),
		MaxCompletionTokens:       64000,
		Thinking:                  &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: true},
		SupportedInputModalities:  []string{"text"},
		SupportedOutputModalities: []string{"text"},
	}
}

// cursorOneMillionName matches Cursor display names of 1M-context variants
// ("Claude Opus 5.5 1M High", "GPT-5.5 1M"). GetUsableModels carries no
// window size; the name is the only signal.
var cursorOneMillionName = regexp.MustCompile(`(^|\s)1M(\s|$)`)

// cursorContextLength is the advertised context window: 1M for "1M"
// variants, otherwise 200k (Cursor's default window). Metadata only:
// Claude Code does not read it from a gateway (docs/cursor/USAGE.md).
func cursorContextLength(displayName string) int {
	if cursorOneMillionName.MatchString(displayName) {
		return 1000000
	}
	return 200000
}

// GetCursorModels returns the live Cursor catalog when one was fetched,
// otherwise the static fallback.
func GetCursorModels() []*ModelInfo {
	cursorCatalog.mu.RLock()
	models := cursorCatalog.models
	cursorCatalog.mu.RUnlock()
	if len(models) > 0 {
		return cloneModelInfos(models)
	}
	out := make([]*ModelInfo, 0, len(staticCursorModels))
	for _, m := range staticCursorModels {
		out = append(out, NewCursorModelInfo(m.id, m.name))
	}
	return out
}

// SetCursorModels installs a fetched catalog and reports whether its model
// ids differ from the catalog served before. An empty list is ignored.
func SetCursorModels(models []*ModelInfo, now time.Time) bool {
	if len(models) == 0 {
		return false
	}
	before := GetCursorModels()
	cursorCatalog.mu.Lock()
	cursorCatalog.models = cloneModelInfos(models)
	cursorCatalog.fetchedAt = now
	cursorCatalog.mu.Unlock()
	return !sameModelIDs(before, models)
}

// CursorModelsFetchedAt reports when the live catalog was last installed (zero: never).
func CursorModelsFetchedAt() time.Time {
	cursorCatalog.mu.RLock()
	defer cursorCatalog.mu.RUnlock()
	return cursorCatalog.fetchedAt
}

// resetCursorModelsForTest drops the live catalog.
func resetCursorModelsForTest() {
	cursorCatalog.mu.Lock()
	cursorCatalog.models, cursorCatalog.fetchedAt = nil, time.Time{}
	cursorCatalog.mu.Unlock()
}

func sameModelIDs(a, b []*ModelInfo) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, m := range a {
		if m != nil {
			seen[m.ID]++
		}
	}
	for _, m := range b {
		if m == nil || seen[m.ID] == 0 {
			return false
		}
		seen[m.ID]--
	}
	return true
}

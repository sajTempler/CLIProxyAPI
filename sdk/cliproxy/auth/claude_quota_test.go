package auth

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func loadClaudeHeaderFixture(t *testing.T, name string) http.Header {
	t.Helper()
	f, errOpen := os.Open(filepath.Join("testdata", name))
	if errOpen != nil {
		t.Fatalf("open fixture: %v", errOpen)
	}
	defer func() { _ = f.Close() }()
	h := http.Header{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		h.Add(strings.TrimSpace(key), strings.TrimSpace(value))
	}
	return h
}

func loadClaudeUsageFixture(t *testing.T, name string) ClaudeUsageResponse {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", name))
	if errRead != nil {
		t.Fatalf("read fixture: %v", errRead)
	}
	var u ClaudeUsageResponse
	if errUnmarshal := json.Unmarshal(raw, &u); errUnmarshal != nil {
		t.Fatalf("decode fixture: %v", errUnmarshal)
	}
	return u
}

func newTestClaudeQuotaTracker(now time.Time) (*ClaudeQuotaTracker, *fakeClock) {
	clock := &fakeClock{t: now}
	tracker := NewClaudeQuotaTracker()
	tracker.SetNowFunc(clock.Now)
	return tracker, clock
}

func mustWindow(t *testing.T, s ClaudeQuotaSnapshot, name ClaudeQuotaWindowName) ClaudeQuotaWindow {
	t.Helper()
	w, ok := s.Windows[name]
	if !ok {
		t.Fatalf("window %s missing; windows=%+v", name, s.Windows)
	}
	return w
}

var liveFixtureNow = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

func TestClaudeQuotaObserveHeaders_FableFixture(t *testing.T) {
	tracker, _ := newTestClaudeQuotaTracker(liveFixtureNow)
	tracker.ObserveHeaders("a", "claude-fable-5-1", loadClaudeHeaderFixture(t, "claude_headers_fable.txt"), liveFixtureNow)

	snap, ok := tracker.Snapshot("a")
	if !ok {
		t.Fatal("snapshot missing")
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("windows = %d, want 3: %+v", len(snap.Windows), snap.Windows)
	}
	five := mustWindow(t, snap, ClaudeWindowFiveHour)
	if five.UsedPercent != 10 || five.Status != "allowed" || five.Source != ClaudeQuotaSourceHeaders {
		t.Fatalf("five_hour = %+v", five)
	}
	if want := time.Date(2026, 10, 21, 10, 30, 0, 0, time.UTC); !five.ResetsAt.Equal(want) {
		t.Fatalf("five_hour reset = %v, want %v", five.ResetsAt, want)
	}
	if week := mustWindow(t, snap, ClaudeWindowSevenDay); week.UsedPercent != 55 {
		t.Fatalf("seven_day used = %v, want 55", week.UsedPercent)
	}
	fable := mustWindow(t, snap, ClaudeWindowSevenDayFable)
	if fable.UsedPercent != 46 || !fable.ResetsAt.Equal(time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("seven_day_fable = %+v", fable)
	}
}

func TestClaudeQuotaObserveHeaders_OpusFixtureHasNoFable(t *testing.T) {
	tracker, _ := newTestClaudeQuotaTracker(liveFixtureNow)
	tracker.ObserveHeaders("a", "claude-opus", loadClaudeHeaderFixture(t, "claude_headers_opus.txt"), liveFixtureNow)
	snap, _ := tracker.Snapshot("a")
	if _, ok := snap.Windows[ClaudeWindowSevenDayFable]; ok {
		t.Fatal("opus headers must not produce a Fable window")
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
}

func TestClaudeQuotaObserveHeaders_IgnoresNonUnified(t *testing.T) {
	tracker, _ := newTestClaudeQuotaTracker(liveFixtureNow)
	tracker.ObserveHeaders("a", "m", http.Header{"Retry-After": {"30"}, "Content-Type": {"json"}}, liveFixtureNow)
	if _, ok := tracker.Snapshot("a"); ok {
		t.Fatal("non-unified headers must not create a snapshot")
	}
}

func TestClaudeQuotaObserveHeaders_RFC3339ResetAndRejectedWithoutUtilization(t *testing.T) {
	tracker, _ := newTestClaudeQuotaTracker(liveFixtureNow)
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "2026-10-21T10:30:00Z")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	tracker.ObserveHeaders("a", "m", h, liveFixtureNow)
	five := mustWindow(t, mustSnapshot(t, tracker, "a"), ClaudeWindowFiveHour)
	if five.UsedPercent != 100 || five.Status != "rejected" || !five.ResetsAt.Equal(time.Date(2026, 10, 21, 10, 30, 0, 0, time.UTC)) {
		t.Fatalf("five_hour = %+v", five)
	}
}

func mustSnapshot(t *testing.T, tracker *ClaudeQuotaTracker, id string) ClaudeQuotaSnapshot {
	t.Helper()
	snap, ok := tracker.Snapshot(id)
	if !ok {
		t.Fatalf("snapshot %s missing", id)
	}
	return snap
}

func TestClaudeQuotaObserveUsage_LiveFixture(t *testing.T) {
	tracker, _ := newTestClaudeQuotaTracker(liveFixtureNow)
	tracker.ObserveUsage("a", loadClaudeUsageFixture(t, "claude_usage_live.json"), liveFixtureNow)
	snap := mustSnapshot(t, tracker, "a")
	five := mustWindow(t, snap, ClaudeWindowFiveHour)
	if five.UsedPercent != 10 || five.Source != ClaudeQuotaSourcePoll || five.Status != "" {
		t.Fatalf("five_hour = %+v", five)
	}
	if want := time.Date(2026, 10, 21, 10, 29, 59, 551785000, time.UTC); !five.ResetsAt.Equal(want) {
		t.Fatalf("five_hour reset = %v, want %v", five.ResetsAt, want)
	}
	if week := mustWindow(t, snap, ClaudeWindowSevenDay); week.UsedPercent != 56 {
		t.Fatalf("seven_day = %+v", week)
	}
	if fable := mustWindow(t, snap, ClaudeWindowSevenDayFable); fable.UsedPercent != 47 {
		t.Fatalf("seven_day_fable = %+v", fable)
	}
}

func TestClaudeQuotaObserveUsage_Fable5DisplayNameAndNullReset(t *testing.T) {
	tracker, _ := newTestClaudeQuotaTracker(liveFixtureNow)
	tracker.ObserveUsage("a", loadClaudeUsageFixture(t, "claude_usage_fable5_null_reset.json"), liveFixtureNow)
	snap := mustSnapshot(t, tracker, "a")
	fable := mustWindow(t, snap, ClaudeWindowSevenDayFable)
	if fable.UsedPercent != 2 || !fable.ResetsAt.IsZero() {
		t.Fatalf("seven_day_fable = %+v, want 2%% with no reset (Opus scoped limit ignored)", fable)
	}
	if five := mustWindow(t, snap, ClaudeWindowFiveHour); !five.ResetsAt.IsZero() || five.UsedPercent != 0 {
		t.Fatalf("five_hour = %+v", five)
	}
}

func TestClaudeQuotaObserveUsage_PrefersActiveFableLimit(t *testing.T) {
	raw := `{"limits":[
		{"kind":"weekly_scoped","percent":10,"scope":{"model":{"display_name":"Fable"}},"is_active":false},
		{"kind":"weekly_scoped","percent":20,"scope":{"model":{"display_name":"Fable 5.1"}},"is_active":true}]}`
	var u ClaudeUsageResponse
	if errUnmarshal := json.Unmarshal([]byte(raw), &u); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	tracker, _ := newTestClaudeQuotaTracker(liveFixtureNow)
	tracker.ObserveUsage("a", u, liveFixtureNow)
	if fable := mustWindow(t, mustSnapshot(t, tracker, "a"), ClaudeWindowSevenDayFable); fable.UsedPercent != 20 {
		t.Fatalf("fable used = %v, want active entry 20", fable.UsedPercent)
	}
}

func TestClaudeQuotaMerge_NewerWinsAcrossSources(t *testing.T) {
	tracker, _ := newTestClaudeQuotaTracker(liveFixtureNow)
	tracker.ObserveHeaders("a", "claude-fable-5-1", loadClaudeHeaderFixture(t, "claude_headers_fable.txt"), liveFixtureNow)
	// An older poll must not overwrite newer header data.
	tracker.ObserveUsage("a", loadClaudeUsageFixture(t, "claude_usage_live.json"), liveFixtureNow.Add(-time.Minute))
	if five := mustWindow(t, mustSnapshot(t, tracker, "a"), ClaudeWindowFiveHour); five.Source != ClaudeQuotaSourceHeaders {
		t.Fatalf("older poll replaced newer headers: %+v", five)
	}
	// A newer poll wins.
	tracker.ObserveUsage("a", loadClaudeUsageFixture(t, "claude_usage_live.json"), liveFixtureNow.Add(time.Minute))
	if five := mustWindow(t, mustSnapshot(t, tracker, "a"), ClaudeWindowFiveHour); five.Source != ClaudeQuotaSourcePoll {
		t.Fatalf("newer poll did not win: %+v", five)
	}
}

func TestClaudeQuotaMerge_OpusHeadersKeepFable(t *testing.T) {
	tracker, _ := newTestClaudeQuotaTracker(liveFixtureNow)
	tracker.ObserveHeaders("a", "claude-fable-5-1", loadClaudeHeaderFixture(t, "claude_headers_fable.txt"), liveFixtureNow)
	tracker.ObserveHeaders("a", "claude-opus", loadClaudeHeaderFixture(t, "claude_headers_opus.txt"), liveFixtureNow.Add(time.Minute))
	snap := mustSnapshot(t, tracker, "a")
	if fable := mustWindow(t, snap, ClaudeWindowSevenDayFable); fable.UsedPercent != 46 || !fable.ObservedAt.Equal(liveFixtureNow) {
		t.Fatalf("opus headers changed fable window: %+v", fable)
	}
	if five := mustWindow(t, snap, ClaudeWindowFiveHour); five.UsedPercent != 12 {
		t.Fatalf("five_hour not updated by opus headers: %+v", five)
	}
}

func TestClaudeQuotaSnapshot_WindowExpiryWithFakeClock(t *testing.T) {
	tracker, clock := newTestClaudeQuotaTracker(liveFixtureNow)
	tracker.ObserveHeaders("a", "claude-fable-5-1", loadClaudeHeaderFixture(t, "claude_headers_fable.txt"), liveFixtureNow)

	clock.Advance(90*time.Minute + time.Second) // past the 10:30 5h reset
	snap := mustSnapshot(t, tracker, "a")
	five := mustWindow(t, snap, ClaudeWindowFiveHour)
	if five.UsedPercent != 0 || !five.ResetsAt.IsZero() || five.Status != "" {
		t.Fatalf("expired five_hour = %+v, want fresh", five)
	}
	if week := mustWindow(t, snap, ClaudeWindowSevenDay); week.UsedPercent != 55 {
		t.Fatalf("seven_day expired early: %+v", week)
	}
}

func TestClaudeQuotaSnapshot_IsDeepCopy(t *testing.T) {
	tracker, _ := newTestClaudeQuotaTracker(liveFixtureNow)
	tracker.ObserveHeaders("a", "m", loadClaudeHeaderFixture(t, "claude_headers_opus.txt"), liveFixtureNow)
	tracker.ObservePlan("a", ClaudeQuotaPlan{Label: "Max 5x"})
	snap := mustSnapshot(t, tracker, "a")
	snap.Windows[ClaudeWindowFiveHour] = ClaudeQuotaWindow{UsedPercent: 99}
	snap.Plan.Label = "mutated"
	again := mustSnapshot(t, tracker, "a")
	if again.Windows[ClaudeWindowFiveHour].UsedPercent == 99 || again.Plan.Label != "Max 5x" {
		t.Fatal("snapshot shares state with tracker")
	}
}

func TestClaudeQuotaPollBookkeepingAndForget(t *testing.T) {
	tracker, _ := newTestClaudeQuotaTracker(liveFixtureNow)
	tracker.ObservePollResult("a", liveFixtureNow, time.Time{}, nil, time.Time{})
	if snap := mustSnapshot(t, tracker, "a"); !snap.PollStartedAt.Equal(liveFixtureNow) {
		t.Fatalf("poll_started_at = %v", snap.PollStartedAt)
	}
	next := liveFixtureNow.Add(10 * time.Minute)
	tracker.ObservePollResult("a", liveFixtureNow, liveFixtureNow.Add(time.Second), errTestPoll("unauthorized"), next)
	snap := mustSnapshot(t, tracker, "a")
	if !snap.PollStartedAt.IsZero() || snap.PollError != "unauthorized" || !snap.NextPollAt.Equal(next) || !snap.LastPollAt.IsZero() {
		t.Fatalf("after error: %+v", snap)
	}
	tracker.ObservePollResult("a", liveFixtureNow, liveFixtureNow.Add(2*time.Second), nil, next)
	if snap = mustSnapshot(t, tracker, "a"); snap.PollError != "" || !snap.LastPollAt.Equal(liveFixtureNow.Add(2*time.Second)) {
		t.Fatalf("after success: %+v", snap)
	}
	tracker.Forget("a")
	if _, ok := tracker.Snapshot("a"); ok {
		t.Fatal("Forget kept state")
	}
}

type errTestPoll string

func (e errTestPoll) Error() string { return string(e) }

func TestClaudeQuotaRequestPollNeverBlocks(t *testing.T) {
	tracker := NewClaudeQuotaTracker()
	for i := 0; i < claudeQuotaPollQueueSize*2; i++ {
		tracker.RequestPoll("a")
	}
	if got := len(tracker.PollRequests()); got != claudeQuotaPollQueueSize {
		t.Fatalf("queued = %d", got)
	}
}

func registerClaudeQuotaTestAuth(t *testing.T, m *Manager, id string) {
	t.Helper()
	if _, errRegister := m.Register(context.Background(), &Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": "fake-" + id}}); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
}

func TestManagerMarkResult_FeedsClaudeQuotaTracker(t *testing.T) {
	m := NewManager(nil, nil, nil)
	tracker := NewClaudeQuotaTracker()
	m.SetClaudeQuotaTracker(tracker)
	if m.ClaudeQuotaTracker() != tracker {
		t.Fatal("tracker getter mismatch")
	}
	registerClaudeQuotaTestAuth(t, m, "claude-feed")

	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, loadClaudeHeaderFixture(t, "claude_headers_fable.txt"))
	m.MarkResult(ctx, Result{AuthID: "claude-feed", Provider: "claude", Model: "claude-fable-5-1", Success: true})

	snap, ok := tracker.Snapshot("claude-feed")
	if !ok || len(snap.Windows) != 3 {
		t.Fatalf("tracker not fed: ok=%v snap=%+v", ok, snap)
	}

	m.Remove(context.Background(), "claude-feed")
	if _, ok = tracker.Snapshot("claude-feed"); ok {
		t.Fatal("Remove did not forget tracker state")
	}
}

func TestManagerMarkResult_ClaudeQuotaHonorsSkipQuotaObservation(t *testing.T) {
	m := NewManager(nil, nil, nil)
	tracker := NewClaudeQuotaTracker()
	m.SetClaudeQuotaTracker(tracker)
	registerClaudeQuotaTestAuth(t, m, "claude-skip")

	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, loadClaudeHeaderFixture(t, "claude_headers_fable.txt"))
	m.MarkResult(ctx, Result{AuthID: "claude-skip", Provider: "claude", Model: "claude-fable-5-1", Success: true, SkipQuotaObservation: true})
	if _, ok := tracker.Snapshot("claude-skip"); ok {
		t.Fatal("SkipQuotaObservation result reached the tracker")
	}

	// Non-Claude providers never feed the tracker.
	m.MarkResult(ctx, Result{AuthID: "claude-skip", Provider: "codex", Model: "gpt", Success: true})
	if _, ok := tracker.Snapshot("claude-skip"); ok {
		t.Fatal("non-claude result reached the tracker")
	}
}

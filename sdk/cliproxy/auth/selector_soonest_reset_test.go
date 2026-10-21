package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var soonestNow = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

func claudeOAuthTestAuth(id string) *Auth {
	return &Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": "fake-" + id}}
}

// unifiedHeaders builds a unified rate-limit header set. Pass reset <= 0 to omit a window.
func unifiedHeaders(fiveUsed float64, fiveReset time.Time, fiveStatus string, weekUsed float64, weekReset time.Time, fableUsed float64, fableReset time.Time) http.Header {
	h := http.Header{}
	set := func(prefix string, used float64, reset time.Time, status string) {
		if reset.IsZero() {
			return
		}
		h.Set("Anthropic-Ratelimit-Unified-"+prefix+"-Utilization", strconv.FormatFloat(used/100, 'f', -1, 64))
		h.Set("Anthropic-Ratelimit-Unified-"+prefix+"-Reset", strconv.FormatInt(reset.Unix(), 10))
		if status == "" {
			status = "allowed"
		}
		h.Set("Anthropic-Ratelimit-Unified-"+prefix+"-Status", status)
	}
	set("5h", fiveUsed, fiveReset, fiveStatus)
	set("7d", weekUsed, weekReset, "")
	set("7d_oi", fableUsed, fableReset, "")
	return h
}

func newSoonestFixture(t *testing.T) (*ClaudeQuotaTracker, *fakeClock, *SoonestResetSelector) {
	t.Helper()
	tracker, clock := newTestClaudeQuotaTracker(soonestNow)
	return tracker, clock, NewSoonestResetSelector(tracker, SoonestResetConfig{})
}

func pickID(t *testing.T, s *SoonestResetSelector, model string, auths ...*Auth) string {
	t.Helper()
	got, err := s.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	return got.ID
}

func rankIDs(ranks []ClaudeQuotaRank) []string {
	out := make([]string, len(ranks))
	for i, r := range ranks {
		out[i] = r.AuthID
	}
	return out
}

func assertOrder(t *testing.T, ranks []ClaudeQuotaRank, want ...string) {
	t.Helper()
	got := rankIDs(ranks)
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestSoonestReset_OrdersByFableResetWithFallbacks(t *testing.T) {
	tracker, _, sel := newSoonestFixture(t)
	h := soonestNow.Add(2 * time.Hour)
	// a: Fable resets in 3 days, overall 7d in 1 day -> Fable key wins (3d).
	tracker.ObserveHeaders("a", "", unifiedHeaders(10, h, "", 20, soonestNow.Add(24*time.Hour), 30, soonestNow.Add(72*time.Hour)), soonestNow)
	// b: no Fable window -> overall 7d key (2d).
	tracker.ObserveHeaders("b", "", unifiedHeaders(10, h, "", 20, soonestNow.Add(48*time.Hour), 0, time.Time{}), soonestNow)
	// c: only 5h known -> fresh key (now + 7d).
	tracker.ObserveHeaders("c", "", unifiedHeaders(10, h, "", 0, time.Time{}, 0, time.Time{}), soonestNow)

	ranks := RankClaudeQuota([]*Auth{claudeOAuthTestAuth("c"), claudeOAuthTestAuth("a"), claudeOAuthTestAuth("b")}, "claude-opus", tracker, SoonestResetConfig{}, soonestNow)
	assertOrder(t, ranks, "b", "a", "c")
	wantKeys := map[string]string{"a": ClaudeQuotaKeyFable, "b": ClaudeQuotaKeyWeek, "c": ClaudeQuotaKeyFresh}
	for _, r := range ranks {
		if r.State != ClaudeQuotaRankEligible || r.KeySource != wantKeys[r.AuthID] {
			t.Fatalf("rank %+v, want eligible key %s", r, wantKeys[r.AuthID])
		}
	}
	if got := pickID(t, sel, "claude-opus", claudeOAuthTestAuth("a"), claudeOAuthTestAuth("b"), claudeOAuthTestAuth("c")); got != "b" {
		t.Fatalf("Pick = %s, want b", got)
	}
}

func TestSoonestReset_SkipsAtFiveHourThresholdAndRejected(t *testing.T) {
	tracker, _, _ := newSoonestFixture(t)
	fiveReset := soonestNow.Add(time.Hour)
	tracker.ObserveHeaders("a", "", unifiedHeaders(100, fiveReset, "", 10, soonestNow.Add(24*time.Hour), 0, time.Time{}), soonestNow)
	tracker.ObserveHeaders("b", "", unifiedHeaders(50, fiveReset.Add(time.Hour), "rejected", 10, soonestNow.Add(25*time.Hour), 0, time.Time{}), soonestNow)
	tracker.ObserveHeaders("c", "", unifiedHeaders(85, fiveReset, "", 10, soonestNow.Add(96*time.Hour), 0, time.Time{}), soonestNow)
	auths := []*Auth{claudeOAuthTestAuth("a"), claudeOAuthTestAuth("b"), claudeOAuthTestAuth("c")}

	ranks := RankClaudeQuota(auths, "claude-opus", tracker, SoonestResetConfig{}, soonestNow)
	assertOrder(t, ranks, "c", "a", "b")
	if ranks[1].State != ClaudeQuotaRankSkipped5h || ranks[2].State != ClaudeQuotaRankSkipped5h {
		t.Fatalf("states = %+v", ranks)
	}

	// A lower threshold skips c as well; skipped ones order by unblock time.
	ranks = RankClaudeQuota(auths, "claude-opus", tracker, SoonestResetConfig{SkipFiveHourAtPercent: 80}, soonestNow)
	assertOrder(t, ranks, "a", "c", "b")
	for _, r := range ranks {
		if r.State != ClaudeQuotaRankSkipped5h {
			t.Fatalf("rank %+v, want skipped_5h", r)
		}
	}
}

func TestSoonestReset_SkipsWeeklyAt100(t *testing.T) {
	tracker, _, _ := newSoonestFixture(t)
	tracker.ObserveHeaders("a", "", unifiedHeaders(10, soonestNow.Add(time.Hour), "", 100, soonestNow.Add(time.Hour), 0, time.Time{}), soonestNow)
	tracker.ObserveHeaders("b", "", unifiedHeaders(10, soonestNow.Add(time.Hour), "", 99, soonestNow.Add(48*time.Hour), 0, time.Time{}), soonestNow)
	ranks := RankClaudeQuota([]*Auth{claudeOAuthTestAuth("a"), claudeOAuthTestAuth("b")}, "", tracker, SoonestResetConfig{}, soonestNow)
	assertOrder(t, ranks, "b", "a")
	if ranks[1].State != ClaudeQuotaRankSkipped7d {
		t.Fatalf("a state = %s", ranks[1].State)
	}
}

func TestSoonestReset_FableSkipOnlyForFableModels(t *testing.T) {
	tracker, _, sel := newSoonestFixture(t)
	five := soonestNow.Add(time.Hour)
	tracker.ObserveHeaders("a", "", unifiedHeaders(10, five, "", 50, soonestNow.Add(24*time.Hour), 100, soonestNow.Add(24*time.Hour)), soonestNow)
	tracker.ObserveHeaders("b", "", unifiedHeaders(10, five, "", 50, soonestNow.Add(48*time.Hour), 40, soonestNow.Add(48*time.Hour)), soonestNow)
	a, b := claudeOAuthTestAuth("a"), claudeOAuthTestAuth("b")

	if got := pickID(t, sel, "claude-opus-4", a, b); got != "a" {
		t.Fatalf("non-Fable model pick = %s, want a (Fable exhaustion ignored)", got)
	}
	if got := pickID(t, sel, "claude-fable-5-1(high)", a, b); got != "b" {
		t.Fatalf("Fable model pick = %s, want b", got)
	}
	ranks := RankClaudeQuota([]*Auth{a, b}, "claude-fable-5-1", tracker, SoonestResetConfig{}, soonestNow)
	if ranks[1].AuthID != "a" || ranks[1].State != ClaudeQuotaRankSkipped7dFable {
		t.Fatalf("ranks = %+v", ranks)
	}
}

func TestSoonestReset_UnknownAfterKnownAndRequestsPoll(t *testing.T) {
	tracker, _, sel := newSoonestFixture(t)
	tracker.ObserveHeaders("z", "", unifiedHeaders(10, soonestNow.Add(time.Hour), "", 10, soonestNow.Add(96*time.Hour), 0, time.Time{}), soonestNow)
	ranks := RankClaudeQuota([]*Auth{claudeOAuthTestAuth("b"), claudeOAuthTestAuth("z"), claudeOAuthTestAuth("a")}, "", tracker, SoonestResetConfig{}, soonestNow)
	assertOrder(t, ranks, "z", "a", "b")
	if ranks[1].State != ClaudeQuotaRankUnknown {
		t.Fatalf("a state = %s", ranks[1].State)
	}
	if len(tracker.PollRequests()) != 0 {
		t.Fatal("ranking alone must not request polls")
	}

	// Only unknown candidates: the pick is unknown and requests a poll for it.
	if got := pickID(t, sel, "", claudeOAuthTestAuth("b"), claudeOAuthTestAuth("a")); got != "a" {
		t.Fatalf("Pick = %s, want a", got)
	}
	select {
	case id := <-tracker.PollRequests():
		if id != "a" {
			t.Fatalf("poll requested for %s, want a", id)
		}
	default:
		t.Fatal("unknown pick did not request a poll")
	}
	// A known pick does not request a poll.
	pickID(t, sel, "", claudeOAuthTestAuth("z"), claudeOAuthTestAuth("a"))
	if len(tracker.PollRequests()) != 0 {
		t.Fatal("known pick requested a poll")
	}
}

func TestSoonestReset_AllSkippedStillReturnsSoonest(t *testing.T) {
	tracker, _, sel := newSoonestFixture(t)
	tracker.ObserveHeaders("a", "", unifiedHeaders(100, soonestNow.Add(3*time.Hour), "rejected", 10, soonestNow.Add(24*time.Hour), 0, time.Time{}), soonestNow)
	tracker.ObserveHeaders("b", "", unifiedHeaders(100, soonestNow.Add(time.Hour), "rejected", 10, soonestNow.Add(48*time.Hour), 0, time.Time{}), soonestNow)
	if got := pickID(t, sel, "", claudeOAuthTestAuth("a"), claudeOAuthTestAuth("b")); got != "b" {
		t.Fatalf("Pick = %s, want b (unblocks first)", got)
	}
}

func TestSoonestReset_ExpiredWindowsCountAsReset(t *testing.T) {
	tracker, clock, sel := newSoonestFixture(t)
	tracker.ObserveHeaders("a", "", unifiedHeaders(100, soonestNow.Add(time.Hour), "rejected", 10, soonestNow.Add(24*time.Hour), 0, time.Time{}), soonestNow)
	tracker.ObserveHeaders("b", "", unifiedHeaders(10, soonestNow.Add(time.Hour), "", 10, soonestNow.Add(48*time.Hour), 0, time.Time{}), soonestNow)
	if got := pickID(t, sel, "", claudeOAuthTestAuth("a"), claudeOAuthTestAuth("b")); got != "b" {
		t.Fatalf("Pick = %s, want b", got)
	}
	clock.Advance(time.Hour) // a's 5h window resets
	if got := pickID(t, sel, "", claudeOAuthTestAuth("a"), claudeOAuthTestAuth("b")); got != "a" {
		t.Fatalf("Pick after 5h reset = %s, want a", got)
	}
}

func TestSoonestReset_TieWindowAndTieBreaks(t *testing.T) {
	reset := soonestNow.Add(24 * time.Hour)
	five := soonestNow.Add(time.Hour)
	t.Run("weekly remaining", func(t *testing.T) {
		tracker, _, _ := newSoonestFixture(t)
		// b resets 30s later but has more weekly quota left: within the 1 min tie window.
		tracker.ObserveHeaders("a", "", unifiedHeaders(10, five, "", 60, reset, 0, time.Time{}), soonestNow)
		tracker.ObserveHeaders("b", "", unifiedHeaders(10, five, "", 40, reset.Add(30*time.Second), 0, time.Time{}), soonestNow)
		assertOrder(t, RankClaudeQuota([]*Auth{claudeOAuthTestAuth("a"), claudeOAuthTestAuth("b")}, "", tracker, SoonestResetConfig{}, soonestNow), "b", "a")
	})
	t.Run("outside tie window", func(t *testing.T) {
		tracker, _, _ := newSoonestFixture(t)
		tracker.ObserveHeaders("a", "", unifiedHeaders(10, five, "", 60, reset, 0, time.Time{}), soonestNow)
		tracker.ObserveHeaders("b", "", unifiedHeaders(10, five, "", 40, reset.Add(2*time.Minute), 0, time.Time{}), soonestNow)
		assertOrder(t, RankClaudeQuota([]*Auth{claudeOAuthTestAuth("b"), claudeOAuthTestAuth("a")}, "", tracker, SoonestResetConfig{}, soonestNow), "a", "b")
	})
	t.Run("five hour used", func(t *testing.T) {
		tracker, _, _ := newSoonestFixture(t)
		tracker.ObserveHeaders("a", "", unifiedHeaders(30, five, "", 50, reset, 0, time.Time{}), soonestNow)
		tracker.ObserveHeaders("b", "", unifiedHeaders(20, five, "", 50, reset, 0, time.Time{}), soonestNow)
		assertOrder(t, RankClaudeQuota([]*Auth{claudeOAuthTestAuth("a"), claudeOAuthTestAuth("b")}, "", tracker, SoonestResetConfig{}, soonestNow), "b", "a")
	})
	t.Run("auth id", func(t *testing.T) {
		tracker, _, _ := newSoonestFixture(t)
		tracker.ObserveHeaders("a", "", unifiedHeaders(20, five, "", 50, reset, 0, time.Time{}), soonestNow)
		tracker.ObserveHeaders("b", "", unifiedHeaders(20, five, "", 50, reset, 0, time.Time{}), soonestNow)
		assertOrder(t, RankClaudeQuota([]*Auth{claudeOAuthTestAuth("b"), claudeOAuthTestAuth("a")}, "", tracker, SoonestResetConfig{}, soonestNow), "a", "b")
	})
}

func TestSoonestReset_MixedNonClaudeAuthsAfterKnown(t *testing.T) {
	tracker, _, sel := newSoonestFixture(t)
	tracker.ObserveHeaders("z-claude", "", unifiedHeaders(10, soonestNow.Add(time.Hour), "", 10, soonestNow.Add(24*time.Hour), 0, time.Time{}), soonestNow)
	codex := &Auth{ID: "a-codex", Provider: "codex", Metadata: map[string]any{"access_token": "fake"}}
	apiKey := &Auth{ID: "a-claude-key", Provider: "claude", Attributes: map[string]string{"api_key": "fake"}}
	// Headers recorded for an API-key auth are ignored: it is not an OAuth subscription account.
	tracker.ObserveHeaders("a-claude-key", "", unifiedHeaders(10, soonestNow.Add(time.Hour), "", 10, soonestNow.Add(time.Hour), 0, time.Time{}), soonestNow)

	ranks := RankClaudeQuota([]*Auth{codex, apiKey, claudeOAuthTestAuth("z-claude")}, "", tracker, SoonestResetConfig{}, soonestNow)
	assertOrder(t, ranks, "z-claude", "a-claude-key", "a-codex")
	got, err := sel.Pick(context.Background(), "mixed", "", cliproxyexecutor.Options{}, []*Auth{codex, apiKey, claudeOAuthTestAuth("z-claude")})
	if err != nil || got.ID != "z-claude" {
		t.Fatalf("Pick = %v, %v", got, err)
	}
	if len(tracker.PollRequests()) != 0 {
		t.Fatal("non-OAuth candidates must not request polls")
	}
}

func TestSoonestReset_NeverReturnsCooledAuths(t *testing.T) {
	tracker, _, sel := newSoonestFixture(t)
	tracker.ObserveHeaders("a", "", unifiedHeaders(10, soonestNow.Add(time.Hour), "", 10, soonestNow.Add(time.Hour), 0, time.Time{}), soonestNow)
	tracker.ObserveHeaders("b", "", unifiedHeaders(10, soonestNow.Add(time.Hour), "", 10, soonestNow.Add(96*time.Hour), 0, time.Time{}), soonestNow)
	cooled := claudeOAuthTestAuth("a")
	cooled.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: soonestNow.Add(30 * time.Minute)}
	if got := pickID(t, sel, "claude-opus", cooled, claudeOAuthTestAuth("b")); got != "b" {
		t.Fatalf("Pick = %s, want b (a cooling down)", got)
	}
	_, err := sel.Pick(context.Background(), "claude", "claude-opus", cliproxyexecutor.Options{}, []*Auth{cooled})
	if err == nil {
		t.Fatal("all-cooled pick returned no error")
	}
	if _, ok := err.(*modelCooldownError); !ok {
		t.Fatalf("err = %T %v, want modelCooldownError", err, err)
	}
}

func TestSoonestReset_NilTrackerFallsBackToIDOrder(t *testing.T) {
	sel := NewSoonestResetSelector(nil, SoonestResetConfig{})
	got, err := sel.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{claudeOAuthTestAuth("b"), claudeOAuthTestAuth("a")})
	if err != nil || got.ID != "a" {
		t.Fatalf("Pick = %v, %v", got, err)
	}
}

func TestSoonestResetSelectorIsCustom(t *testing.T) {
	sel := NewSoonestResetSelector(nil, SoonestResetConfig{})
	if isBuiltInSelector(sel) || selectorStrategy(sel) != schedulerStrategyCustom {
		t.Fatal("soonest-reset must stay a custom selector (no scheduler fast path)")
	}
}

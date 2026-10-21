package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

const soonestAffinityModel = "claude-soonest-affinity-test"

type soonestAffinityFixture struct {
	manager  *Manager
	tracker  *ClaudeQuotaTracker
	affinity *SessionAffinitySelector
	now      time.Time
	ids      map[string]string // short name -> auth ID
}

// newSoonestAffinityFixture wires SessionAffinity(Fallback: SoonestReset, RebindOn: quota)
// into a Manager. The tracker clock is frozen at the real start time so manager
// cooldowns (wall clock) and quota windows (tracker clock) agree.
func newSoonestAffinityFixture(t *testing.T, names ...string) *soonestAffinityFixture {
	t.Helper()
	withQuotaCooldownEnabled(t)
	now := time.Now().UTC().Truncate(time.Second)
	tracker := NewClaudeQuotaTracker()
	tracker.SetNowFunc(func() time.Time { return now })
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: NewSoonestResetSelector(tracker, SoonestResetConfig{}),
		TTL:      time.Hour,
		RebindOn: SessionAffinityRebindOnQuota,
	})
	t.Cleanup(affinity.Stop)
	manager := NewManager(nil, affinity, nil)
	manager.SetClaudeQuotaTracker(tracker)
	manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "claude"})
	f := &soonestAffinityFixture{manager: manager, tracker: tracker, affinity: affinity, now: now, ids: map[string]string{}}
	for _, name := range names {
		id := name + "-" + t.Name()
		f.ids[name] = id
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: soonestAffinityModel}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), claudeOAuthTestAuth(id)); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	return f
}

// weekly sets the account's 5h and weekly windows, observed at the frozen clock.
func (f *soonestAffinityFixture) weekly(name string, resetIn time.Duration, at time.Time) {
	f.tracker.ObserveHeaders(f.ids[name], "", unifiedHeaders(10, f.now.Add(time.Hour), "", 20, f.now.Add(resetIn), 0, time.Time{}), at)
}

func (f *soonestAffinityFixture) pick(t *testing.T, session string) string {
	t.Helper()
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": {session}}}
	auth, errSelect := f.manager.SelectAuth(context.Background(), "claude", soonestAffinityModel, opts)
	if errSelect != nil {
		t.Fatalf("SelectAuth(%s): %v", session, errSelect)
	}
	for name, id := range f.ids {
		if id == auth.ID {
			return name
		}
	}
	t.Fatalf("unknown auth %s", auth.ID)
	return ""
}

func (f *soonestAffinityFixture) boundTo(t *testing.T, session string) string {
	t.Helper()
	// Claude Code session headers are namespaced with the client type ("claude:<id>").
	key := "claude::" + cliproxysession.BoundSessionIdentity("claude:"+session) + "::" + soonestAffinityModel
	id, ok := f.affinity.cache.Get(key)
	if !ok {
		return ""
	}
	for name, authID := range f.ids {
		if authID == id {
			return name
		}
	}
	return id
}

func TestSoonestResetWithSessionAffinity(t *testing.T) {
	f := newSoonestAffinityFixture(t, "a", "b", "c")
	// Manual observations are stamped before the frozen clock: MarkResult stamps
	// header observations with the wall clock, and the newer observation wins.
	f.weekly("a", 48*time.Hour, f.now.Add(-3*time.Second))
	f.weekly("b", 24*time.Hour, f.now.Add(-3*time.Second))
	f.weekly("c", 72*time.Hour, f.now.Add(-3*time.Second))

	// A cold session binds rank 1.
	if got := f.pick(t, "s1"); got != "b" {
		t.Fatalf("cold s1 = %s, want b", got)
	}

	// Ranks change: c now resets soonest. The bound session stays; a new one takes the new rank 1.
	f.weekly("c", 12*time.Hour, f.now.Add(-2*time.Second))
	if got := f.pick(t, "s1"); got != "b" {
		t.Fatalf("bound s1 = %s after rank change, want b", got)
	}
	if got := f.pick(t, "s2"); got != "c" {
		t.Fatalf("new s2 = %s, want c", got)
	}

	// A tracked 5h of 100 % on a bound account does not move its session.
	f.tracker.ObserveHeaders(f.ids["b"], "", unifiedHeaders(100, f.now.Add(time.Hour), "", 20, f.now.Add(24*time.Hour), 0, time.Time{}), f.now.Add(-time.Second))
	if got := f.pick(t, "s1"); got != "b" {
		t.Fatalf("s1 moved on tracked 5h exhaustion: %s", got)
	}

	// A real 429 with unified 5h rejected headers cools b; s1 rebinds to the next rank.
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	rejected := unifiedHeaders(100, f.now.Add(time.Hour), "rejected", 20, f.now.Add(24*time.Hour), 0, time.Time{})
	internallogging.SetResponseHeaders(ctx, rejected)
	retryAfter := time.Hour
	f.manager.MarkResult(ctx, Result{
		AuthID: f.ids["b"], Provider: "claude", Model: soonestAffinityModel, CredentialScope: true, RetryAfter: &retryAfter,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "5h limit rejected"},
	})
	if snap, _ := f.tracker.Snapshot(f.ids["b"]); snap.Windows[ClaudeWindowFiveHour].Status != "rejected" {
		t.Fatalf("tracker missed rejected status: %+v", snap.Windows)
	}
	if got := f.pick(t, "s1"); got != "c" {
		t.Fatalf("s1 after 429 = %s, want c (next rank)", got)
	}
	if got := f.boundTo(t, "s1"); got != "c" {
		t.Fatalf("s1 bound to %s after 429, want c", got)
	}

	// A 5xx on the bound auth keeps the binding (rebind-on: quota).
	f.manager.MarkResult(context.Background(), Result{
		AuthID: f.ids["c"], Provider: "claude", Model: soonestAffinityModel,
		Error: &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream 502"},
	})
	f.pick(t, "s2") // may fail over for this request while c is transiently blocked
	if got := f.boundTo(t, "s2"); got != "c" {
		t.Fatalf("s2 bound to %s after 5xx, want c", got)
	}
}

package test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const quotaRoutingModel = "claude-quota-routing-test"

// quotaStatusError mimics the Claude executor's credential-scoped 429.
type quotaStatusError struct {
	status     int
	retryAfter *time.Duration
	scoped     bool
}

func (e *quotaStatusError) Error() string               { return "upstream status " + strconv.Itoa(e.status) }
func (e *quotaStatusError) StatusCode() int             { return e.status }
func (e *quotaStatusError) RetryAfter() *time.Duration  { return e.retryAfter }
func (e *quotaStatusError) IsCredentialScoped() bool    { return e.scoped }
func (e *quotaStatusError) IsUpstreamStatusError() bool { return true }

// quotaRoutingExecutor is a fake Claude executor. Every response carries unified
// rate-limit headers for the serving account, so routing is fed passively.
type quotaRoutingExecutor struct {
	mu       sync.Mutex
	now      time.Time
	resets   map[string]time.Duration // auth ID -> weekly reset offset
	behavior map[string]int           // auth ID -> HTTP status to fail with (0 = success)
	served   []string
}

func (e *quotaRoutingExecutor) headers(authID string, rejected bool) http.Header {
	five, status := "0.1", "allowed"
	if rejected {
		five, status = "1.0", "rejected"
	}
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", five)
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(e.now.Add(time.Hour).Unix(), 10))
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", status)
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.2")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(e.now.Add(e.resets[authID]).Unix(), 10))
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	return h
}

func (e *quotaRoutingExecutor) Identifier() string { return "claude" }

func (e *quotaRoutingExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.served = append(e.served, auth.ID)
	switch status := e.behavior[auth.ID]; status {
	case 0:
		internallogging.SetResponseHeaders(ctx, e.headers(auth.ID, false))
		return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
	case http.StatusTooManyRequests:
		internallogging.SetResponseHeaders(ctx, e.headers(auth.ID, true))
		retryAfter := time.Hour
		return cliproxyexecutor.Response{}, &quotaStatusError{status: status, retryAfter: &retryAfter, scoped: true}
	default:
		return cliproxyexecutor.Response{}, &quotaStatusError{status: status}
	}
}

func (e *quotaRoutingExecutor) ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, fmt.Errorf("stream not supported")
}

func (e *quotaRoutingExecutor) Refresh(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

func (e *quotaRoutingExecutor) CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, fmt.Errorf("count tokens not supported")
}

func (e *quotaRoutingExecutor) HttpRequest(context.Context, *cliproxyauth.Auth, *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("http request not supported")
}

func (e *quotaRoutingExecutor) set(authID string, reset time.Duration, status int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if reset > 0 {
		e.resets[authID] = reset
	}
	e.behavior[authID] = status
}

func (e *quotaRoutingExecutor) takeServed() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.served
	e.served = nil
	return out
}

// TestClaudeQuotaRoutingThroughManager runs soonest-reset under session affinity
// (rebind-on: quota) through Manager.Execute with fake Claude accounts.
func TestClaudeQuotaRoutingThroughManager(t *testing.T) {
	cliproxyauth.SetTransientErrorCooldownSeconds(-1)
	t.Cleanup(func() { cliproxyauth.SetTransientErrorCooldownSeconds(0) })

	now := time.Now().UTC().Truncate(time.Second)
	tracker := cliproxyauth.NewClaudeQuotaTracker()
	tracker.SetNowFunc(func() time.Time { return now })
	affinity := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{
		Fallback: cliproxyauth.NewSoonestResetSelector(tracker, cliproxyauth.SoonestResetConfig{}),
		TTL:      time.Hour,
		RebindOn: cliproxyauth.SessionAffinityRebindOnQuota,
	})
	t.Cleanup(affinity.Stop)
	manager := cliproxyauth.NewManager(nil, affinity, nil)
	manager.SetClaudeQuotaTracker(tracker)
	manager.SetRetryConfig(0, 0, 0)

	executor := &quotaRoutingExecutor{now: now, resets: map[string]time.Duration{}, behavior: map[string]int{}}
	manager.RegisterExecutor(executor)

	ids := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		id := "quota-routing-" + name
		ids[name] = id
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: quotaRoutingModel}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
			ID: id, Provider: "claude", Status: cliproxyauth.StatusActive,
			Metadata: map[string]any{"access_token": "fake-" + name, "email": name + "@example.invalid"},
		}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	// Seed the order with a poll-like observation: b resets first, then a, then c.
	seed := func(name string, reset time.Duration, at time.Time) {
		used, resetAt := 20.0, now.Add(reset).Format(time.RFC3339)
		tracker.ObserveUsage(ids[name], cliproxyauth.ClaudeUsageResponse{
			SevenDay: &cliproxyauth.ClaudeUsageWindow{Utilization: &used, ResetsAt: &resetAt},
		}, at)
	}
	executor.set(ids["a"], 48*time.Hour, 0)
	executor.set(ids["b"], 24*time.Hour, 0)
	executor.set(ids["c"], 72*time.Hour, 0)
	seed("a", 48*time.Hour, now.Add(-time.Minute))
	seed("b", 24*time.Hour, now.Add(-time.Minute))
	seed("c", 72*time.Hour, now.Add(-time.Minute))

	run := func(session string) (string, error) {
		t.Helper()
		resp, errExec := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: quotaRoutingModel},
			cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": {session}}})
		return string(resp.Payload), errExec
	}
	mustServe := func(session, wantName string) {
		t.Helper()
		got, errRun := run(session)
		if errRun != nil {
			t.Fatalf("%s: %v", session, errRun)
		}
		if got != ids[wantName] {
			t.Fatalf("%s served by %s, want %s", session, got, ids[wantName])
		}
	}

	// Cold session binds rank 1 (b).
	mustServe("s1", "b")
	if snap, ok := tracker.Snapshot(ids["b"]); !ok || snap.Windows[cliproxyauth.ClaudeWindowFiveHour].Source != cliproxyauth.ClaudeQuotaSourceHeaders {
		t.Fatalf("response headers did not reach the tracker: %+v", snap)
	}

	// c now resets soonest (seen in its own response headers on the next serve); s1 stays on b.
	executor.set(ids["c"], 12*time.Hour, 0)
	seed("c", 12*time.Hour, now.Add(-time.Second))
	mustServe("s1", "b")
	mustServe("s2", "c")
	executor.takeServed()

	// b hits its 5h limit: the 429 fails s1 over to the next rank and rebinds it there.
	executor.set(ids["b"], 0, http.StatusTooManyRequests)
	got, errRun := run("s1")
	served := executor.takeServed()
	if len(served) == 0 || served[0] != ids["b"] {
		t.Fatalf("s1 attempts = %v, want b first", served)
	}
	if errRun == nil && got != ids["c"] {
		t.Fatalf("s1 failover served %s, want c", got)
	}
	if snap, _ := tracker.Snapshot(ids["b"]); snap.Windows[cliproxyauth.ClaudeWindowFiveHour].Status != "rejected" {
		t.Fatalf("tracker missed the rejected 5h window: %+v", snap.Windows)
	}
	mustServe("s1", "c")
	if served = executor.takeServed(); len(served) != 1 || served[0] != ids["c"] {
		t.Fatalf("rebound s1 attempts = %v, want only c", served)
	}

	// A 5xx on c keeps s2 bound to c (rebind-on: quota).
	executor.set(ids["c"], 0, http.StatusBadGateway)
	_, _ = run("s2")
	executor.takeServed()
	executor.set(ids["c"], 0, 0)
	mustServe("s2", "c")

	// A brand-new session after all this avoids the rejected account.
	mustServe("s3", "c")
}

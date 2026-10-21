package claudequota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeAuths struct {
	mu    sync.Mutex
	auths map[string]*coreauth.Auth
}

func (f *fakeAuths) List() []*coreauth.Auth {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*coreauth.Auth, 0, len(f.auths))
	for _, a := range f.auths {
		out = append(out, a.Clone())
	}
	return out
}

func (f *fakeAuths) GetByID(id string) (*coreauth.Auth, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.auths[id]
	if !ok {
		return nil, false
	}
	return a.Clone(), true
}

func newFakeAuths(auths ...*coreauth.Auth) *fakeAuths {
	f := &fakeAuths{auths: map[string]*coreauth.Auth{}}
	for _, a := range auths {
		f.auths[a.ID] = a
	}
	return f
}

func claudeAuth(id, token string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": token}}
}

const usagePayloadA = `{"five_hour":{"utilization":10.0,"resets_at":"2026-10-21T10:29:59.551785+00:00"},
"seven_day":{"utilization":56.0,"resets_at":"2026-10-22T03:59:59.551812+00:00"},
"limits":[{"kind":"weekly_scoped","percent":47,"resets_at":"2026-10-22T03:59:59.552041+00:00","scope":{"model":{"display_name":"Fable"}},"is_active":false}]}`

const usagePayloadB = `{"five_hour":{"utilization":80.0,"resets_at":"2026-10-21T11:00:00+00:00"},
"seven_day":{"utilization":5.0,"resets_at":"2026-10-25T00:00:00+00:00"},"limits":[]}`

const profilePayload = `{"account":{"has_claude_max":true,"has_claude_pro":false},
"organization":{"organization_type":"claude_team","rate_limit_tier":"default_claude_max_5x","subscription_status":"active"}}`

// fakeUsageServer answers per bearer token. Unknown tokens get 401.
type fakeUsageServer struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	usage    map[string]func(w http.ResponseWriter, r *http.Request)
	hits     map[string]int // "<token> <path>" -> count
	profiles atomic.Int32
}

func newFakeUsageServer(t *testing.T) *fakeUsageServer {
	f := &fakeUsageServer{t: t, usage: map[string]func(http.ResponseWriter, *http.Request){}, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUsageServer) serve(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.Method != http.MethodGet || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" ||
		r.Header.Get("User-Agent") != "claude-cli/9.9.9 (external, cli)" || r.Header.Get("Accept") != "application/json" {
		f.t.Errorf("bad request headers: method=%s beta=%q ua=%q", r.Method, r.Header.Get("anthropic-beta"), r.Header.Get("User-Agent"))
	}
	f.mu.Lock()
	f.hits[token+" "+r.URL.Path]++
	handler := f.usage[token]
	f.mu.Unlock()
	switch r.URL.Path {
	case "/api/oauth/profile":
		f.profiles.Add(1)
		_, _ = w.Write([]byte(profilePayload))
	case "/api/oauth/usage":
		if handler == nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeUsageServer) set(token string, h func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	f.usage[token] = h
	f.mu.Unlock()
}

func (f *fakeUsageServer) usageHits(token string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[token+" /api/oauth/usage"]
}

func body(payload string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(payload)) }
}

func status(code int, retryAfter string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(code)
	}
}

var pollStart = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

type pollerFixture struct {
	poller  *Poller
	tracker *coreauth.ClaudeQuotaTracker
	clock   *testClock
	server  *fakeUsageServer
	auths   *fakeAuths
}

func newPollerFixture(t *testing.T, interval time.Duration, auths ...*coreauth.Auth) *pollerFixture {
	t.Helper()
	clock := &testClock{t: pollStart}
	tracker := coreauth.NewClaudeQuotaTracker()
	tracker.SetNowFunc(clock.Now)
	server := newFakeUsageServer(t)
	source := newFakeAuths(auths...)
	poller := New(Options{
		Tracker:   tracker,
		Auths:     source,
		BaseURL:   server.srv.URL,
		UserAgent: func() string { return "claude-cli/9.9.9 (external, cli)" },
		Interval:  interval,
		Now:       clock.Now,
		Rand:      func() float64 { return 0.5 }, // no jitter
	})
	t.Cleanup(poller.Stop)
	return &pollerFixture{poller: poller, tracker: tracker, clock: clock, server: server, auths: source}
}

func TestPollerPerBearerPayloadsAndHeaders(t *testing.T) {
	f := newPollerFixture(t, 10*time.Minute, claudeAuth("a", "fake-a"), claudeAuth("b", "fake-b"),
		&coreauth.Auth{ID: "k", Provider: "claude", Attributes: map[string]string{"api_key": "fake-key"}},
		&coreauth.Auth{ID: "x", Provider: "codex", Metadata: map[string]any{"access_token": "fake-x"}},
		&coreauth.Auth{ID: "d", Provider: "claude", Disabled: true, Metadata: map[string]any{"access_token": "fake-d"}})
	f.server.set("fake-a", body(usagePayloadA))
	f.server.set("fake-b", body(usagePayloadB))

	f.poller.pollDue(context.Background(), true)

	a, ok := f.tracker.Snapshot("a")
	if !ok || len(a.Windows) != 3 || a.Windows[coreauth.ClaudeWindowSevenDayFable].UsedPercent != 47 {
		t.Fatalf("a = %+v", a)
	}
	if a.Plan == nil || a.Plan.Label != "Max 5x" || a.Plan.RateLimitTier != "default_claude_max_5x" {
		t.Fatalf("a plan = %+v", a.Plan)
	}
	if !a.LastPollAt.Equal(pollStart) || !a.NextPollAt.Equal(pollStart.Add(10*time.Minute)) || a.PollError != "" {
		t.Fatalf("a bookkeeping = %+v", a)
	}
	b, _ := f.tracker.Snapshot("b")
	if b.Windows[coreauth.ClaudeWindowFiveHour].UsedPercent != 80 || len(b.Windows) != 2 {
		t.Fatalf("b = %+v", b)
	}
	for _, id := range []string{"k", "x", "d"} {
		if _, ok = f.tracker.Snapshot(id); ok {
			t.Fatalf("%s must not be polled", id)
		}
	}
	if f.server.usageHits("fake-x") != 0 || f.server.usageHits("fake-d") != 0 {
		t.Fatal("ineligible credentials hit the usage endpoint")
	}

	// Not due again until the interval passes.
	f.poller.pollDue(context.Background(), true)
	if f.server.usageHits("fake-a") != 1 {
		t.Fatalf("usage hits = %d, want 1", f.server.usageHits("fake-a"))
	}
	f.clock.Advance(10 * time.Minute)
	f.poller.pollDue(context.Background(), true)
	if f.server.usageHits("fake-a") != 2 {
		t.Fatalf("usage hits after interval = %d, want 2", f.server.usageHits("fake-a"))
	}
}

func TestPollerRateLimitBackoff(t *testing.T) {
	for _, tc := range []struct {
		name, retryAfter string
		code             int
		want             time.Duration
	}{
		{"429 retry-after", "120", http.StatusTooManyRequests, 2 * time.Minute},
		{"429 no retry-after", "", http.StatusTooManyRequests, 5 * time.Minute},
		{"429 capped", "7200", http.StatusTooManyRequests, time.Hour},
		{"403 http-date", pollStart.Add(90 * time.Second).Format(http.TimeFormat), http.StatusForbidden, 90 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPollerFixture(t, 10*time.Minute, claudeAuth("a", "fake-a"))
			f.server.set("fake-a", status(tc.code, tc.retryAfter))
			err := f.poller.Refresh(context.Background(), "a")
			var backoff *coreauth.ClaudeQuotaBackoffError
			if !errors.As(err, &backoff) || backoff.RetryAfter != tc.want || backoff.Status != tc.code {
				t.Fatalf("err = %v (%+v), want backoff %v", err, backoff, tc.want)
			}
			// The next background poll is the later of the backoff and the regular interval.
			nextPoll := tc.want
			if nextPoll < 10*time.Minute {
				nextPoll = 10 * time.Minute
			}
			snap, _ := f.tracker.Snapshot("a")
			if !snap.NextPollAt.Equal(pollStart.Add(nextPoll)) || !strings.Contains(snap.PollError, "rate_limited") {
				t.Fatalf("snapshot = %+v", snap)
			}
			// During backoff neither a manual refresh nor a background poll reaches upstream.
			f.clock.Advance(tc.want - time.Second)
			err = f.poller.Refresh(context.Background(), "a")
			if !errors.As(err, &backoff) || backoff.RetryAfter != time.Second {
				t.Fatalf("refresh in backoff = %v", err)
			}
			f.poller.pollDue(context.Background(), true)
			if hits := f.server.usageHits("fake-a"); hits != 1 {
				t.Fatalf("usage hits during backoff = %d, want 1", hits)
			}
			f.clock.Advance(nextPoll - tc.want + time.Second)
			f.server.set("fake-a", body(usagePayloadA))
			f.poller.pollDue(context.Background(), true)
			if hits := f.server.usageHits("fake-a"); hits != 2 {
				t.Fatalf("usage hits after backoff = %d, want 2", hits)
			}
		})
	}
}

func TestPollerUnauthorizedAndExpiredToken(t *testing.T) {
	expired := claudeAuth("e", "fake-e")
	expired.Metadata["expired"] = pollStart.Add(-time.Minute).Format(time.RFC3339)
	f := newPollerFixture(t, 10*time.Minute, claudeAuth("u", "fake-unknown"), expired)

	f.poller.pollDue(context.Background(), true)

	u, _ := f.tracker.Snapshot("u")
	if u.PollError != "unauthorized" || !u.NextPollAt.Equal(pollStart.Add(10*time.Minute)) || !u.LastPollAt.IsZero() {
		t.Fatalf("unauthorized snapshot = %+v", u)
	}
	e, _ := f.tracker.Snapshot("e")
	if e.PollError != "token_expired" || !e.NextPollAt.Equal(pollStart.Add(10*time.Minute)) {
		t.Fatalf("expired snapshot = %+v", e)
	}
	if f.server.usageHits("fake-e") != 0 {
		t.Fatal("expired token reached upstream")
	}
	if !errors.Is(f.poller.Refresh(context.Background(), "u"), ErrUnauthorized) {
		t.Fatal("manual refresh of a rejected token must return ErrUnauthorized")
	}
}

func TestPollerSingleFlightUnderConcurrentRefresh(t *testing.T) {
	f := newPollerFixture(t, 10*time.Minute, claudeAuth("a", "fake-a"))
	entered := make(chan struct{})
	release := make(chan struct{})
	f.server.set("fake-a", func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte(usagePayloadA))
	})
	const callers = 5
	joined := make(chan struct{}, callers)
	f.poller.onJoin = func() { joined <- struct{}{} }

	errs := make(chan error, callers)
	go func() { errs <- f.poller.Refresh(context.Background(), "a") }()
	<-entered
	for i := 1; i < callers; i++ {
		go func() { errs <- f.poller.Refresh(context.Background(), "a") }()
	}
	for i := 1; i < callers; i++ {
		<-joined
	}
	close(release)
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if hits := f.server.usageHits("fake-a"); hits != 1 {
		t.Fatalf("usage hits = %d, want 1 (single flight)", hits)
	}
}

func observeAllHeaders(tracker *coreauth.ClaudeQuotaTracker, id string, at time.Time, withFable bool) {
	h := http.Header{}
	reset := at.Add(time.Hour).Format(time.RFC3339)
	for _, prefix := range []string{"5h", "7d", "7d_oi"} {
		if prefix == "7d_oi" && !withFable {
			continue
		}
		h.Set("Anthropic-Ratelimit-Unified-"+prefix+"-Utilization", "0.1")
		h.Set("Anthropic-Ratelimit-Unified-"+prefix+"-Reset", reset)
	}
	tracker.ObserveHeaders(id, "", h, at)
}

func TestPollerSkipsWhenPassiveDataIsFresh(t *testing.T) {
	f := newPollerFixture(t, 10*time.Minute, claudeAuth("a", "fake-a"), claudeAuth("b", "fake-b"))
	f.server.set("fake-a", body(usagePayloadA))
	f.server.set("fake-b", body(usagePayloadB))
	observeAllHeaders(f.tracker, "a", pollStart.Add(-2*time.Minute), true)
	observeAllHeaders(f.tracker, "b", pollStart, false) // no Fable window: still polled

	f.poller.pollDue(context.Background(), true)
	if f.server.usageHits("fake-a") != 0 || f.server.usageHits("fake-b") != 1 {
		t.Fatalf("hits a=%d b=%d, want 0 and 1", f.server.usageHits("fake-a"), f.server.usageHits("fake-b"))
	}
	// a's oldest passive window ages out 8 minutes later.
	f.clock.Advance(7 * time.Minute)
	f.poller.pollDue(context.Background(), true)
	if f.server.usageHits("fake-a") != 0 {
		t.Fatal("polled while passive data was still fresh")
	}
	f.clock.Advance(time.Minute)
	f.poller.pollDue(context.Background(), true)
	if f.server.usageHits("fake-a") != 1 {
		t.Fatal("did not poll after passive data aged out")
	}
}

func TestPollerProfileFetchedOncePer24h(t *testing.T) {
	f := newPollerFixture(t, 10*time.Minute, claudeAuth("a", "fake-a"))
	f.server.set("fake-a", body(usagePayloadA))
	for i := 0; i < 3; i++ {
		if err := f.poller.Refresh(context.Background(), "a"); err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(11 * time.Minute)
	}
	if got := f.server.profiles.Load(); got != 1 {
		t.Fatalf("profile fetches = %d, want 1", got)
	}
	f.clock.Advance(24 * time.Hour)
	if err := f.poller.Refresh(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if got := f.server.profiles.Load(); got != 2 {
		t.Fatalf("profile fetches after 24h = %d, want 2", got)
	}
}

func TestJitterBounds(t *testing.T) {
	d := 10 * time.Minute
	if got := Jitter(d, 0); got != 9*time.Minute {
		t.Fatalf("Jitter(0) = %v", got)
	}
	if got := Jitter(d, 0.5); got != d {
		t.Fatalf("Jitter(0.5) = %v", got)
	}
	if got := Jitter(d, 1); got != 11*time.Minute {
		t.Fatalf("Jitter(1) = %v", got)
	}
	f := newPollerFixture(t, d, claudeAuth("a", "fake-a"))
	f.poller.rand = func() float64 { return 0.999 }
	f.server.set("fake-a", body(usagePayloadA))
	if err := f.poller.Refresh(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	snap, _ := f.tracker.Snapshot("a")
	if gap := snap.NextPollAt.Sub(pollStart); gap < 9*time.Minute || gap > 11*time.Minute {
		t.Fatalf("next poll gap = %v, want within ±10%%", gap)
	}
}

func TestPollerIntervalZeroMeansNoBackgroundPolls(t *testing.T) {
	f := newPollerFixture(t, 0, claudeAuth("a", "fake-a"))
	f.server.set("fake-a", body(usagePayloadA))
	f.poller.pollDue(context.Background(), true)
	f.poller.pollRequested(context.Background(), "a", true)
	if f.server.usageHits("fake-a") != 0 {
		t.Fatal("interval 0 polled in the background")
	}
	if err := f.poller.Refresh(context.Background(), "a"); err != nil {
		t.Fatalf("manual refresh with interval 0: %v", err)
	}
	snap, _ := f.tracker.Snapshot("a")
	if f.server.usageHits("fake-a") != 1 || !snap.NextPollAt.IsZero() {
		t.Fatalf("manual refresh hits=%d next=%v", f.server.usageHits("fake-a"), snap.NextPollAt)
	}

	// Hot reload turns background polling on.
	f.poller.SetInterval(5 * time.Minute)
	f.poller.pollDue(context.Background(), true)
	if f.server.usageHits("fake-a") != 2 {
		t.Fatalf("hits after SetInterval = %d, want 2", f.server.usageHits("fake-a"))
	}
}

func TestPollerRequestedPollOnlyWhenDue(t *testing.T) {
	f := newPollerFixture(t, 10*time.Minute, claudeAuth("a", "fake-a"))
	f.server.set("fake-a", body(usagePayloadA))
	f.poller.pollRequested(context.Background(), "a", true)
	f.poller.pollRequested(context.Background(), "a", true)
	f.poller.pollRequested(context.Background(), "missing", true)
	if hits := f.server.usageHits("fake-a"); hits != 1 {
		t.Fatalf("requested poll hits = %d, want 1", hits)
	}
}

func TestPollerStopCancelsInFlight(t *testing.T) {
	f := newPollerFixture(t, 10*time.Minute, claudeAuth("a", "fake-a"))
	entered := make(chan struct{})
	f.server.set("fake-a", func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	})
	f.poller.Start(context.Background())
	if f.tracker.Refresher() == nil {
		t.Fatal("Start did not register the refresher")
	}
	<-entered // the loop's first check polls immediately
	f.poller.Stop()
	snap, _ := f.tracker.Snapshot("a")
	if !strings.Contains(snap.PollError, "context canceled") || !snap.PollStartedAt.IsZero() {
		t.Fatalf("after Stop: %+v", snap)
	}
	if f.tracker.Refresher() != nil {
		t.Fatal("Stop did not unregister the refresher")
	}
}

func TestPlanLabel(t *testing.T) {
	for _, tc := range []struct {
		tier, org      string
		hasMax, hasPro bool
		want           string
	}{
		{"default_claude_max_20x", "", true, false, "Max 20x"},
		{"default_claude_max_5x", "claude_team", false, false, "Max 5x"},
		{"", "", true, false, "Max"},
		{"", "", false, true, "Pro"},
		{"default", "claude_team", false, false, "Team"},
		{"", "", false, false, ""},
	} {
		if got := PlanLabel(tc.tier, tc.org, tc.hasMax, tc.hasPro); got != tc.want {
			t.Fatalf("PlanLabel(%+v) = %q, want %q", tc, got, tc.want)
		}
	}
}

// A 429/403 must never pull the next background poll earlier than the regular
// interval: with a 1 h interval, a 429 without Retry-After (5 min backoff) used
// to schedule the next poll in 5 min, polling a rate-limited bearer faster.
func TestPollerBackoffNeverShortensRegularInterval(t *testing.T) {
	f := newPollerFixture(t, time.Hour, claudeAuth("a", "fake-a"))
	f.server.set("fake-a", status(http.StatusTooManyRequests, ""))
	f.poller.pollDue(context.Background(), true)
	if hits := f.server.usageHits("fake-a"); hits != 1 {
		t.Fatalf("first poll hits = %d, want 1", hits)
	}
	snap, _ := f.tracker.Snapshot("a")
	if !snap.NextPollAt.Equal(pollStart.Add(time.Hour)) {
		t.Fatalf("next poll = %v, want %v", snap.NextPollAt, pollStart.Add(time.Hour))
	}
	f.server.set("fake-a", body(usagePayloadA))
	f.clock.Advance(6 * time.Minute) // past the 5 min backoff, well inside the interval
	f.poller.pollDue(context.Background(), true)
	f.poller.pollRequested(context.Background(), "a", true)
	if hits := f.server.usageHits("fake-a"); hits != 1 {
		t.Fatalf("background polls after backoff but before interval = %d, want 1", hits)
	}
	// Manual refresh is gated only by the backoff, which has passed.
	if err := f.poller.Refresh(context.Background(), "a"); err != nil {
		t.Fatalf("manual refresh after backoff: %v", err)
	}
	if hits := f.server.usageHits("fake-a"); hits != 2 {
		t.Fatalf("hits after manual refresh = %d, want 2", hits)
	}
}

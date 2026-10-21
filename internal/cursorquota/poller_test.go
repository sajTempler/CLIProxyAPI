package cursorquota

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
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

func cursorAuth(id, token string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, Provider: "cursor", Metadata: map[string]any{"type": "cursor", "access_token": token}}
}

// Live 2026-10-21 shapes (docs/cursor/USAGE-TRACKING.md), abridged.
const teamUsage = `{"billingCycleStart":"1790807513000","billingCycleEnd":"1793399513000",
"planUsage":{"totalSpend":28233,"includedSpend":2000,"bonusSpend":26233,"limit":2000,"autoPercentUsed":7.811428571428572,"apiPercentUsed":100,"totalPercentUsed":22.5864},
"spendLimitUsage":{"totalSpend":14030,"pooledLimit":"240000","pooledUsed":14030,"pooledRemaining":"225970","individualLimit":40000,"individualUsed":11787,"individualRemaining":28213,"limitType":"team"},
"autoBucketModels":["default","composer-2"]}`

const personalUsage = `{"planUsage":{"includedSpend":1000,"limit":2000},
"spendLimitUsage":{"individualUsed":550,"limitType":"user"}}`

const planInfo = `{"planInfo":{"planName":"Team","price":"$40/mo","billingCycleEnd":"1793399513000"}}`

// fakeDashboard is an httptest DashboardService (Connect unary, JSON) answering per bearer.
type fakeDashboard struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	usage map[string]func(w http.ResponseWriter, r *http.Request)
	hard  func(w http.ResponseWriter, r *http.Request)
	hits  map[string]int // "<token> <method>" -> count
}

func newFakeDashboard(t *testing.T) *fakeDashboard {
	f := &fakeDashboard{t: t, usage: map[string]func(http.ResponseWriter, *http.Request){}, hits: map[string]int{}}
	f.hard = body(`{"hardLimit":200}`)
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDashboard) serve(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	raw, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodPost || string(raw) != "{}" || r.Header.Get("Content-Type") != "application/json" ||
		r.Header.Get("Connect-Protocol-Version") != "1" || r.Header.Get("x-cursor-client-type") != "cli" ||
		r.Header.Get("x-cursor-client-version") != "2026.10.19-test" {
		f.t.Errorf("bad request: method=%s body=%q headers=%v", r.Method, raw, r.Header)
	}
	method := strings.TrimPrefix(r.URL.Path, "/aiserver.v1.DashboardService/")
	f.mu.Lock()
	f.hits[token+" "+method]++
	usage := f.usage[token]
	hard := f.hard
	f.mu.Unlock()
	if usage == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"bad token"}`))
		return
	}
	switch method {
	case "GetCurrentPeriodUsage":
		usage(w, r)
	case "GetPlanInfo":
		_, _ = w.Write([]byte(planInfo))
	case "GetHardLimit":
		hard(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeDashboard) set(token string, h func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	f.usage[token] = h
	f.mu.Unlock()
}

func (f *fakeDashboard) count(token, method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[token+" "+method]
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

type fixture struct {
	poller  *Poller
	tracker *coreauth.CursorQuotaTracker
	clock   *testClock
	server  *fakeDashboard
}

func newFixture(t *testing.T, interval time.Duration, auths ...*coreauth.Auth) *fixture {
	t.Helper()
	clock := &testClock{t: pollStart}
	tracker := coreauth.NewCursorQuotaTracker()
	tracker.SetNowFunc(clock.Now)
	server := newFakeDashboard(t)
	source := &fakeAuths{auths: map[string]*coreauth.Auth{}}
	for _, a := range auths {
		source.auths[a.ID] = a
	}
	poller := New(Options{
		Tracker:       tracker,
		Auths:         source,
		BaseURL:       server.srv.URL,
		ClientVersion: func() string { return "2026.10.19-test" },
		Interval:      interval,
		Now:           clock.Now,
		Rand:          func() float64 { return 0.5 }, // no jitter
	})
	t.Cleanup(poller.Stop)
	return &fixture{poller: poller, tracker: tracker, clock: clock, server: server}
}

func TestPollerMapsUsagePerAccount(t *testing.T) {
	f := newFixture(t, 15*time.Minute, cursorAuth("team", "fake-team"), cursorAuth("solo", "fake-solo"),
		&coreauth.Auth{ID: "c", Provider: "claude", Metadata: map[string]any{"access_token": "fake-claude"}},
		&coreauth.Auth{ID: "d", Provider: "cursor", Disabled: true, Metadata: map[string]any{"access_token": "fake-d"}})
	f.server.set("fake-team", body(teamUsage))
	f.server.set("fake-solo", body(personalUsage))
	f.server.set("fake-claude", body(teamUsage))
	f.server.set("fake-d", body(teamUsage))

	f.poller.pollDue(context.Background(), true)

	team, ok := f.tracker.Snapshot("team")
	if !ok || team.Usage == nil || team.PollError != "" {
		t.Fatalf("team = %+v", team)
	}
	u := team.Usage
	if u.IncludedPercent != 22.5864 || u.APIPercent != 100 || u.OnDemand.LimitKind != "fixed" || u.OnDemand.UsedUSD != 117.87 || u.OnDemand.LimitUSD != 400 {
		t.Fatalf("team usage = %+v", u)
	}
	if !u.ResetsAt.Equal(time.Date(2026, 10, 30, 22, 31, 53, 0, time.UTC)) {
		t.Fatalf("resets_at = %v", u.ResetsAt)
	}
	if team.Plan == nil || team.Plan.Label != "Team" || team.Plan.Price != "$40/mo" {
		t.Fatalf("plan = %+v", team.Plan)
	}
	if !team.LastPollAt.Equal(pollStart) || !team.NextPollAt.Equal(pollStart.Add(15*time.Minute)) || !team.ObservedAt.Equal(pollStart) {
		t.Fatalf("bookkeeping = %+v", team)
	}
	if f.server.count("fake-team", "GetHardLimit") != 0 {
		t.Fatal("team account with individualLimit must not call GetHardLimit")
	}

	solo, _ := f.tracker.Snapshot("solo")
	if solo.Usage == nil || solo.Usage.IncludedPercent != 50 || solo.Usage.OnDemand.LimitKind != "fixed" || solo.Usage.OnDemand.LimitUSD != 200 || solo.Usage.OnDemand.UsedUSD != 5.5 {
		t.Fatalf("solo usage = %+v", solo.Usage)
	}
	// No billingCycleEnd in the usage response: planInfo.billingCycleEnd is the fallback.
	if !solo.Usage.ResetsAt.Equal(time.Date(2026, 10, 30, 22, 31, 53, 0, time.UTC)) {
		t.Fatalf("solo resets_at fallback = %v", solo.Usage.ResetsAt)
	}
	for _, id := range []string{"c", "d"} {
		if _, ok = f.tracker.Snapshot(id); ok {
			t.Fatalf("%s must not be polled", id)
		}
	}
	if f.server.count("fake-claude", "GetCurrentPeriodUsage") != 0 || f.server.count("fake-d", "GetCurrentPeriodUsage") != 0 {
		t.Fatal("ineligible credentials hit DashboardService")
	}

	// Not due again until the interval passes.
	f.poller.pollDue(context.Background(), true)
	if got := f.server.count("fake-team", "GetCurrentPeriodUsage"); got != 1 {
		t.Fatalf("usage calls = %d, want 1", got)
	}
	f.clock.Advance(15 * time.Minute)
	f.poller.pollDue(context.Background(), true)
	if got := f.server.count("fake-team", "GetCurrentPeriodUsage"); got != 2 {
		t.Fatalf("usage calls after interval = %d, want 2", got)
	}
}

func TestPollerPlanAndHardLimitCadence(t *testing.T) {
	f := newFixture(t, 15*time.Minute, cursorAuth("solo", "fake-solo"))
	f.server.set("fake-solo", body(personalUsage))
	for i := 0; i < 4; i++ { // 0, 15, 30, 45 min
		if err := f.poller.Refresh(context.Background(), "solo"); err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(15 * time.Minute)
	}
	if got := f.server.count("fake-solo", "GetPlanInfo"); got != 1 {
		t.Fatalf("plan calls in 1h = %d, want 1", got)
	}
	if got := f.server.count("fake-solo", "GetHardLimit"); got != 1 {
		t.Fatalf("hard limit calls in 1h = %d, want 1", got)
	}
	f.clock.Advance(6 * time.Hour)
	if err := f.poller.Refresh(context.Background(), "solo"); err != nil {
		t.Fatal(err)
	}
	if got := f.server.count("fake-solo", "GetHardLimit"); got != 2 {
		t.Fatalf("hard limit calls after 6h = %d, want 2", got)
	}
	if got := f.server.count("fake-solo", "GetPlanInfo"); got != 1 {
		t.Fatalf("plan calls before 24h = %d, want 1", got)
	}
	f.clock.Advance(24 * time.Hour)
	if err := f.poller.Refresh(context.Background(), "solo"); err != nil {
		t.Fatal(err)
	}
	if got := f.server.count("fake-solo", "GetPlanInfo"); got != 2 {
		t.Fatalf("plan calls after 24h = %d, want 2", got)
	}
}

func TestPollerHardLimitFailureKeepsLastGoodValue(t *testing.T) {
	f := newFixture(t, 15*time.Minute, cursorAuth("solo", "fake-solo"))
	f.server.set("fake-solo", body(personalUsage))
	f.server.hard = status(http.StatusInternalServerError, "")
	if err := f.poller.Refresh(context.Background(), "solo"); err != nil {
		t.Fatalf("hard limit failure must be tolerated: %v", err)
	}
	snap, _ := f.tracker.Snapshot("solo")
	if snap.Usage.OnDemand.LimitKind != "unavailable" {
		t.Fatalf("no hard limit yet: kind = %q", snap.Usage.OnDemand.LimitKind)
	}
	f.server.mu.Lock()
	f.server.hard = body(`{"hardLimit":300}`)
	f.server.mu.Unlock()
	f.clock.Advance(15 * time.Minute) // failed calls are retried on the next poll
	_ = f.poller.Refresh(context.Background(), "solo")
	f.server.mu.Lock()
	f.server.hard = status(http.StatusInternalServerError, "")
	f.server.mu.Unlock()
	f.clock.Advance(7 * time.Hour)
	_ = f.poller.Refresh(context.Background(), "solo")
	snap, _ = f.tracker.Snapshot("solo")
	if snap.Usage.OnDemand.LimitKind != "fixed" || snap.Usage.OnDemand.LimitUSD != 300 {
		t.Fatalf("last good hard limit not kept: %+v", snap.Usage.OnDemand)
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
			f := newFixture(t, 15*time.Minute, cursorAuth("a", "fake-a"))
			f.server.set("fake-a", status(tc.code, tc.retryAfter))
			err := f.poller.Refresh(context.Background(), "a")
			var backoff *coreauth.QuotaBackoffError
			if !errors.As(err, &backoff) || backoff.RetryAfter != tc.want || backoff.Status != tc.code {
				t.Fatalf("err = %v (%+v), want backoff %v", err, backoff, tc.want)
			}
			nextPoll := max(tc.want, 15*time.Minute)
			snap, _ := f.tracker.Snapshot("a")
			if !snap.NextPollAt.Equal(pollStart.Add(nextPoll)) || !strings.HasPrefix(snap.PollError, "rate_limited") {
				t.Fatalf("snapshot = %+v", snap)
			}
			f.clock.Advance(tc.want - time.Second)
			err = f.poller.Refresh(context.Background(), "a")
			if !errors.As(err, &backoff) || backoff.RetryAfter != time.Second {
				t.Fatalf("refresh in backoff = %v", err)
			}
			f.poller.pollDue(context.Background(), true)
			if hits := f.server.count("fake-a", "GetCurrentPeriodUsage"); hits != 1 {
				t.Fatalf("usage calls during backoff = %d, want 1", hits)
			}
			f.clock.Advance(nextPoll - tc.want + time.Second)
			f.server.set("fake-a", body(teamUsage))
			f.poller.pollDue(context.Background(), true)
			if hits := f.server.count("fake-a", "GetCurrentPeriodUsage"); hits != 2 {
				t.Fatalf("usage calls after backoff = %d, want 2", hits)
			}
			if snap, _ = f.tracker.Snapshot("a"); snap.PollError != "" || snap.Usage == nil {
				t.Fatalf("after recovery: %+v", snap)
			}
		})
	}
}

func TestPollerUnauthorizedExpiredAndStaleData(t *testing.T) {
	expired := cursorAuth("e", "fake-e")
	expired.Metadata["expired"] = pollStart.Add(-time.Minute).Format(time.RFC3339)
	noToken := &coreauth.Auth{ID: "n", Provider: "cursor", Metadata: map[string]any{"type": "cursor"}}
	f := newFixture(t, 15*time.Minute, cursorAuth("u", "fake-u"), expired, noToken)
	f.server.set("fake-u", body(teamUsage))

	f.poller.pollDue(context.Background(), true)
	e, _ := f.tracker.Snapshot("e")
	if e.PollError != "token_expired" || !e.NextPollAt.Equal(pollStart.Add(15*time.Minute)) || e.Usage != nil {
		t.Fatalf("expired snapshot = %+v", e)
	}
	if f.server.count("fake-e", "GetCurrentPeriodUsage") != 0 {
		t.Fatal("expired token reached upstream")
	}
	if n, _ := f.tracker.Snapshot("n"); n.PollError != "no_access_token" {
		t.Fatalf("no token snapshot = %+v", n)
	}

	// u succeeds once, then its token is rejected: data stays, poll_error tells it is stale.
	f.server.set("fake-u", nil)
	f.clock.Advance(15 * time.Minute)
	if !errors.Is(f.poller.Refresh(context.Background(), "u"), ErrUnauthorized) {
		t.Fatal("rejected token must return ErrUnauthorized")
	}
	u, _ := f.tracker.Snapshot("u")
	if u.PollError != "unauthorized" || u.Usage == nil || !u.ObservedAt.Equal(pollStart) || !u.LastPollAt.Equal(pollStart) {
		t.Fatalf("unauthorized snapshot = %+v", u)
	}
}

func TestPollerUsageUnavailableAndBadResponse(t *testing.T) {
	f := newFixture(t, 15*time.Minute, cursorAuth("ent", "fake-ent"), cursorAuth("bad", "fake-bad"), cursorAuth("err", "fake-err"))
	f.server.set("fake-ent", body(`{"spendLimitUsage":{"limitType":"team"}}`))
	f.server.set("fake-bad", body(`not json`))
	f.server.set("fake-err", status(http.StatusBadGateway, ""))
	f.poller.pollDue(context.Background(), true)
	for id, want := range map[string]string{"ent": "usage_unavailable", "bad": "invalid_response", "err": "http_502"} {
		snap, _ := f.tracker.Snapshot(id)
		if snap.PollError != want || snap.Usage != nil {
			t.Fatalf("%s: %+v, want %s", id, snap, want)
		}
	}
}

func TestPollerSingleFlightUnderConcurrentRefresh(t *testing.T) {
	f := newFixture(t, 15*time.Minute, cursorAuth("a", "fake-a"))
	entered := make(chan struct{})
	release := make(chan struct{})
	f.server.set("fake-a", func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte(teamUsage))
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
	// A background poll during the in-flight call is skipped, not queued.
	f.poller.pollDue(context.Background(), true)
	close(release)
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if hits := f.server.count("fake-a", "GetCurrentPeriodUsage"); hits != 1 {
		t.Fatalf("usage calls = %d, want 1 (single flight)", hits)
	}
}

func TestJitterBounds(t *testing.T) {
	d := 15 * time.Minute
	if Jitter(d, 0) != 13*time.Minute+30*time.Second || Jitter(d, 0.5) != d || Jitter(d, 1) != 16*time.Minute+30*time.Second {
		t.Fatalf("jitter = %v %v %v", Jitter(d, 0), Jitter(d, 0.5), Jitter(d, 1))
	}
}

func TestPollerIntervalZeroMeansNoBackgroundPolls(t *testing.T) {
	f := newFixture(t, 0, cursorAuth("a", "fake-a"))
	f.server.set("fake-a", body(teamUsage))
	f.poller.pollDue(context.Background(), true)
	if f.server.count("fake-a", "GetCurrentPeriodUsage") != 0 {
		t.Fatal("interval 0 polled in the background")
	}
	if err := f.poller.Refresh(context.Background(), "a"); err != nil {
		t.Fatalf("manual refresh with interval 0: %v", err)
	}
	if snap, _ := f.tracker.Snapshot("a"); !snap.NextPollAt.IsZero() || snap.Usage == nil {
		t.Fatalf("manual refresh snapshot = %+v", snap)
	}
	f.poller.SetInterval(5 * time.Minute)
	f.poller.pollDue(context.Background(), true)
	if got := f.server.count("fake-a", "GetCurrentPeriodUsage"); got != 2 {
		t.Fatalf("calls after SetInterval = %d, want 2", got)
	}
}

func TestPollerStopCancelsInFlight(t *testing.T) {
	f := newFixture(t, 15*time.Minute, cursorAuth("a", "fake-a"))
	entered := make(chan struct{})
	f.server.set("fake-a", func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	})
	f.poller.Start(context.Background())
	if f.tracker.Refresher() == nil {
		t.Fatal("Start did not register the refresher")
	}
	<-entered
	f.poller.Stop()
	snap, _ := f.tracker.Snapshot("a")
	if !strings.Contains(snap.PollError, "context canceled") || !snap.PollStartedAt.IsZero() {
		t.Fatalf("after Stop: %+v", snap)
	}
	if f.tracker.Refresher() != nil {
		t.Fatal("Stop did not unregister the refresher")
	}
}

func TestPollerNeverLogsTokensOrRewritesAuth(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.TraceLevel)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetLevel(prevLevel)
	})
	a := cursorAuth("a", "fake-secret-token")
	f := newFixture(t, 15*time.Minute, a)
	f.server.set("fake-secret-token", status(http.StatusInternalServerError, ""))
	f.server.hard = status(http.StatusInternalServerError, "")
	_ = f.poller.Refresh(context.Background(), "a")
	f.clock.Advance(time.Hour)
	f.server.set("fake-secret-token", body(personalUsage))
	_ = f.poller.Refresh(context.Background(), "a")
	if strings.Contains(buf.String(), "fake-secret-token") {
		t.Fatal("token leaked into logs")
	}
	if snap, _ := f.tracker.Snapshot("a"); strings.Contains(snap.PollError, "fake-secret-token") {
		t.Fatal("token leaked into poll_error")
	}
	if a.Metadata["access_token"] != "fake-secret-token" || len(a.Metadata) != 2 {
		t.Fatalf("poller modified the credential: %v", a.Metadata)
	}
}

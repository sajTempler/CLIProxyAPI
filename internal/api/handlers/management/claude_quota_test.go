package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var quotaTestNow = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

type fakeQuotaRefresher struct {
	mu       sync.Mutex
	err      error
	interval time.Duration
	calls    []string
	onCall   func(id string)
}

func (f *fakeQuotaRefresher) Refresh(_ context.Context, authID string) error {
	f.mu.Lock()
	f.calls = append(f.calls, authID)
	f.mu.Unlock()
	if f.onCall != nil {
		f.onCall(authID)
	}
	return f.err
}

func (f *fakeQuotaRefresher) Interval() time.Duration { return f.interval }

type quotaHandlerFixture struct {
	h       *Handler
	manager *coreauth.Manager
	tracker *coreauth.ClaudeQuotaTracker
	auths   map[string]*coreauth.Auth
}

func newQuotaHandlerFixture(t *testing.T, cfg *config.Config, names ...string) *quotaHandlerFixture {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	tracker := coreauth.NewClaudeQuotaTracker()
	tracker.SetNowFunc(func() time.Time { return quotaTestNow })
	manager.SetClaudeQuotaTracker(tracker)
	f := &quotaHandlerFixture{manager: manager, tracker: tracker, auths: map[string]*coreauth.Auth{}}
	for _, name := range names {
		auth := &coreauth.Auth{
			ID: "claude-" + name + ".json", FileName: "claude-" + name + ".json", Provider: "claude", Status: coreauth.StatusActive,
			Metadata: map[string]any{"access_token": "secret-access-" + name, "refresh_token": "secret-refresh-" + name, "email": name + "@example.invalid"},
		}
		registered, errRegister := manager.Register(context.Background(), auth)
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		registered.EnsureIndex()
		f.auths[name] = registered
	}
	// Non-OAuth and non-Claude credentials never appear.
	for _, other := range []*coreauth.Auth{
		{ID: "claude-key", Provider: "claude", Attributes: map[string]string{"api_key": "secret-api-key"}},
		{ID: "codex-x", Provider: "codex", Metadata: map[string]any{"access_token": "secret-codex"}},
	} {
		if _, errRegister := manager.Register(context.Background(), other); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.AuthDir = t.TempDir()
	f.h = NewHandlerWithoutConfigFilePath(cfg, manager)
	return f
}

func (f *quotaHandlerFixture) usage(name string, five, week float64, fable *float64, weekReset time.Time) {
	fiveReset := quotaTestNow.Add(90 * time.Minute).Format(time.RFC3339)
	wr := weekReset.Format(time.RFC3339)
	u := coreauth.ClaudeUsageResponse{
		FiveHour: &coreauth.ClaudeUsageWindow{Utilization: &five, ResetsAt: &fiveReset},
		SevenDay: &coreauth.ClaudeUsageWindow{Utilization: &week, ResetsAt: &wr},
	}
	if fable != nil {
		var limit coreauth.ClaudeUsageLimit
		raw := `{"kind":"weekly_scoped","resets_at":"` + wr + `","scope":{"model":{"display_name":"Fable"}}}`
		if errUnmarshal := json.Unmarshal([]byte(raw), &limit); errUnmarshal != nil {
			panic(errUnmarshal)
		}
		limit.Percent = fable
		u.Limits = []coreauth.ClaudeUsageLimit{limit}
	}
	f.tracker.ObserveUsage(f.auths[name].ID, u, quotaTestNow.Add(-time.Minute))
}

func pf(v float64) *float64 { return &v }

func (f *quotaHandlerFixture) do(t *testing.T, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	ctx.Request = httptest.NewRequest(method, "/v8/management/quota/claude", reader)
	if body != "" {
		ctx.Request.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodGet {
		f.h.GetClaudeQuota(ctx)
	} else {
		f.h.RefreshClaudeQuota(ctx)
	}
	return rec
}

func decodeQuota(t *testing.T, rec *httptest.ResponseRecorder) claudeQuotaResponseJSON {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out claudeQuotaResponseJSON
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &out); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return out
}

func TestGetClaudeQuotaGoldenShape(t *testing.T) {
	cfg := &config.Config{Routing: config.RoutingConfig{Strategy: "sr"}}
	f := newQuotaHandlerFixture(t, cfg, "a")
	f.usage("a", 10, 56, pf(47), quotaTestNow.Add(19*time.Hour))
	f.tracker.ObservePlan(f.auths["a"].ID, coreauth.ClaudeQuotaPlan{Label: "Max 5x", OrganizationType: "claude_team", RateLimitTier: "default_claude_max_5x"})
	f.tracker.ObservePollResult(f.auths["a"].ID, quotaTestNow.Add(-time.Minute), quotaTestNow.Add(-time.Minute), nil, quotaTestNow.Add(9*time.Minute))

	rec := f.do(t, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	golden := `{
  "provider": "claude",
  "generated_at": "2026-10-21T09:00:00Z",
  "strategy": "soonest-reset",
  "refresh_interval_seconds": 600,
  "summary": {
    "credentials": 1, "loaded": 1, "weekly_window": "seven_day_fable",
    "weekly_remaining_percent_sum": 53, "weekly_capacity_percent": 100,
    "segments": [{"auth_index": "IDX", "remaining_percent": 53}],
    "next_reset_at": "2026-10-22T04:00:00Z"
  },
  "credentials": [{
    "auth_index": "IDX", "id": "claude-a.json", "name": "claude-a.json", "email": "a@example.invalid",
    "label": "", "disabled": false, "status": "active", "cooldown_until": null,
    "plan": {"label": "Max 5x", "organization_type": "claude_team", "rate_limit_tier": "default_claude_max_5x"},
    "routing": {"rank": 1, "state": "eligible", "weekly_reset_at": "2026-10-22T04:00:00Z", "key_source": "seven_day_fable"},
    "windows": {
      "seven_day_fable": {"used_percent": 47, "remaining_percent": 53, "resets_at": "2026-10-22T04:00:00Z", "status": "", "observed_at": "2026-10-21T08:59:00Z", "source": "poll"},
      "five_hour": {"used_percent": 10, "remaining_percent": 90, "resets_at": "2026-10-21T10:30:00Z", "status": "", "observed_at": "2026-10-21T08:59:00Z", "source": "poll"},
      "seven_day": {"used_percent": 56, "remaining_percent": 44, "resets_at": "2026-10-22T04:00:00Z", "status": "", "observed_at": "2026-10-21T08:59:00Z", "source": "poll"}
    },
    "last_poll_at": "2026-10-21T08:59:00Z", "next_poll_at": "2026-10-21T09:09:00Z", "poll_started_at": null, "poll_error": ""
  }]
}`
	golden = strings.ReplaceAll(golden, "IDX", f.auths["a"].Index)
	var want, got any
	if errUnmarshal := json.Unmarshal([]byte(golden), &want); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &got); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("response mismatch\n got: %s\nwant: %s", rec.Body.String(), golden)
	}
}

func TestGetClaudeQuotaSummaryMath(t *testing.T) {
	f := newQuotaHandlerFixture(t, nil, "a", "b", "c", "d", "e", "f")
	reset := quotaTestNow.Add(24 * time.Hour)
	f.usage("a", 10, 50, pf(47), reset.Add(time.Hour))
	f.usage("b", 10, 50, pf(10), reset)
	f.usage("c", 10, 50, pf(20), reset.Add(2*time.Hour))
	f.usage("d", 10, 50, pf(14), reset.Add(3*time.Hour))
	f.usage("e", 10, 50, pf(0), reset.Add(4*time.Hour))
	// f has no data: counted as a credential, not as loaded.

	out := decodeQuota(t, f.do(t, http.MethodGet, ""))
	s := out.Summary
	if s.Credentials != 6 || s.Loaded != 5 || s.WeeklyWindow != "seven_day_fable" {
		t.Fatalf("summary = %+v", s)
	}
	if s.WeeklyRemainingPercentSum != 409 || s.WeeklyCapacityPercent != 500 || len(s.Segments) != 5 {
		t.Fatalf("summary math = %+v, want 409 of 500 across 5", s)
	}
	if s.NextResetAt == nil || !s.NextResetAt.Equal(reset) {
		t.Fatalf("next_reset_at = %v, want %v", s.NextResetAt, reset)
	}
	if out.Strategy != "round-robin" || out.RefreshIntervalSeconds != 0 {
		t.Fatalf("strategy=%s interval=%d", out.Strategy, out.RefreshIntervalSeconds)
	}
	// The dry-run rank orders eligible accounts by Fable reset; f is unknown and last.
	var first, last claudeQuotaCredentialJSON
	for _, c := range out.Credentials {
		switch c.Routing.Rank {
		case 1:
			first = c
		case 6:
			last = c
		}
	}
	if first.ID != "claude-b.json" || last.ID != "claude-f.json" || last.Routing.State != "unknown" {
		t.Fatalf("ranks: first=%+v last=%+v", first.Routing, last.Routing)
	}
	if last.Windows["five_hour"] != nil || len(last.Windows) != 3 {
		t.Fatalf("missing windows must be null: %+v", last.Windows)
	}
}

func TestGetClaudeQuotaWeeklyWindowFallback(t *testing.T) {
	f := newQuotaHandlerFixture(t, nil, "a", "b")
	f.usage("a", 10, 40, pf(47), quotaTestNow.Add(24*time.Hour))
	f.usage("b", 10, 70, nil, quotaTestNow.Add(48*time.Hour))
	s := decodeQuota(t, f.do(t, http.MethodGet, "")).Summary
	if s.WeeklyWindow != "seven_day" || s.WeeklyRemainingPercentSum != 90 || s.WeeklyCapacityPercent != 200 {
		t.Fatalf("summary = %+v, want seven_day 60+30 of 200", s)
	}
}

func TestGetClaudeQuotaNeverLeaksTokens(t *testing.T) {
	f := newQuotaHandlerFixture(t, nil, "a", "b")
	f.usage("a", 10, 40, pf(47), quotaTestNow.Add(24*time.Hour))
	f.tracker.ObservePollResult(f.auths["b"].ID, quotaTestNow, quotaTestNow, errTest("unauthorized"), quotaTestNow.Add(10*time.Minute))
	f.tracker.SetRefresher(&fakeQuotaRefresher{})
	for _, rec := range []*httptest.ResponseRecorder{f.do(t, http.MethodGet, ""), f.do(t, http.MethodPost, "{}")} {
		if strings.Contains(rec.Body.String(), "secret") {
			t.Fatalf("response leaks a secret: %s", rec.Body.String())
		}
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestRefreshClaudeQuotaBackoffAndErrors(t *testing.T) {
	f := newQuotaHandlerFixture(t, nil, "a", "b")

	// No poller.
	if rec := f.do(t, http.MethodPost, "{}"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no poller status = %d, want 503", rec.Code)
	}

	refresher := &fakeQuotaRefresher{err: &coreauth.ClaudeQuotaBackoffError{Status: 429, RetryAfter: 90*time.Second + 500*time.Millisecond}}
	f.tracker.SetRefresher(refresher)
	rec := f.do(t, http.MethodPost, `{"auth_index":"`+f.auths["a"].Index+`"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("backoff status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &body); errUnmarshal != nil || body["error"] != "backoff" || body["retry_after_seconds"] != float64(91) {
		t.Fatalf("backoff body = %s", rec.Body.String())
	}

	if rec = f.do(t, http.MethodPost, `{"auth_index":"nope"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown index status = %d", rec.Code)
	}
	if rec = f.do(t, http.MethodPost, `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d", rec.Code)
	}
}

func TestRefreshClaudeQuotaOneAndAll(t *testing.T) {
	f := newQuotaHandlerFixture(t, nil, "a", "b")
	refresher := &fakeQuotaRefresher{interval: 2 * time.Minute}
	refresher.onCall = func(id string) {
		used := 33.0
		f.tracker.ObserveUsage(id, coreauth.ClaudeUsageResponse{FiveHour: &coreauth.ClaudeUsageWindow{Utilization: &used}}, quotaTestNow)
		f.tracker.ObservePollResult(id, quotaTestNow, quotaTestNow, nil, quotaTestNow.Add(2*time.Minute))
	}
	f.tracker.SetRefresher(refresher)

	rec := f.do(t, http.MethodPost, `{"auth_index":"`+f.auths["b"].Index+`"}`)
	var one struct {
		Credentials []claudeQuotaCredentialJSON `json:"credentials"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &one); errUnmarshal != nil || rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(one.Credentials) != 1 || one.Credentials[0].ID != "claude-b.json" || one.Credentials[0].LastPollAt == nil ||
		one.Credentials[0].Windows["five_hour"] == nil || one.Credentials[0].Windows["five_hour"].RemainingPercent != 67 {
		t.Fatalf("single refresh = %+v", one.Credentials)
	}

	refresher.calls = nil
	rec = f.do(t, http.MethodPost, "")
	var all struct {
		Credentials []claudeQuotaCredentialJSON `json:"credentials"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &all); errUnmarshal != nil || rec.Code != http.StatusOK || len(all.Credentials) != 2 || len(refresher.calls) != 2 {
		t.Fatalf("refresh all status=%d calls=%v body=%s", rec.Code, refresher.calls, rec.Body.String())
	}
	if out := decodeQuota(t, f.do(t, http.MethodGet, "")); out.RefreshIntervalSeconds != 120 {
		t.Fatalf("refresh_interval_seconds = %d, want poller interval 120", out.RefreshIntervalSeconds)
	}
}

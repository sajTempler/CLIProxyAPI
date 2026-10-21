package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type cursorQuotaFixture struct {
	h       *Handler
	tracker *coreauth.CursorQuotaTracker
	auths   map[string]*coreauth.Auth
}

func newCursorQuotaFixture(t *testing.T, names ...string) *cursorQuotaFixture {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	tracker := coreauth.NewCursorQuotaTracker()
	tracker.SetNowFunc(func() time.Time { return quotaTestNow })
	manager.SetCursorQuotaTracker(tracker)
	f := &cursorQuotaFixture{tracker: tracker, auths: map[string]*coreauth.Auth{}}
	for _, name := range names {
		auth := &coreauth.Auth{
			ID: "cursor-" + name + ".json", FileName: "cursor-" + name + ".json", Provider: "cursor", Status: coreauth.StatusActive,
			Metadata: map[string]any{"type": "cursor", "access_token": "secret-access-" + name, "api_key": "secret-key-" + name, "email": name + "@example.invalid"},
		}
		registered, errRegister := manager.Register(context.Background(), auth)
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		registered.EnsureIndex()
		f.auths[name] = registered
	}
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "claude-x.json", Provider: "claude", Metadata: map[string]any{"access_token": "secret-claude"}}); errRegister != nil {
		t.Fatal(errRegister)
	}
	f.h = NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	return f
}

var cursorReset = time.Date(2026, 10, 30, 22, 31, 53, 0, time.UTC)

func liveCursorUsage() coreauth.CursorUsage {
	return coreauth.CursorUsage{
		IncludedPercent: 22.5864, AutoPercent: 7.8114, APIPercent: 100,
		OnDemand:         coreauth.CursorOnDemand{LimitKind: "fixed", UsedUSD: 117.87, LimitUSD: 400},
		CycleStart:       time.Date(2026, 9, 30, 22, 31, 53, 0, time.UTC),
		ResetsAt:         cursorReset,
		IncludedSpendUSD: 20, IncludedLimitUSD: 20, BonusUSD: 262.33, TotalSpendUSD: 282.33,
		AutoBucketModels: []string{"default", "composer-2"},
	}
}

func (f *cursorQuotaFixture) do(t *testing.T, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(method, "/v8/management/quota/cursor", bytes.NewReader([]byte(body)))
	if body != "" {
		ctx.Request.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodGet {
		f.h.GetCursorQuota(ctx)
	} else {
		f.h.RefreshCursorQuota(ctx)
	}
	return rec
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestGetCursorQuotaContractShape(t *testing.T) {
	f := newCursorQuotaFixture(t, "a", "b")
	a := f.auths["a"].ID
	polled := quotaTestNow.Add(-time.Minute)
	f.tracker.ObservePlan(a, coreauth.CursorQuotaPlan{Label: "Team", Price: "$40/mo"})
	f.tracker.ObserveUsage(a, liveCursorUsage(), polled)
	f.tracker.ObservePollResult(a, polled, polled, nil, polled.Add(15*time.Minute))
	f.tracker.SetRefresher(&fakeQuotaRefresher{interval: 15 * time.Minute})

	rec := f.do(t, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if got := keys(out); !reflect.DeepEqual(got, []string{"credentials", "generated_at", "provider", "refresh_interval_seconds", "summary"}) {
		t.Fatalf("top-level keys = %v", got)
	}
	if out["provider"] != "cursor" || out["refresh_interval_seconds"] != float64(900) || out["generated_at"] != "2026-10-21T09:00:00Z" {
		t.Fatalf("top level = %v", out)
	}
	summary := out["summary"].(map[string]any)
	wantSummary := map[string]any{
		"credentials": float64(2), "loaded": float64(1), "window": "included",
		"remaining_percent_sum": 100 - 22.5864, "capacity_percent": float64(100),
		"segments":               []any{map[string]any{"auth_index": f.auths["a"].Index, "remaining_percent": 100 - 22.5864}},
		"on_demand_used_usd_sum": 117.87, "next_reset_at": "2026-10-30T22:31:53Z",
	}
	if !reflect.DeepEqual(summary, wantSummary) {
		t.Fatalf("summary = %v\nwant %v", summary, wantSummary)
	}
	creds := out["credentials"].([]any)
	if len(creds) != 2 {
		t.Fatalf("credentials = %d, want 2 (claude excluded)", len(creds))
	}
	ca := creds[0].(map[string]any)
	wantKeys := []string{"auth_index", "disabled", "email", "extra", "id", "label", "last_poll_at", "name", "next_poll_at", "plan", "poll_error", "poll_started_at", "status", "windows"}
	if got := keys(ca); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("credential keys = %v", got)
	}
	if ca["id"] != "cursor-a.json" || ca["email"] != "a@example.invalid" || ca["last_poll_at"] != "2026-10-21T08:59:00Z" || ca["poll_started_at"] != nil || ca["poll_error"] != "" {
		t.Fatalf("credential a = %v", ca)
	}
	if !reflect.DeepEqual(ca["plan"], map[string]any{"label": "Team", "price": "$40/mo"}) {
		t.Fatalf("plan = %v", ca["plan"])
	}
	windows := ca["windows"].(map[string]any)
	if got := keys(windows); !reflect.DeepEqual(got, []string{"included", "included_api", "included_auto", "on_demand"}) {
		t.Fatalf("window keys = %v", got)
	}
	wantAPI := map[string]any{"kind": "percent", "used_percent": float64(100), "remaining_percent": float64(0),
		"resets_at": "2026-10-30T22:31:53Z", "observed_at": "2026-10-21T08:59:00Z", "source": "poll"}
	if !reflect.DeepEqual(windows["included_api"], wantAPI) {
		t.Fatalf("included_api = %v", windows["included_api"])
	}
	wantOD := map[string]any{"kind": "spend", "limit_kind": "fixed", "limit_scope": "", "used_usd": 117.87, "limit_usd": float64(400),
		"remaining_usd": 400 - 117.87, "used_percent": 117.87 / 400 * 100,
		"resets_at": "2026-10-30T22:31:53Z", "observed_at": "2026-10-21T08:59:00Z", "source": "poll"}
	if !reflect.DeepEqual(windows["on_demand"], wantOD) {
		t.Fatalf("on_demand = %v\nwant %v", windows["on_demand"], wantOD)
	}
	wantExtra := map[string]any{"billing_cycle_start": "2026-09-30T22:31:53Z", "included_spend_usd": float64(20), "included_limit_usd": float64(20),
		"bonus_usd": 262.33, "total_spend_usd": 282.33, "auto_bucket_models": []any{"default", "composer-2"}}
	if !reflect.DeepEqual(ca["extra"], wantExtra) {
		t.Fatalf("extra = %v", ca["extra"])
	}

	// Never polled: every nullable key is present and null.
	cb := creds[1].(map[string]any)
	if got := keys(cb); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("unpolled credential keys = %v", got)
	}
	for _, k := range []string{"plan", "extra", "last_poll_at", "next_poll_at", "poll_started_at"} {
		if cb[k] != nil {
			t.Fatalf("unpolled %s = %v, want null", k, cb[k])
		}
	}
	if !reflect.DeepEqual(cb["windows"], map[string]any{"included": nil, "included_auto": nil, "included_api": nil, "on_demand": nil}) {
		t.Fatalf("unpolled windows = %v", cb["windows"])
	}
}

func TestGetCursorQuotaNonFixedLimitAndEmpty(t *testing.T) {
	f := newCursorQuotaFixture(t)
	rec := f.do(t, http.MethodGet, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"credentials":[]`) || !strings.Contains(rec.Body.String(), `"segments":[]`) {
		t.Fatalf("empty = %d %s", rec.Code, rec.Body.String())
	}
	// No poller: the interval comes from config (default 15m).
	if !strings.Contains(rec.Body.String(), `"refresh_interval_seconds":900`) {
		t.Fatalf("interval without poller: %s", rec.Body.String())
	}

	f = newCursorQuotaFixture(t, "a")
	u := liveCursorUsage()
	u.OnDemand = coreauth.CursorOnDemand{LimitKind: "unlimited", LimitScope: "team_member", UsedUSD: 5}
	u.AutoBucketModels = nil
	f.tracker.ObserveUsage(f.auths["a"].ID, u, quotaTestNow)
	var out struct {
		Credentials []map[string]any `json:"credentials"`
	}
	if err := json.Unmarshal(f.do(t, http.MethodGet, "").Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	od := out.Credentials[0]["windows"].(map[string]any)["on_demand"].(map[string]any)
	if od["limit_kind"] != "unlimited" || od["limit_scope"] != "team_member" || od["used_usd"] != float64(5) ||
		od["limit_usd"] != nil || od["remaining_usd"] != nil || od["used_percent"] != nil {
		t.Fatalf("unlimited on_demand = %v", od)
	}
	if models := out.Credentials[0]["extra"].(map[string]any)["auto_bucket_models"]; !reflect.DeepEqual(models, []any{}) {
		t.Fatalf("auto_bucket_models = %v, want []", models)
	}
}

func TestCursorQuotaNeverLeaksTokens(t *testing.T) {
	f := newCursorQuotaFixture(t, "a", "b")
	f.tracker.ObserveUsage(f.auths["a"].ID, liveCursorUsage(), quotaTestNow)
	f.tracker.ObservePollResult(f.auths["b"].ID, quotaTestNow, quotaTestNow, errTest("unauthorized"), quotaTestNow.Add(15*time.Minute))
	f.tracker.SetRefresher(&fakeQuotaRefresher{})
	for _, rec := range []*httptest.ResponseRecorder{f.do(t, http.MethodGet, ""), f.do(t, http.MethodPost, "{}")} {
		if strings.Contains(rec.Body.String(), "secret") {
			t.Fatalf("response leaks a secret: %s", rec.Body.String())
		}
	}
}

func TestRefreshCursorQuotaErrors(t *testing.T) {
	f := newCursorQuotaFixture(t, "a")
	if rec := f.do(t, http.MethodPost, "{}"); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "cursor quota poller is not running") {
		t.Fatalf("no poller = %d %s", rec.Code, rec.Body.String())
	}
	f.tracker.SetRefresher(&fakeQuotaRefresher{err: &coreauth.QuotaBackoffError{RetryAfter: 90*time.Second + 500*time.Millisecond}})
	rec := f.do(t, http.MethodPost, `{"auth_index":"`+f.auths["a"].Index+`"}`)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"retry_after_seconds":91`) || !strings.Contains(rec.Body.String(), `"error":"backoff"`) {
		t.Fatalf("backoff = %d %s", rec.Code, rec.Body.String())
	}
	if rec = f.do(t, http.MethodPost, `{"auth_index":"nope"}`); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "cursor credential not found") {
		t.Fatalf("unknown index = %d", rec.Code)
	}
	if rec = f.do(t, http.MethodPost, `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
}

func TestRefreshCursorQuotaOneAndAll(t *testing.T) {
	f := newCursorQuotaFixture(t, "a", "b")
	refresher := &fakeQuotaRefresher{interval: 15 * time.Minute}
	refresher.onCall = func(id string) {
		f.tracker.ObserveUsage(id, liveCursorUsage(), quotaTestNow)
		f.tracker.ObservePollResult(id, quotaTestNow, quotaTestNow, nil, quotaTestNow.Add(15*time.Minute))
	}
	f.tracker.SetRefresher(refresher)

	var one struct {
		Credentials []cursorQuotaCredentialJSON `json:"credentials"`
	}
	rec := f.do(t, http.MethodPost, `{"auth_index":"`+f.auths["b"].Index+`"}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(one.Credentials) != 1 || one.Credentials[0].ID != "cursor-b.json" || one.Credentials[0].Windows.Included == nil {
		t.Fatalf("single refresh = %+v", one.Credentials)
	}

	refresher.calls = nil
	rec = f.do(t, http.MethodPost, "")
	var all struct {
		Credentials []cursorQuotaCredentialJSON `json:"credentials"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil || rec.Code != http.StatusOK || len(all.Credentials) != 2 || len(refresher.calls) != 2 {
		t.Fatalf("refresh all status=%d calls=%v body=%s", rec.Code, refresher.calls, rec.Body.String())
	}
}

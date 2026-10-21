package cursor

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// liveCurrentPeriodUsage is the 2026-10-21 live response (docs/cursor/USAGE-TRACKING.md), abridged.
const liveCurrentPeriodUsage = `{
  "billingCycleStart": "1790807513000",
  "billingCycleEnd": "1793399513000",
  "planUsage": {"totalSpend": 28233, "includedSpend": 2000, "bonusSpend": 26233, "limit": 2000,
    "remainingBonus": false, "autoPercentUsed": 7.811428571428572, "apiPercentUsed": 100, "totalPercentUsed": 22.5864},
  "spendLimitUsage": {"totalSpend": 14030, "pooledLimit": "240000", "pooledUsed": 14030, "pooledRemaining": "225970",
    "individualLimit": 40000, "individualUsed": 11787, "individualRemaining": 28213, "limitType": "team"},
  "displayThreshold": 200, "enabled": true,
  "displayMessage": "You've used 33% of your included usage",
  "autoBucketModels": ["default", "composer-2"]
}`

func decodeUsage(t *testing.T, raw string) CurrentPeriodUsage {
	t.Helper()
	var u CurrentPeriodUsage
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return u
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestMapUsageLiveTeamResponse(t *testing.T) {
	u := decodeUsage(t, liveCurrentPeriodUsage)
	if u.NeedsHardLimit() {
		t.Fatal("team account with individualLimit must not need GetHardLimit")
	}
	got, ok := MapUsage(u, nil, time.Time{})
	if !ok {
		t.Fatal("planUsage present, want ok")
	}
	if !near(got.IncludedPercent, 22.5864) || !near(got.AutoPercent, 7.811428571428572) || !near(got.APIPercent, 100) {
		t.Fatalf("percents = %v/%v/%v", got.IncludedPercent, got.AutoPercent, got.APIPercent)
	}
	want := coreauth.CursorOnDemand{LimitKind: coreauth.CursorLimitFixed, UsedUSD: 117.87, LimitUSD: 400}
	if got.OnDemand != want {
		t.Fatalf("on demand = %+v, want %+v", got.OnDemand, want)
	}
	if got.ResetsAt != time.Date(2026, 10, 30, 22, 31, 53, 0, time.UTC) {
		t.Fatalf("resets_at = %v", got.ResetsAt)
	}
	if got.CycleStart != time.Date(2026, 9, 30, 22, 31, 53, 0, time.UTC) {
		t.Fatalf("cycle start = %v", got.CycleStart)
	}
	if !near(got.BonusUSD, 262.33) || !near(got.TotalSpendUSD, 282.33) || !near(got.IncludedSpendUSD, 20) || !near(got.IncludedLimitUSD, 20) {
		t.Fatalf("spend = %+v", got)
	}
	if len(got.AutoBucketModels) != 2 {
		t.Fatalf("auto bucket = %v", got.AutoBucketModels)
	}
}

func TestMapUsageFallbacks(t *testing.T) {
	planEnd := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	u := decodeUsage(t, `{"planUsage": {"includedSpend": 500, "limit": 2000}}`)
	got, ok := MapUsage(u, nil, planEnd)
	if !ok || !near(got.IncludedPercent, 25) || got.AutoPercent != 0 || got.APIPercent != 0 {
		t.Fatalf("fallback percents = %+v ok=%v", got, ok)
	}
	if got.ResetsAt != planEnd {
		t.Fatalf("reset fallback = %v", got.ResetsAt)
	}
	if got.OnDemand.LimitKind != coreauth.CursorLimitUnavailable {
		t.Fatalf("no spend data, no hard limit: kind = %q", got.OnDemand.LimitKind)
	}
	if got.AutoBucketModels == nil {
		t.Fatal("auto bucket models must be [] not nil")
	}
	zero, _ := MapUsage(decodeUsage(t, `{"planUsage": {"includedSpend": 5, "limit": 0}}`), nil, time.Time{})
	if zero.IncludedPercent != 0 || !zero.ResetsAt.IsZero() {
		t.Fatalf("limit 0: %+v", zero)
	}
	if _, ok := MapUsage(decodeUsage(t, `{"spendLimitUsage": {}}`), nil, time.Time{}); ok {
		t.Fatal("no planUsage must report not ok")
	}
}

func TestMapUsageOnDemandRule(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	cases := []struct {
		name  string
		spend *SpendLimitUsage
		hard  *HardLimit
		want  coreauth.CursorOnDemand
	}{
		{"team individual fixed", &SpendLimitUsage{LimitType: "team", IndividualLimit: i64(40000), IndividualUsed: 100}, &HardLimit{HardLimit: 0}, coreauth.CursorOnDemand{LimitKind: "fixed", UsedUSD: 1, LimitUSD: 400}},
		{"team individual zero", &SpendLimitUsage{LimitType: "team", IndividualLimit: i64(0)}, nil, coreauth.CursorOnDemand{LimitKind: "disabled"}},
		{"team hard ok", &SpendLimitUsage{LimitType: "team", IndividualUsed: 250}, &HardLimit{HardLimit: 2400}, coreauth.CursorOnDemand{LimitKind: "unlimited", LimitScope: "team_member", UsedUSD: 2.5}},
		{"team hard disabled", &SpendLimitUsage{LimitType: "team"}, &HardLimit{HardLimit: 2400, NoUsageBasedAllowed: true}, coreauth.CursorOnDemand{LimitKind: "disabled"}},
		{"team hard failed pooled", &SpendLimitUsage{LimitType: "team", PooledLimit: 240000}, nil, coreauth.CursorOnDemand{LimitKind: "unlimited", LimitScope: "team_member"}},
		{"team hard failed no pool", &SpendLimitUsage{LimitType: "team"}, nil, coreauth.CursorOnDemand{LimitKind: "unavailable"}},
		{"personal no hard, individual", &SpendLimitUsage{LimitType: "user", IndividualLimit: i64(5000)}, nil, coreauth.CursorOnDemand{LimitKind: "fixed", LimitUSD: 50}},
		{"personal no hard", &SpendLimitUsage{LimitType: "user"}, nil, coreauth.CursorOnDemand{LimitKind: "unavailable"}},
		{"personal hard overrides individual", &SpendLimitUsage{LimitType: "user", IndividualLimit: i64(5000), IndividualUsed: 1000}, &HardLimit{HardLimit: 200}, coreauth.CursorOnDemand{LimitKind: "fixed", UsedUSD: 10, LimitUSD: 200}},
		{"personal no usage based", &SpendLimitUsage{}, &HardLimit{HardLimit: 200, NoUsageBasedAllowed: true}, coreauth.CursorOnDemand{LimitKind: "disabled"}},
		{"personal unlimited", &SpendLimitUsage{}, &HardLimit{HardLimit: maxHardLimit}, coreauth.CursorOnDemand{LimitKind: "unlimited", LimitScope: "personal"}},
		{"personal zero", &SpendLimitUsage{}, &HardLimit{}, coreauth.CursorOnDemand{LimitKind: "disabled"}},
		{"no spend usage", nil, nil, coreauth.CursorOnDemand{LimitKind: "unavailable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := onDemand(tc.spend, tc.hard); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestInt64AcceptsStringNumberNull(t *testing.T) {
	var v struct {
		A, B, C Int64
	}
	if err := json.Unmarshal([]byte(`{"A":"12","B":34,"C":null}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != 12 || v.B != 34 || v.C != 0 {
		t.Fatalf("got %+v", v)
	}
}

func TestPlanInfo(t *testing.T) {
	var p PlanInfo
	if err := json.Unmarshal([]byte(`{"planInfo":{"planName":"Team","price":"$40/mo","billingCycleEnd":"1793399513000"}}`), &p); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	if got := p.Plan(now); got.Label != "Team" || got.Price != "$40/mo" || got.FetchedAt != now {
		t.Fatalf("plan = %+v", got)
	}
	if p.CycleEnd() != time.Date(2026, 10, 30, 22, 31, 53, 0, time.UTC) {
		t.Fatalf("cycle end = %v", p.CycleEnd())
	}
	if got := (PlanInfo{}).Plan(now); got.Label != "Current plan" {
		t.Fatalf("empty plan label = %q", got.Label)
	}
}

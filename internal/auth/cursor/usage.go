package cursor

import (
	"bytes"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// DashboardService RPC paths (Connect unary, JSON). See docs/cursor/USAGE-TRACKING.md.
const (
	GetCurrentPeriodUsagePath = "/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	GetHardLimitPath          = "/aiserver.v1.DashboardService/GetHardLimit"
	GetPlanInfoPath           = "/aiserver.v1.DashboardService/GetPlanInfo"
)

// maxHardLimit is the int32 sentinel the backend uses for "no personal limit".
const maxHardLimit = 2147483647

// Int64 decodes a proto3 JSON int64, which arrives as a string or a number.
type Int64 int64

// UnmarshalJSON accepts "123", 123 and null.
func (v *Int64) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	raw := strings.Trim(string(data), `"`)
	if raw == "" {
		*v = 0
		return nil
	}
	n, errParse := strconv.ParseInt(raw, 10, 64)
	if errParse != nil {
		f, errFloat := strconv.ParseFloat(raw, 64)
		if errFloat != nil {
			return errParse
		}
		n = int64(f)
	}
	*v = Int64(n)
	return nil
}

// PlanUsage is GetCurrentPeriodUsageResponse.PlanUsage (amounts in cents).
type PlanUsage struct {
	TotalSpend       int64    `json:"totalSpend"`
	IncludedSpend    int64    `json:"includedSpend"`
	BonusSpend       int64    `json:"bonusSpend"`
	Limit            int64    `json:"limit"`
	AutoPercentUsed  *float64 `json:"autoPercentUsed"`
	APIPercentUsed   *float64 `json:"apiPercentUsed"`
	TotalPercentUsed *float64 `json:"totalPercentUsed"`
}

// SpendLimitUsage is GetCurrentPeriodUsageResponse.SpendLimitUsage (amounts in cents).
type SpendLimitUsage struct {
	TotalSpend      int64  `json:"totalSpend"`
	PooledLimit     Int64  `json:"pooledLimit"`
	IndividualLimit *int64 `json:"individualLimit"`
	IndividualUsed  int64  `json:"individualUsed"`
	LimitType       string `json:"limitType"`
}

// CurrentPeriodUsage is the GetCurrentPeriodUsage response subset the proxy reads.
type CurrentPeriodUsage struct {
	BillingCycleStart Int64            `json:"billingCycleStart"`
	BillingCycleEnd   Int64            `json:"billingCycleEnd"`
	PlanUsage         *PlanUsage       `json:"planUsage"`
	SpendLimitUsage   *SpendLimitUsage `json:"spendLimitUsage"`
	AutoBucketModels  []string         `json:"autoBucketModels"`
}

// HardLimit is the GetHardLimit response subset (hardLimit in dollars).
type HardLimit struct {
	HardLimit           int64 `json:"hardLimit"`
	NoUsageBasedAllowed bool  `json:"noUsageBasedAllowed"`
}

// PlanInfo is the GetPlanInfo response subset.
type PlanInfo struct {
	PlanInfo *struct {
		PlanName        string `json:"planName"`
		Price           string `json:"price"`
		BillingCycleEnd Int64  `json:"billingCycleEnd"`
	} `json:"planInfo"`
}

// Plan converts the response to the tracker's plan record.
func (p PlanInfo) Plan(now time.Time) coreauth.CursorQuotaPlan {
	plan := coreauth.CursorQuotaPlan{Label: "Current plan", FetchedAt: now}
	if p.PlanInfo != nil {
		if name := strings.TrimSpace(p.PlanInfo.PlanName); name != "" {
			plan.Label = name
		}
		plan.Price = strings.TrimSpace(p.PlanInfo.Price)
	}
	return plan
}

// CycleEnd returns planInfo.billingCycleEnd, or the zero time.
func (p *PlanInfo) CycleEnd() time.Time {
	if p == nil || p.PlanInfo == nil {
		return time.Time{}
	}
	return msTime(p.PlanInfo.BillingCycleEnd)
}

// NeedsHardLimit reports whether the on-demand limit depends on GetHardLimit.
// Only a team account with an individual limit is decided without it.
func (u CurrentPeriodUsage) NeedsHardLimit() bool {
	s := u.SpendLimitUsage
	return s == nil || s.LimitType != "team" || s.IndividualLimit == nil
}

// MapUsage reproduces the agent CLI's /usage model (src/usage/usage-data.ts).
// hardLimit is nil when GetHardLimit failed or was not called; planCycleEnd is
// the GetPlanInfo fallback for the reset date. ok is false when the response
// has no planUsage (Enterprise view, unsupported).
func MapUsage(u CurrentPeriodUsage, hardLimit *HardLimit, planCycleEnd time.Time) (coreauth.CursorUsage, bool) {
	if u.PlanUsage == nil {
		return coreauth.CursorUsage{}, false
	}
	p := u.PlanUsage
	out := coreauth.CursorUsage{
		CycleStart:       msTime(u.BillingCycleStart),
		ResetsAt:         msTime(u.BillingCycleEnd),
		IncludedSpendUSD: cents(p.IncludedSpend),
		IncludedLimitUSD: cents(p.Limit),
		BonusUSD:         cents(p.BonusSpend),
		TotalSpendUSD:    cents(p.TotalSpend),
		AutoBucketModels: append([]string{}, u.AutoBucketModels...),
	}
	if out.ResetsAt.IsZero() {
		out.ResetsAt = planCycleEnd
	}
	switch {
	case p.TotalPercentUsed != nil:
		out.IncludedPercent = *p.TotalPercentUsed
	case p.Limit > 0:
		out.IncludedPercent = float64(p.IncludedSpend) / float64(p.Limit) * 100
	}
	if p.AutoPercentUsed != nil {
		out.AutoPercent = *p.AutoPercentUsed
	}
	if p.APIPercentUsed != nil {
		out.APIPercent = *p.APIPercentUsed
	}
	out.OnDemand = onDemand(u.SpendLimitUsage, hardLimit)
	return out, true
}

// onDemand is the CLI's u() limit rule.
func onDemand(s *SpendLimitUsage, hard *HardLimit) coreauth.CursorOnDemand {
	var spend SpendLimitUsage
	if s != nil {
		spend = *s
	}
	used := cents(spend.IndividualUsed)
	fixed := func(limit float64) coreauth.CursorOnDemand {
		return coreauth.CursorOnDemand{LimitKind: coreauth.CursorLimitFixed, UsedUSD: used, LimitUSD: limit}
	}
	kind := func(k, scope string) coreauth.CursorOnDemand {
		return coreauth.CursorOnDemand{LimitKind: k, LimitScope: scope, UsedUSD: used}
	}
	var individual *coreauth.CursorOnDemand
	if spend.IndividualLimit != nil {
		v := kind(coreauth.CursorLimitDisabled, "")
		if *spend.IndividualLimit > 0 {
			v = fixed(cents(*spend.IndividualLimit))
		}
		individual = &v
	}
	if spend.LimitType == "team" {
		switch {
		case individual != nil:
			return *individual
		case hard != nil:
			if hard.NoUsageBasedAllowed || hard.HardLimit <= 0 {
				return kind(coreauth.CursorLimitDisabled, "")
			}
			return kind(coreauth.CursorLimitUnlimited, coreauth.CursorLimitScopeTeamMember)
		case spend.PooledLimit > 0:
			return kind(coreauth.CursorLimitUnlimited, coreauth.CursorLimitScopeTeamMember)
		default:
			return kind(coreauth.CursorLimitUnavailable, "")
		}
	}
	switch {
	case hard == nil:
		if individual != nil {
			return *individual
		}
		return kind(coreauth.CursorLimitUnavailable, "")
	case hard.NoUsageBasedAllowed:
		return kind(coreauth.CursorLimitDisabled, "")
	case hard.HardLimit >= maxHardLimit:
		return kind(coreauth.CursorLimitUnlimited, coreauth.CursorLimitScopePersonal)
	case hard.HardLimit > 0:
		return fixed(float64(hard.HardLimit))
	default:
		return kind(coreauth.CursorLimitDisabled, "")
	}
}

func cents(v int64) float64 {
	return float64(v) / 100
}

func msTime(ms Int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(ms)).UTC()
}

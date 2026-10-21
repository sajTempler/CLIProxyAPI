# Cursor usage tracking: design

Goal: show what the agent CLI's `/usage` view shows (monthly Included % with Auto and API
buckets, On-Demand $spent/$limit, reset date) per Cursor account, the same way the Claude quota
work on `feat/quota-routing` shows 5h/7d windows. Design only; nothing here is implemented yet.

Evidence: agent CLI 2026.10.19-6bbf2c4 (`544.index.js`, module `./src/usage/usage-data.ts`, and
`index.js`, `../cursor-config/dist/dashboard.js`), plus 3 live calls on 2026-10-21 with the
keychain JWT (Team plan account). Probe script kept out of the repo; values below are real,
identifiers redacted.

## 1. The RPCs

All three are Connect **unary** RPCs on the same backend the auth code uses:

```
POST https://api2.cursor.sh/aiserver.v1.DashboardService/<Method>
Authorization: Bearer <session JWT>          # access_token from the cursor auth file
Content-Type: application/json
Connect-Protocol-Version: 1
x-cursor-client-type: cli
x-cursor-client-version: <agent version>     # optional for these calls; sent by the probe
body: {}
```

The CLI's `/usage` command runs these in parallel with a 10 s timeout each:

| Method | Request | Needed for |
|---|---|---|
| `GetCurrentPeriodUsage` | `GetCurrentPeriodUsageRequest{team_id?, include_pooled_usage?}`, sent empty | everything except the plan name and the personal on-demand limit |
| `GetHardLimit` | `GetHardLimitRequest{team_id?, personal_scope?}`, sent empty; failure tolerated | On-Demand limit when `spend_limit_usage` has no `individual_limit` |
| `GetPlanInfo` | `GetPlanInfoRequest{}`; failure tolerated | plan name ("Team", "Pro", ...); fallback `billing_cycle_end` |

JSON uses proto3 JSON names (camelCase). **int64 fields arrive as strings** (`billingCycleStart`,
`billingCycleEnd`, `pooledLimit`, `pooledRemaining`); int32 fields are numbers.

### Live response (abridged, 200)

```json
// GetCurrentPeriodUsage
{
  "billingCycleStart": "1790807513000",            // ms, 2026-09-30T22:31:53Z
  "billingCycleEnd":   "1793399513000",            // ms, 2026-10-30T22:31:53Z
  "planUsage": {
    "totalSpend": 28233, "includedSpend": 2000, "bonusSpend": 26233, "limit": 2000,   // cents
    "remainingBonus": false, "bonusTooltip": "We work with model providers to give you free usage ...",
    "autoPercentUsed": 7.811428571428572,
    "apiPercentUsed": 100,
    "totalPercentUsed": 22.5864
  },
  "spendLimitUsage": {
    "totalSpend": 14030, "pooledLimit": "240000", "pooledUsed": 14030, "pooledRemaining": "225970",
    "individualLimit": 40000, "individualUsed": 11787, "individualRemaining": 28213,  // cents
    "limitType": "team"
  },
  "displayThreshold": 200, "enabled": true,
  "displayMessage": "You've used 33% of your included usage",
  "autoModelSelectedDisplayMessage": "You've used 23% of your included total usage",
  "namedModelSelectedDisplayMessage": "You've used 100% of your included API usage",
  "autoBucketModels": ["default", "composer-2", "composer-2.5", "grok-4.7", "..."]   // 48 ids
}
// GetHardLimit
{ "hardLimit": 2400 }                               // dollars (team pool), not cents
// GetPlanInfo
{ "planInfo": { "planName": "Team", "price": "$40/mo", "billingCycleEnd": "1793399513000", "includedUsagePeriod": "..." } }
```

Full schemas (from the bundle's descriptor strings):

```
GetCurrentPeriodUsageResponse|1 billing_cycle_start 3|2 billing_cycle_end 3|3 plan_usage #0|4 spend_limit_usage #1|5 display_threshold 5?|6 enabled 8|7 display_message 9|8 free_best_of_n_promotion #2?|11 auto_model_selected_display_message 9?|12 named_model_selected_display_message 9?|13 auto_bucket_models 9*
GetCurrentPeriodUsageResponse.PlanUsage|1 total_spend 5|2 included_spend 5|3 bonus_spend 5|4 remaining 5|5 limit 5|6 remaining_bonus 8?|7 bonus_tooltip 9?|8 auto_spend 5?|9 api_spend 5?|10 auto_limit 5?|11 api_limit 5?|12 auto_percent_used 1?|13 api_percent_used 1?|14 total_percent_used 1?
GetCurrentPeriodUsageResponse.SpendLimitUsage|1 total_spend 5|2 pooled_limit 3?|3 pooled_used 5?|4 pooled_remaining 3?|5 individual_limit 5?|6 individual_used 5|7 individual_remaining 5|8 limit_type 9|9 overall_limit 5?|10 overall_used 5?|11 overall_remaining 5?
GetHardLimitResponse|1 hard_limit 5|2 no_usage_based_allowed 8|3 hard_limit_per_user 5?|4 per_user_monthly_limit_dollars 5|...
```

## 2. Field → `/usage` row mapping

This is the CLI's own logic (`usage-data.ts`), so our numbers match what the user sees.

| `/usage` row | Source | Live value |
|---|---|---|
| Plan name | `GetPlanInfo.planInfo.planName`, else "Current plan" | Team |
| **Included %** | `planUsage.totalPercentUsed`; if absent `includedSpend / limit * 100` (0 when `limit == 0`) | 22.6 % (CLI shows 23 %) |
| **Auto %** | `planUsage.autoPercentUsed ?? 0` | 7.8 % |
| **API %** | `planUsage.apiPercentUsed ?? 0` | 100 % |
| **On-Demand $x** | `spendLimitUsage.individualUsed / 100` | $117.87 |
| **On-Demand / $y** | see the rule below | $400 |
| **Resets date** | `billingCycleEnd` (ms) if > 0, else `planInfo.billingCycleEnd`; formatted "Resets MMM d" in **UTC** | Resets Oct 30 |

On-Demand limit rule (`u()` in the bundle), with `n = individualUsed/100`:
1. If `spendLimitUsage.individualLimit` is present: `> 0` → **fixed** `$n / $(individualLimit/100)`; `0` → **disabled**.
2. Else if `limitType == "team"`: `GetHardLimit` ok → `noUsageBasedAllowed || hardLimit <= 0` → disabled, otherwise **unlimited** (team member); `GetHardLimit` failed → unlimited if `pooledLimit > 0`, else unavailable.
3. Else (personal): no `GetHardLimit` → unavailable; `noUsageBasedAllowed` → disabled; `hardLimit >= 2147483647` → unlimited; `hardLimit > 0` → fixed `$n / $hardLimit` (dollars, no /100); else disabled.

Notes:
- Do **not** use `displayMessage` for the percentage: live it said 33 % while the CLI showed
  `totalPercentUsed` (23 %). The two auto/named messages match the bucket numbers.
- `bonusSpend` is free usage beyond the plan ("Amounts may vary"); `totalSpend` = included + bonus.
  Worth showing as a secondary line, not as a quota.
- `autoBucketModels` lists the model ids billed to the Auto bucket; everything else hits the API
  bucket. Useful later for routing (an account at API 100 % can still serve `composer-*`).
- If `planUsage` is absent the CLI switches to an Enterprise spend view (`GetMe`,
  `GetMonthlyBillingCycle`, `GetAggregatedUsageEvents`). Out of scope: report `unavailable`.
- The cycle is 30 days anchored to the subscription time (22:31:53Z here), not a calendar month.
- 401 / Connect `unauthenticated` means the JWT is invalid or expired → "re-login".

## 3. Poll cadence

There is no passive signal: Cursor's Run stream carries no quota headers that we know of, so
polling is the only source (unlike Claude, where headers are primary).

- Default interval **15 min** ± 10 % jitter, 1 min floor, per account; the values move slowly
  (monthly window) and the CLI itself only calls on demand. Manual refresh always allowed.
- `GetCurrentPeriodUsage` on every poll. `GetPlanInfo` on the first poll and then every 24 h.
  `GetHardLimit` only when rule 1 above does not apply (no `individualLimit`), every 6 h.
  Steady state: 4 calls/hour/account.
- Poll only accounts with `type: "cursor"`, not disabled, with `access_token` and `expired` in the
  future; otherwise `poll_error = "token_expired"` (the auth layer owns re-login warnings).
- 429 / `resource_exhausted`: back off `retry-after` or 5 min, capped at 1 h (same as Claude).
  401: `poll_error = "unauthorized"`, next poll at `now + interval`.
- Opt-in like Claude's D7: poll when `routing.cursor-quota.refresh-interval` is set or when the
  quota page asks for it; default off for upstream friendliness, on in this fork's config.
- No HTTP client timeout after connect (AGENTS.md), single-flight per account, cancel on shutdown.
  Credential acquisition is the only place allowed a timeout, so a probe-style 10 s timeout is
  **not** allowed here.

## 4. Plugging into the Claude quota design (`feat/quota-routing`)

What exists there: `ClaudeQuotaTracker` (`sdk/cliproxy/auth/claude_quota.go`), `internal/claudequota`
poller, `GET /v8/management/quota/claude` + `POST /v8/management/quota/claude/refresh`, and an MC
fork whose Claude quota tab reads that endpoint.

Recommendation: **generalize the HTTP surface, keep the trackers per provider.**

1. **Routes.** Register `GET /v8/management/quota/:provider` and `POST /v8/management/quota/:provider/refresh`
   dispatching through a small registry `map[string]QuotaView` where
   ```go
   type QuotaView interface {
       Snapshot(ctx context.Context) (any, error)                    // full provider payload
       Refresh(ctx context.Context, authIndex string) (any, error)   // "" = all
   }
   ```
   `claude` registers the existing handlers unchanged (the literal `/quota/claude` routes can stay
   as aliases), `cursor` registers the new one. Unknown provider → 404 `provider_not_found`.
2. **Shared envelope, provider-specific windows.** Same top level as Claude
   (`provider`, `generated_at`, `refresh_interval_seconds`, `summary`, `credentials[]` with
   `auth_index`, `id`, `email`, `status`, `last_poll_at`, `next_poll_at`, `poll_error`), so the MC
   card/header components are reused. Windows gain a `kind`:
   ```json
   "plan": {"label": "Team", "price": "$40/mo"},
   "windows": {
     "included":      {"kind": "percent", "used_percent": 22.59, "remaining_percent": 77.41, "resets_at": "2026-10-30T22:31:53Z", "source": "poll"},
     "included_auto": {"kind": "percent", "used_percent": 7.81,  "remaining_percent": 92.19, "resets_at": "2026-10-30T22:31:53Z"},
     "included_api":  {"kind": "percent", "used_percent": 100,   "remaining_percent": 0,     "resets_at": "2026-10-30T22:31:53Z"},
     "on_demand":     {"kind": "spend", "limit_kind": "fixed", "used_usd": 117.87, "limit_usd": 400, "used_percent": 29.47, "resets_at": "2026-10-30T22:31:53Z"}
   },
   "extra": {"bonus_usd": 262.33, "auto_bucket_models": ["composer-2", "..."]}
   ```
   `limit_kind` ∈ `fixed | unlimited | disabled | unavailable`; `used_percent` only for `fixed`.
   Summary: `included` remaining sum and `next_reset_at` (earliest `billingCycleEnd`).
3. **Code layout (mirrors Claude, no executor dependency).**
   - `internal/auth/cursor/usage.go`: `FetchCurrentPeriodUsage`, `FetchHardLimit`, `FetchPlanInfo`
     on `CursorAuth` (it already has the proxy-aware client and Connect headers), plus a pure
     `MapUsage(cpu, hardLimit, plan) CursorUsage` implementing section 2, table-tested against the
     live JSON above.
   - `sdk/cliproxy/auth/cursor_quota.go`: `CursorQuotaTracker` (mutex, `byAuth`, `Snapshot`,
     `Forget`, `RequestPoll`), same shape as `ClaudeQuotaTracker` minus headers.
   - `internal/cursorquota/poller.go`: copy of `internal/claudequota` with the cadence of section 3.
   - `management/cursor_quota.go` + the registry hookup; `routing.cursor-quota.refresh-interval` config.
   - MC fork: a `cursor` tab in `QUOTA_TAB_ORDER` with a body showing Included (with Auto/API
     sub-bars), On-Demand `$x / $y`, and "Resets Oct 30".
4. **Routing (later, optional).** `soonest-reset` does not transfer (monthly windows, one per
   account). A useful Cursor rule is "skip an account for API-bucket models when `included_api` is
   at 100 % and `on_demand` is disabled or at its limit", which needs `autoBucketModels`. Phase 4+.

Ordering: this needs only Phase 3 (cursor auth files) and the quota-routing merge; it does **not**
need the Cursor executor (Phase 4), so usage can show in the dashboard before inference works.
The poller should still not run for cursor auths the user disabled.

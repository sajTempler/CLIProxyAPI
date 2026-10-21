# Cursor usage tracking: design

Goal: show what the agent CLI's `/usage` view shows (monthly Included % with Auto and API
buckets, On-Demand $spent/$limit, reset date) per Cursor account, the same way the Claude quota
work on `feat/quota-routing` shows 5h/7d windows. Sections 1-4 are the design; section 5 is the
fixed endpoint contract the dashboard builds against.

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
  `GetHardLimit` every 6 h, skipped when `limitType == "team"` and `individualLimit` is present
  (rule 1 decides alone there; for personal accounts the CLI lets `GetHardLimit` override
  `individualLimit`, so it is always needed). Steady state: 4 calls/hour/account.
- Poll only accounts with `type: "cursor"`, not disabled, with `access_token` and `expired` in the
  future; otherwise `poll_error = "token_expired"` (the auth layer owns re-login warnings).
- 429 / `resource_exhausted`: back off `retry-after` or 5 min, capped at 1 h (same as Claude).
  401: `poll_error = "unauthorized"`, next poll at `now + interval`.
- Config `routing.cursor-quota.refresh-interval`: unset/`""` = 15 min (only cursor auth files are
  polled, and they exist only after a deliberate login/import), `"0"` = no background polling
  (manual refresh still works), below 1 min clamps to 1 min. Hot-reloaded.
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

## 5. Endpoint contract (fixed)

Branch `feat/cursor-usage`. This section wins over the sketch in section 4.2. Display-only: no
routing data. `GET /v8/management/quota/claude` and its refresh are unchanged.

### `GET /v8/management/quota/cursor`

Management auth like every `/v8/management` route. Always 200 (an empty `credentials` array when
there are no cursor auth files); 503 `{"error": "core auth manager unavailable"}` only if the
server has no auth manager.

```json
{
  "provider": "cursor",
  "generated_at": "2026-10-21T09:00:00Z",
  "refresh_interval_seconds": 900,
  "summary": {
    "credentials": 1,
    "loaded": 1,
    "window": "included",
    "remaining_percent_sum": 77.41,
    "capacity_percent": 100,
    "segments": [{"auth_index": "3", "remaining_percent": 77.41}],
    "on_demand_used_usd_sum": 117.87,
    "next_reset_at": "2026-10-30T22:31:53Z"
  },
  "credentials": [
    {
      "auth_index": "3",
      "id": "cursor-user@example.com.json",
      "name": "cursor-user@example.com.json",
      "email": "user@example.com",
      "label": "",
      "disabled": false,
      "status": "active",
      "plan": {"label": "Team", "price": "$40/mo"},
      "windows": {
        "included":      {"kind": "percent", "used_percent": 22.5864, "remaining_percent": 77.4136, "resets_at": "2026-10-30T22:31:53Z", "observed_at": "2026-10-21T08:59:58Z", "source": "poll"},
        "included_auto": {"kind": "percent", "used_percent": 7.8114,  "remaining_percent": 92.1886, "resets_at": "2026-10-30T22:31:53Z", "observed_at": "2026-10-21T08:59:58Z", "source": "poll"},
        "included_api":  {"kind": "percent", "used_percent": 100,     "remaining_percent": 0,       "resets_at": "2026-10-30T22:31:53Z", "observed_at": "2026-10-21T08:59:58Z", "source": "poll"},
        "on_demand": {
          "kind": "spend", "limit_kind": "fixed", "limit_scope": "",
          "used_usd": 117.87, "limit_usd": 400, "remaining_usd": 282.13, "used_percent": 29.4675,
          "resets_at": "2026-10-30T22:31:53Z", "observed_at": "2026-10-21T08:59:58Z", "source": "poll"
        }
      },
      "extra": {
        "billing_cycle_start": "2026-09-30T22:31:53Z",
        "included_spend_usd": 20, "included_limit_usd": 20, "bonus_usd": 262.33, "total_spend_usd": 282.33,
        "auto_bucket_models": ["default", "composer-2"]
      },
      "last_poll_at": "2026-10-21T08:59:58Z",
      "next_poll_at": "2026-10-21T09:14:10Z",
      "poll_started_at": null,
      "poll_error": ""
    }
  ]
}
```

Rules (every key is always present; "nullable" means the value is JSON `null`, never omitted):

- Times: RFC 3339 UTC strings or `null`. Money: US dollars as JSON numbers (cents / 100, not
  rounded). Percentages: 0-100 floats, not rounded and not clamped (upstream may exceed 100).
- `refresh_interval_seconds`: background poll interval of the running poller; `0` = background
  polling off (manual refresh still works).
- `credentials[]`: every auth with `type: "cursor"` (including disabled ones), ordered by file
  name. `auth_index`, `id`, `name`, `email`, `label`, `disabled`, `status` mean the same as in the
  Claude endpoint; `email` is full (the UI masks it) and `""` when unknown.
- `plan`: nullable until `GetPlanInfo` succeeded once. `label` = `planName` or `"Current plan"`;
  `price` = upstream string or `""`.
- `windows`: always an object with exactly the four keys `included`, `included_auto`,
  `included_api`, `on_demand` (display in that order). Each value is `null` until the first
  successful poll; after that, values persist through later failed polls (stale data keeps its
  `observed_at`; `poll_error` tells the UI it is stale).
- Percent windows (`kind: "percent"`): `used_percent` per section 2 (`included` =
  `totalPercentUsed`, else `includedSpend / limit * 100`, else 0; `included_auto`/`included_api` =
  `autoPercentUsed`/`apiPercentUsed`, missing = 0). `remaining_percent = max(0, 100 - used_percent)`.
  To match the CLI, render percentages as `round(clamp(p, 0, 100))`, except values in (0, 1)
  render as `1%`.
- `resets_at`: `billingCycleEnd`, else `planInfo.billingCycleEnd`, else `null` (CLI: "Current
  period"). The CLI formats it as `Resets MMM d` in **UTC**. Same value in all four windows.
- `observed_at`: when the poll that produced the value finished. `source`: always `"poll"`.
- On-demand window (`kind: "spend"`): `limit_kind` ∈ `fixed | unlimited | disabled | unavailable`
  per the section 2 rule. `limit_scope` is `"team_member"` or `"personal"` for `unlimited` (CLI
  detail "No personal limit" / "No monthly limit"), else `""`. `used_usd` =
  `individualUsed / 100` (0 when missing), always a number. `limit_usd`, `remaining_usd`
  (`max(0, limit - used)`) and `used_percent` (`used / limit * 100`) are numbers only for
  `fixed`, otherwise `null`. CLI rendering: `fixed` → `$used / $limit` + `$remaining remaining`;
  other kinds → `$used` when > 0, else `Disabled` / `Unavailable` (`unlimited` always shows
  `$used`); dollars use 0 decimals when integral, else 2.
- `extra`: nullable until the first successful poll. `billing_cycle_start` nullable;
  `included_spend_usd`, `included_limit_usd`, `bonus_usd`, `total_spend_usd` come from `planUsage`
  (`includedSpend`, `limit`, `bonusSpend`, `totalSpend`, cents / 100); `auto_bucket_models` is a
  string array (`[]` when missing).
- `last_poll_at` (last successful poll), `next_poll_at`, `poll_started_at` (non-null while a poll
  is in flight): nullable times.
- `poll_error`: `""` after a successful poll, else one fixed code: `token_expired` (the auth
  file's `expired` is in the past; no call made, the user must re-login), `no_access_token`,
  `unauthorized` (401 / Connect `unauthenticated`), `rate_limited (429)` / `rate_limited (403)`
  (backing off), `request_failed` (network), `invalid_response`, `http_<status>`,
  `usage_unavailable` (no `planUsage`, e.g. Enterprise; windows stay `null`).
- `summary`: `credentials` = number of entries; `loaded` = entries whose `included` window is
  non-null; `window` is always `"included"`; `remaining_percent_sum` / `capacity_percent` (100 per
  loaded entry) and `segments` cover loaded entries only; `on_demand_used_usd_sum` sums `used_usd`
  over loaded entries; `next_reset_at` = earliest future `resets_at` among loaded entries, else
  `null`.

### `POST /v8/management/quota/cursor/refresh`

Body `{"auth_index": "3"}` refreshes one credential, `{}` or an empty body refreshes every
enabled cursor credential (concurrently). The call is synchronous and returns
`200 {"credentials": [<entry as above>...]}` with the refreshed entries only.

- Unknown `auth_index` → `404 {"error": "cursor credential not found"}`.
- Malformed JSON → `400 {"error": "invalid body"}`.
- Single credential in backoff (after 429/403) → `429 {"error": "backoff", "retry_after_seconds": N}`
  without an upstream call. In the all-credentials form, per-credential failures (backoff, 401,
  expired token) show up only in each entry's `poll_error`.
- Poller not running → `503 {"error": "cursor quota poller is not running"}`.
- Concurrent refreshes of one credential share a single upstream call.

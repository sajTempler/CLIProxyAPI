# Quota routing: implementation spec

Branch `feat/quota-routing` (worktree `.worktrees/quota-routing`). Requirements: `SOURCE.md`.
Progress log: `STATUS.md`. Repo rules: `AGENTS.md`.

Scope: Change 1 (`soonest-reset` routing) and Change 3 (quota dashboard). Change 2 (session
affinity) is config-only and already live; this spec only makes Change 1 compose with it.

## 1. Findings (evidence)

### 1.1 Usage data sources for Claude OAuth accounts

**(a) OAuth usage endpoint (what Claude Code's `/usage` reads).** Verified live on 2026-10-21.

- Request: `GET https://api.anthropic.com/api/oauth/usage`
  - `Authorization: Bearer <oauth access_token>`
  - `anthropic-beta: oauth-2025-04-20`
  - `User-Agent: claude-cli/<ver> (external, cli)` (Management Center sends `claude-cli/2.1.280 (external, cli)`)
  - Claude Code 2.1.306 also uses the variants `?at_wall=1&skip_spend=1` and `?cedar_ember=1&skip_spend=1`.
    The plain path returns everything we need.
- Source in the binary (`~/.local/share/claude/versions/2.1.306`): `BASE_API_URL:"https://api.anthropic.com"`,
  `ep="oauth-2025-04-20"`, the path switch `case"plain":return"/api/oauth/usage"`, and the
  usage keys `["five_hour","seven_day","seven_day_oauth_apps","seven_day_opus","seven_day_sonnet",…,"extra_usage","limits"]`.
- Live response (200, abridged, one account):

```json
{
  "five_hour":  {"utilization": 10.0, "resets_at": "2026-10-21T10:29:59.551785+00:00", "...": null},
  "seven_day":  {"utilization": 56.0, "resets_at": "2026-10-22T03:59:59.551812+00:00"},
  "seven_day_opus": null, "seven_day_sonnet": null, "seven_day_oauth_apps": null,
  "extra_usage": {"is_enabled": false, "disabled_reason": "out_of_credits", "currency": "EUR"},
  "limits": [
    {"kind": "session",       "group": "session", "percent": 10, "resets_at": "2026-10-21T10:29:59.551785+00:00", "scope": null, "is_active": false},
    {"kind": "weekly_all",    "group": "weekly",  "percent": 56, "resets_at": "2026-10-22T03:59:59.551812+00:00", "scope": null, "is_active": true},
    {"kind": "weekly_scoped", "group": "weekly",  "percent": 47, "resets_at": "2026-10-22T03:59:59.552041+00:00",
     "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": false}
  ],
  "weekly_scoped_shares": [{"limit_index": 2, "used_percent_of_weekly": 24, "allowance_percent_of_weekly": 50}]
}
```

- Field mapping (endpoint values are whole percent USED, 0–100; `resets_at` ISO-8601 or null):
  - 5-hour: `five_hour.utilization` / `five_hour.resets_at` (same as `limits[kind=session]`).
  - 7-day overall: `seven_day.*` (same as `limits[kind=weekly_all]`).
  - **7-day Fable: there is no `seven_day_fable` key.** It is `limits[]` where
    `kind == "weekly_scoped"` and `scope.model.display_name` is `Fable` (case-insensitive; the
    binary's allowlist is `["Fable","Fable 5","Fable 5.1"]`). Management Center matches the same
    way and falls back to the `iguana_necktie` window.
  - 7-day Opus (`seven_day_opus`): ignored per SOURCE.md.
- Plan: `GET https://api.anthropic.com/api/oauth/profile` (same headers). Live fields:
  `organization.organization_type` (`claude_team`), `organization.rate_limit_tier`
  (`default_claude_max_5x`), `organization.subscription_status`, `account.has_claude_max`,
  `account.has_claude_pro`. The auth file has no plan field.
- Rate limiting: Claude Code treats a 429 or 403 from this endpoint as "remembered for this
  bearer; not asking again" for `min(retry-after || 5 min, 1 h)` (`Pt=300000`, `Ge=3600000`).
  It is not a free endpoint; poll it sparingly.

**(b) Passive `anthropic-ratelimit-unified-*` headers.** Present on every Claude OAuth
`/v1/messages` response, and upstream already stores them (see 1.2). Read live from
`GET /v8/management/credentials` (`quota.signals` and `model_quotas.<model>.signals`):

```
Anthropic-Ratelimit-Unified-5h-Utilization: 0.1        (fraction used)
Anthropic-Ratelimit-Unified-5h-Reset:       1792578600 (unix s = 2026-10-21T10:30:00Z)
Anthropic-Ratelimit-Unified-5h-Status:      allowed    (allowed | allowed_warning | rejected)
Anthropic-Ratelimit-Unified-7d-Utilization: 0.55
Anthropic-Ratelimit-Unified-7d-Reset:       1792641600 (= 2026-10-22T04:00:00Z)
Anthropic-Ratelimit-Unified-7d_oi-Utilization: 0.46    <- ONLY on Fable responses
Anthropic-Ratelimit-Unified-7d_oi-Reset:       1792641600
Anthropic-Ratelimit-Unified-7d_oi-Status:      allowed
Anthropic-Ratelimit-Unified-Status / -Reset / -Representative-Claim (five_hour) / -Overage-* / -Fallback-Percentage
```

- `7d_oi` is the Fable bucket. In Claude Code the header-to-window table is
  `[["five_hour","5h"],["seven_day","7d"],["seven_day_overage_included","7d_oi"],["overage","overage"]]`,
  and the label is `seven_day_overage_included:"Fable limit"`. Claude Code builds its own Fable
  row from this header (`kind:"weekly_scoped"`, `scope.model.display_name` from the allowlist).
- Live check: an Opus request's headers had no `7d_oi-*`. A one-token `claude-fable-5-1` request
  produced `7d_oi` utilization 0.46, against 47 % from the endpoint. Header 5h/7d resets match the
  endpoint `resets_at` to the second. Utilization differs by up to 1 point (rounding).
- The proxy does not forward these headers downstream. It records them in `Auth.Quota.Signals`.

**Verdict.**
- Use the passive headers as the primary source. They are free, fresh on every request, and give
  5h and 7d. They also give 7d Fable, but only when that account recently served a Fable request.
- Headers alone are not enough:
  1. An idle account (no traffic since start, or since a reset) has no data. That is exactly the
     account `soonest-reset` must rank.
  2. The Fable window shows up only on Fable traffic.
  3. The upstream auth-level snapshot is replaced per response, so an Opus response erases the Fable
     window at auth level. It survives only in `ModelStates["claude-fable-5-1"]`.
- Add a low-rate poller on the OAuth usage endpoint for each Claude OAuth account. Default interval
  10 min, with ±10 % jitter and a 1 min floor. Skip a poll when passive data for all three windows is
  newer than the interval. On 429/403 back off for `retry-after`, or 5 min if absent, capped at 1 h
  (the same as Claude Code). Fetch the profile (plan) on the first poll and then every 24 h.
- With N accounts and mostly-passive coverage this costs at most 6·N usage calls/hour, and usually
  fewer.

### 1.2 What upstream already has (reuse over new code)

| Need | Already in upstream | Reuse |
|---|---|---|
| Parse unified headers | `internal/runtime/executor/helps/claude_ratelimit.go` already reads 5h/7d/7d_oi status, reset and utilization (it calls 7d_oi "Fable-specific"). | Copy only the header names and the unix/RFC3339 parsing idea. The auth package must not import executor helps. |
| Store passive headers | `sdk/cliproxy/auth/quota_signals.go` `QuotaState.ObserveResponseHeadersForProvider`, called in `conductor_cooldown.go:1333` for the auth and the model state on every result. `ProviderSupportsQuotaObservation("claude")` is true. | Hook the new tracker in at this exact call site. Leave the snapshot semantics alone. |
| Expose passive headers | `GET /v8/management/credentials` returns `quota.signals` and `model_quotas` (`internal/api/handlers/management/auth_files.go:683`). | Leave as is. The new endpoint is the normalized view. |
| 429 cooldown | `ParseClaudeRateLimitResetForError` and `ClaudeHeadersIndicateUnifiedRateLimitRejection` (`claude_executor_request.go:817`, `claude_executor_fast_error.go:149`) set a credential-scope cooldown until the rejected window resets (+1–30 s fuzz). The selector filter `getSelectorAvailableAuths` / `isAuthBlockedForModel` excludes cooled auths. `upstream.claude.model-level-cooling` scopes it per model. | **The 429 cooldown that SOURCE.md asks for already exists.** No new cooldown code. The tracker also records `status=rejected` from the same headers. |
| Deterministic "burn one account first" | `FillFirstSelector` (`selector.go:72,812`), wired in `sdk/cliproxy/service_config.go:79,105`, `management/config_basic.go:319`, `scheduler.go:169`, `conductor_selection.go:75`. | `SoonestResetSelector` copies its shape: same availability filter, sorted instead of first. |
| Session affinity | `SessionAffinitySelector` (`selector.go:912`) wraps a `Fallback` selector and calls it only on a cold binding, with no session ID, or when the bound auth is unavailable (`selector.go:1154,1195,1228,1242,1339`). With `rebind-on: quota`, transient failures keep the binding. | Composition is free: pass `SoonestResetSelector` as `Fallback`. |
| Custom-selector path | A selector that is not built in skips the scheduler fast path (`useSchedulerFastPath`) and receives the highest-priority available candidates in `pickNextLegacy` / `pickNextMixedLegacy` (`conductor_selection.go:1899,2245`). | Keep `SoonestResetSelector` out of `isBuiltInSelector` and `selectorStrategy`, so no scheduler changes are needed. |
| Authenticated upstream GET | `POST /v8/management/requests/api-call` with `$TOKEN$` substitution, plain transport plus proxy (`api_tools.go`). Used for the live checks. | The poller uses `helps.NewProxyAwareHTTPClient` (same proxy resolution) and the token from `auth.Metadata["access_token"]`. |
| Quota polling precedent | Devin: `devin_executor.go:203` writes `Quota.Signals` from a status call. `conductor_refresh.go:606` revalidates hourly when exhausted. | Same idea, but in a separate poller. A token refresh happens only near expiry, so the refresh loop cannot host it. |
| Plugin quota API | `/v8/management/plugins/:id/quota` and v0 `/quota/fetch` (`plugin_quota.go`), plugin-only. | Not used: it is plugin-host shaped and v0. |

Not present upstream: per-window merged usage per account, any use of quota in selection, an
endpoint poller for Claude, and a normalized quota endpoint.

### 1.3 Dashboards

Repos were cloned read-only into `temp/` (gitignored):

| Repo | Commit |
|---|---|
| router-for-me/Cli-Proxy-API-Management-Center (MC) | `603539ab` |
| seakee/CPA-Manager-Plus | `a6b8e318` |
| WizisCool/oh-my-cpa | `9e6a6cce` |

- **MC is the bundled panel, and it already has a "Quota Management" page.**
  - `src/features/quota/QuotaPage.tsx` has provider tabs (`QUOTA_TAB_ORDER` starts with `claude`).
  - The header (`components/QuotaHeader.tsx`) shows count, loaded and attention counts, with a refresh-all button.
  - Each card has a per-row "Refresh Quota" button (`QuotaCard.tsx`).
  - The Claude body (`providers/claude/ClaudeQuotaBody.tsx`) shows the plan chip from `/api/oauth/profile`, plus 5-hour, 7-day and 7-day Fable meters with reset labels.
  - It fetches through `POST /v8/management/requests/api-call` (`src/features/quota/providers/claude/data.ts`, `src/utils/quota/constants.ts:101-124`).
  - Fable is detected as `kind==='weekly_scoped' && display_name in ('fable','fable 5')`.
  - Missing: a "Show emails" toggle (no masking anywhere), the summed summary card, "% remaining" wording, the Fable / 5h / 7d row order, and "No reset pending".
  - Stack: React 19 + Vite + zustand + i18next, bundled single-file into `management.html`.
  - CLIProxyAPI downloads it from that repo's latest GitHub release (`internal/managementasset/updater.go`, re-checked every 3 h). The repo is set by `management.panel-github-repository`.
- **CPA-Manager-Plus**: a heavy MC fork on the v0 API. It has masked/full display (default full) and account summaries, but its quota automation focuses on Codex and xAI. It is far larger.
- **oh-my-cpa**: a separate Go + SQLite service with its own console. It polls `/api/oauth/usage` through our api-call. A poor fit, since it is another service to run.

**Recommendation.**
- Extend MC, in a fork published as a release with a `management.html` asset, and point
  `management.panel-github-repository` at the fork.
- Change the MC Claude adapter to read the new proxy endpoint `GET /v8/management/quota/claude`
  first, and fall back to the current api-call path on 404. Then one poller feeds both routing and
  the UI, and the browser stops hitting the rate-limited usage endpoint itself.
- Add to MC: the email toggle (masked by default, applied in `getQuotaDisplayName`,
  `src/utils/quota/identity.ts`), the summary card, the meter order, "% remaining", and
  "No reset pending".
- The MC changes are about 300–500 lines. Upstreaming them to MC is optional.

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | Passive headers first, endpoint poll second, both merged per window (the newer `observed_at` wins). | Headers are free and current. The poll covers idle accounts and the Fable window. Merging per window fixes the problem that the auth-level snapshot replaces Fable data. |
| D2 | New `ClaudeQuotaTracker` in `sdk/cliproxy/auth`, fed from the existing observation call site. The upstream `QuotaState.Signals` behavior stays as is. | One hook line, race-free (the tracker has its own mutex and never calls back into the Manager), cheap upstream merges. |
| D3 | The poller lives in `internal/claudequota`, is started and stopped by the service, and reaches the tracker through a `ClaudeUsageFetcher` interface. | Keeps HTTP and proxy code out of the auth package. Tests use httptest. |
| D4 | `SoonestResetSelector` is a custom selector (not built in) and wraps nothing. Session affinity wraps it. | No scheduler or fast-path changes. Affinity already limits fallback calls to cold bindings and failover. |
| D5 | No new 429 cooldown. | It already exists (1.2). Tracked `rejected` / 100 % only steers cold picks. |
| D6 | Tracking is advisory. If every candidate looks exhausted, still pick the one whose limiting window resets soonest. | The data can be stale by up to the interval. The hard block stays the real 429 → cooldown path. |
| D7 | Poll only when needed: by default when `strategy == soonest-reset`, or when `routing.claude-quota.refresh-interval` is set explicitly. Manual refresh from the UI always works. | Users who don't opt in get no new upstream traffic. Needed for an upstream PR. |
| D8 | The endpoint returns full emails. The UI masks them by default. | `/v8/management/credentials` already returns emails to the same management key, so masking is a display concern. |
| D9 | No HTTP client timeout on the poll. Single-flight per account, and cancellation on shutdown. | AGENTS.md forbids timeouts after connect, outside listed exceptions. A hung poll blocks only that account's next poll and is visible via `poll_started_at`. |
| D10 | Fork first, keep it upstreamable: new files, three small hooks, English docs, `/v8` only. | Answers the SOURCE.md open question. Offer upstream once it has run live for a week. |

## 3. Selection rules: `soonest-reset`

Input: the candidates `Pick` receives (already the highest priority tier, after the cooldown,
disabled and unauthorized filters through `getSelectorAvailableAuths`), the route `model`, and the
tracker snapshots.

1. **Classify each candidate.**
   - `claude` provider plus an OAuth token, with a snapshot → **known**.
   - Otherwise → **unknown**: no data yet, an API-key Claude auth, or a non-Claude auth in a `mixed` pick.
2. **Expire windows.** A window whose `resets_at <= now` counts as reset: used = 0 and reset unknown.
3. **Skip** a known candidate (it moves to the "skipped" group) when any of these holds:
   - 5h: `status == rejected`, or `used_percent >= skip-five-hour-at` (default 100), and `resets_at > now`;
   - 7d overall: `status == rejected` or `used_percent >= 100`, and `resets_at > now`;
   - 7d Fable: the same test, **only when the route model is a Fable model**
     (`strings.Contains(canonicalModelKey(model), "fable")`).
4. **Order key** for eligible known candidates: `weekly_reset`, which is the Fable 7d `resets_at`
   if known and in the future, else the 7d overall `resets_at`, else `now + 7d` (a fresh window
   starts on first use, so it resets last). Ascending, so the soonest reset goes first.
5. **Ties.** Two `weekly_reset` values within 1 minute of each other are equal (live resets differ
   by microseconds). Break ties by:
   1. higher weekly remaining (more quota to lose before the reset);
   2. lower 5h used percent;
   3. auth ID ascending (deterministic, like fill-first).
6. **Result order.** First the eligible known candidates (ordered as above), then the unknown ones
   by auth ID, then the skipped ones ordered by their earliest blocking `resets_at`.
   - Pick the first.
   - An unknown pick also calls `tracker.RequestPoll(authID)`. The poll is non-blocking and coalesced.
7. **Never return an error when there are candidates.** Errors come only from
   `getSelectorAvailableAuths` (every candidate in cooldown), so the existing `modelCooldownError`
   behavior is kept.
8. **Session affinity.** Wrapped as `Fallback`, the order applies only to:
   - new bindings;
   - requests without a session ID;
   - re-selection after the bound auth became unavailable: a 429 cooldown, auth failure or disable.
     With `rebind-on: quota`, a 5xx or timeout keeps the binding.

   A tracked 5h of 100 % on a bound account does not move its sessions. They move only when the
   account actually fails (a 429 → cooldown → rebind). This keeps prompt caches.
9. **Out of scope for this strategy:**
   - Home mode: `SelectAuth` refuses legacy selection while Home is enabled.
   - A configured plugin scheduler: it takes precedence, as for every strategy.

## 4. Interfaces

### 4.1 Tracker (`sdk/cliproxy/auth/claude_quota.go`)

```go
// ClaudeQuotaWindowName identifies a tracked Anthropic subscription window.
type ClaudeQuotaWindowName string

const (
	ClaudeWindowFiveHour      ClaudeQuotaWindowName = "five_hour"
	ClaudeWindowSevenDay      ClaudeQuotaWindowName = "seven_day"
	ClaudeWindowSevenDayFable ClaudeQuotaWindowName = "seven_day_fable" // header 7d_oi, endpoint limits[weekly_scoped Fable]
)

// ClaudeQuotaWindow is one window's latest observation. UsedPercent is 0..100+ (headers are scaled x100).
type ClaudeQuotaWindow struct {
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitempty"`
	Status      string    `json:"status,omitempty"` // allowed | allowed_warning | rejected | "" (endpoint has none)
	ObservedAt  time.Time `json:"observed_at"`
	Source      string    `json:"source"` // "headers" | "poll"
}

type ClaudeQuotaPlan struct {
	Label            string    `json:"label"`             // "Max 5x", "Max 20x", "Pro", "Team", ""
	OrganizationType string    `json:"organization_type"` // raw
	RateLimitTier    string    `json:"rate_limit_tier"`   // raw
	FetchedAt        time.Time `json:"fetched_at"`
}

type ClaudeQuotaSnapshot struct {
	AuthID        string                                      `json:"auth_id"`
	Windows       map[ClaudeQuotaWindowName]ClaudeQuotaWindow `json:"windows"`
	Plan          *ClaudeQuotaPlan                            `json:"plan,omitempty"`
	LastPollAt    time.Time                                   `json:"last_poll_at,omitempty"`
	PollStartedAt time.Time                                   `json:"poll_started_at,omitempty"` // non-zero while in flight
	NextPollAt    time.Time                                   `json:"next_poll_at,omitempty"`
	PollError     string                                      `json:"poll_error,omitempty"` // sanitized, no tokens
}

type ClaudeQuotaTracker struct {
	mu      sync.RWMutex
	byAuth  map[string]*ClaudeQuotaSnapshot
	pollReq chan string // buffered; RequestPoll is non-blocking
	now     func() time.Time
}

func NewClaudeQuotaTracker() *ClaudeQuotaTracker
func (t *ClaudeQuotaTracker) ObserveHeaders(authID, model string, h http.Header, at time.Time) // ignores non-unified headers
func (t *ClaudeQuotaTracker) ObserveUsage(authID string, u ClaudeUsageResponse, at time.Time)
func (t *ClaudeQuotaTracker) ObservePlan(authID string, p ClaudeQuotaPlan)
func (t *ClaudeQuotaTracker) ObservePollResult(authID string, started, finished time.Time, err error, next time.Time)
func (t *ClaudeQuotaTracker) Snapshot(authID string) (ClaudeQuotaSnapshot, bool) // deep copy
func (t *ClaudeQuotaTracker) Forget(authID string)
func (t *ClaudeQuotaTracker) RequestPoll(authID string)
func (t *ClaudeQuotaTracker) PollRequests() <-chan string

// ClaudeUsageResponse is the subset of GET /api/oauth/usage we read.
type ClaudeUsageResponse struct {
	FiveHour *ClaudeUsageWindow `json:"five_hour"`
	SevenDay *ClaudeUsageWindow `json:"seven_day"`
	Limits   []ClaudeUsageLimit `json:"limits"`
}
type ClaudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}
type ClaudeUsageLimit struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resets_at"`
	IsActive bool     `json:"is_active"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// Ranking shared by the selector and the management endpoint (dry-run view).
type ClaudeQuotaRank struct {
	AuthID        string
	State         string    // "eligible" | "unknown" | "skipped_5h" | "skipped_7d" | "skipped_7d_fable"
	WeeklyResetAt time.Time // order key actually used
	KeySource     string    // "seven_day_fable" | "seven_day" | "fresh"
}
func RankClaudeQuota(auths []*Auth, model string, t *ClaudeQuotaTracker, cfg SoonestResetConfig, now time.Time) []ClaudeQuotaRank
```

Header mapping in `ObserveHeaders`:
- `5h` → `five_hour`, `7d` → `seven_day`, `7d_oi` → `seven_day_fable`;
- `-utilization` ×100, `-reset` (unix seconds or RFC 3339), `-status`.

A window is updated only when its utilization or reset header is present, so an Opus response
never clears Fable.

Endpoint mapping in `ObserveUsage`:
- `five_hour` and `seven_day` map directly.
- Fable is the first `limits[]` entry with `kind=="weekly_scoped"` and
  `display_name` (lower-cased, trimmed) in `{"fable","fable 5","fable 5.1"}`, preferring `is_active`.
- `resets_at: null` → zero time ("No reset pending").

### 4.2 Hook (`sdk/cliproxy/auth/conductor.go`, `conductor_cooldown.go`)

```go
func (m *Manager) SetClaudeQuotaTracker(t *ClaudeQuotaTracker)
func (m *Manager) ClaudeQuotaTracker() *ClaudeQuotaTracker
```

In `conductor_cooldown.go` next to line 1333, inside the existing `!result.SkipQuotaObservation`
block, add:

```go
if t := m.claudeQuota; t != nil && strings.EqualFold(result.Provider, "claude") {
	t.ObserveHeaders(auth.ID, result.Model, responseHeaders, now)
}
```

On auth removal, call `t.Forget(id)` from the existing remove path.

### 4.3 Selector (`sdk/cliproxy/auth/selector_soonest_reset.go`)

```go
type SoonestResetConfig struct {
	SkipFiveHourAtPercent float64 // default 100
}

type SoonestResetSelector struct {
	tracker *ClaudeQuotaTracker
	cfg     SoonestResetConfig
	now     func() time.Time // nil -> time.Now; tests inject a fake clock
}

func NewSoonestResetSelector(t *ClaudeQuotaTracker, cfg SoonestResetConfig) *SoonestResetSelector
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
```

`Pick` calls `getSelectorAvailableAuths(ctx, auths, provider, model, now)`, then
`RankClaudeQuota`, and returns the first auth. It is not added to `isBuiltInSelector` or
`selectorStrategy`.

### 4.4 Poller (`internal/claudequota/poller.go`)

```go
type Poller struct {
	tracker  *coreauth.ClaudeQuotaTracker
	manager  *coreauth.Manager            // List() for Claude OAuth auths (clones)
	client   func(*coreauth.Auth) *http.Client // helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
	baseURL  string                       // "https://api.anthropic.com"; tests use httptest URL
	interval time.Duration                // 0 = no background polling
	now      func() time.Time
}
func New(...) *Poller
func (p *Poller) Start(ctx context.Context)
func (p *Poller) Stop()
func (p *Poller) SetInterval(d time.Duration)                    // hot reload
func (p *Poller) Refresh(ctx context.Context, authID string) error // manual; single-flight; respects backoff
```

- **Eligible auths**: `Provider=="claude"`, not disabled, with a non-empty
  `Metadata["access_token"]`, and the `expired` metadata in the future. An expired token is
  skipped with `poll_error="token_expired"`; the refresh loop owns token refresh.
- **Headers**: as in 1.1. The User-Agent is the proxy's configured Claude CLI user agent
  (`helps` device profile default).
- **Poll is due** when `now >= next_poll_at` and not every window was passively observed within
  the interval. For each account: `next = now + interval ± 10 %`.
- **On 429/403**: `next = now + min(retry-after || 5m, 1h)`.
- **On 401**: `poll_error="unauthorized"`; next poll at `now + interval`.
- **Profile**: fetched on the first successful poll, then every 24 h.

### 4.5 Config (`internal/config/config_types.go`, `RoutingConfig`)

```yaml
routing:
  strategy: "soonest-reset"        # new value; aliases: soonestreset, sr
  claude-quota:
    refresh-interval: "10m"        # OAuth usage poll per Claude account. "" = 10m when strategy is soonest-reset, else off. "0" = off.
    skip-five-hour-at: 100         # percent used at which soonest-reset skips an account for new sessions
```

```go
type ClaudeQuotaConfig struct {
	RefreshInterval string   `yaml:"refresh-interval,omitempty" json:"refresh-interval,omitempty"`
	SkipFiveHourAt  *float64 `yaml:"skip-five-hour-at,omitempty" json:"skip-five-hour-at,omitempty"`
}
// RoutingConfig gains: ClaudeQuota ClaudeQuotaConfig `yaml:"claude-quota,omitempty" json:"claude-quota,omitempty"`
```

- Interval parse: below 1 minute clamps to 1 minute; invalid falls back to the default.
- Places that must learn `soonest-reset`:
  - `normalizedRoutingRuntimeState` and `newRoutingSelector` (`sdk/cliproxy/service_config.go`);
  - `normalizeRoutingStrategy` (`management/config_basic.go`);
  - the `Strategy` comment;
  - `config.example.yaml`.
- `routingRuntimeState` gains `skipFiveHourAt`, so a change rebuilds the selector. The interval is
  applied to the poller without rebuilding it.

### 4.6 Management endpoints (`internal/api/server_management_v8.go`, new `management/claude_quota.go`)

`GET /v8/management/quota/claude`

```json
{
  "provider": "claude",
  "generated_at": "2026-10-21T09:00:00Z",
  "strategy": "soonest-reset",
  "refresh_interval_seconds": 600,
  "summary": {
    "credentials": 5,
    "loaded": 4,
    "weekly_window": "seven_day_fable",
    "weekly_remaining_percent_sum": 409,
    "weekly_capacity_percent": 500,
    "segments": [{"auth_index": "1", "remaining_percent": 53}],
    "next_reset_at": "2026-10-22T03:59:59Z"
  },
  "credentials": [
    {
      "auth_index": "1",
      "id": "claude-xxxx-user@example.com.json",
      "name": "claude-xxxx-user@example.com.json",
      "email": "user@example.com",
      "label": "",
      "disabled": false,
      "status": "active",
      "cooldown_until": null,
      "plan": {"label": "Max 5x", "organization_type": "claude_team", "rate_limit_tier": "default_claude_max_5x"},
      "routing": {"rank": 1, "state": "eligible", "weekly_reset_at": "2026-10-22T03:59:59Z", "key_source": "seven_day_fable"},
      "windows": {
        "seven_day_fable": {"used_percent": 47, "remaining_percent": 53, "resets_at": "2026-10-22T03:59:59Z", "status": "allowed", "observed_at": "...", "source": "poll"},
        "five_hour":       {"used_percent": 10, "remaining_percent": 90, "resets_at": "2026-10-21T10:30:00Z", "status": "allowed", "observed_at": "...", "source": "headers"},
        "seven_day":       {"used_percent": 56, "remaining_percent": 44, "resets_at": "2026-10-22T04:00:00Z", "status": "allowed", "observed_at": "...", "source": "headers"}
      },
      "last_poll_at": "...",
      "next_poll_at": "...",
      "poll_error": ""
    }
  ]
}
```

- A missing window is `null`. `resets_at: null` → the UI shows "No reset pending".
- `remaining_percent = max(0, 100 - used_percent)`.
- `summary.weekly_window` is `seven_day_fable` when every loaded account has it, else `seven_day`.
- `loaded` counts accounts with at least one window.
- `routing` is the `RankClaudeQuota` dry run for the model `""`. It is present for every strategy,
  so the order can be inspected before switching.

`POST /v8/management/quota/claude/refresh` with body `{"auth_index": "1"}`, or `{}` for all:
- Runs `Poller.Refresh` synchronously and returns `{"credentials": [<entry>...]}`.
- During backoff it answers `429 {"error": "backoff", "retry_after_seconds": N}`.
- Without a poller: `503`.

Document both in `docs/management-api-v8.md` (Operational endpoints table). No `/v0` route
(AGENTS.md).

## 5. File touch list

| File | Change |
|---|---|
| `sdk/cliproxy/auth/claude_quota.go` (+ `_test.go`) | new: tracker, header and usage parsing, `RankClaudeQuota` |
| `sdk/cliproxy/auth/selector_soonest_reset.go` (+ `_test.go`) | new: selector |
| `sdk/cliproxy/auth/conductor.go` | `claudeQuota` field + setter/getter |
| `sdk/cliproxy/auth/conductor_cooldown.go` | 3-line observe hook next to line 1333 |
| `sdk/cliproxy/auth/conductor_lifecycle.go` (remove path) | `Forget` on remove |
| `sdk/cliproxy/auth/testdata/claude_usage_*.json`, `claude_headers_*.txt` | sanitized live fixtures |
| `internal/claudequota/poller.go` (+ `_test.go`) | new: poller |
| `internal/config/config_types.go` | `ClaudeQuotaConfig`, strategy comment |
| `internal/config/config_v8_save_layout_test.go` (or new test) | round-trip `routing.claude-quota.*` |
| `config.example.yaml` | document strategy + keys |
| `sdk/cliproxy/service_config.go` | strategy normalize, selector build, poller interval apply |
| `sdk/cliproxy/builder.go` | create tracker before the manager, pass it to the selector and `SetClaudeQuotaTracker` |
| `sdk/cliproxy/service_lifecycle.go` | start/stop poller |
| `internal/api/handlers/management/config_basic.go` | `normalizeRoutingStrategy` accepts `soonest-reset` |
| `internal/api/handlers/management/claude_quota.go` (+ `_test.go`) | new: GET + POST refresh |
| `internal/api/server_management_v8.go` | 2 routes |
| `docs/management-api-v8.md` | 2 rows |
| `test/claude_quota_routing_test.go` | cross-module: manager + affinity + soonest-reset + fake executor |
| MC fork (separate repo) | Claude adapter source switch, email toggle, summary card, meter order, i18n |

Untouched: `internal/translator/`, `/v0/management`, executors (no request-path change, so the
payload-ordering rule is unaffected), the scheduler fast path, and `QuotaState` semantics.

## 6. Build plan

Every phase must end with all of these green:

```bash
gofmt -l . | grep -v '^temp/'      # must print nothing
go build -o test-output ./cmd/server && rm test-output
go test ./sdk/cliproxy/... ./internal/... ./test/...
go test ./...                       # before commit
```

Commit per phase on `feat/quota-routing`, append to `STATUS.md`, do not push.

1. **Tracker + passive feed.**
   - Build: `claude_quota.go`, the Manager hook, `Forget`.
   - Tests:
     - header fixture parse, including the live set with and without `7d_oi`;
     - usage JSON parse, including the live payload with Fable in `limits[]`, null `resets_at`, and the `fable 5` display name;
     - per-window merge: newer wins across sources, and an Opus header set does not clear Fable;
     - window expiry with a fake clock;
     - a `Manager.MarkResult` test proving headers in ctx reach the tracker, and that `SkipQuotaObservation` is honored.
2. **Selector + config.**
   - Build: `SoonestResetSelector`, `RankClaudeQuota`, config type, strategy wiring in service, management and example config.
   - Tests (fake `now`, no `time.Sleep`):
     - ordering by Fable reset, with fallback to 7d and to `fresh`;
     - skip at 5h threshold and on `rejected`;
     - Fable skip only for Fable models;
     - unknown after known, and `RequestPoll` emitted;
     - all-skipped still returns the soonest;
     - the tie window and each tie-break;
     - mixed non-Claude auths go last;
     - cooldown-filtered auths are never returned;
     - config load/save round trip, and the alias normalize.
3. **Affinity composition.** Test `NewSessionAffinitySelectorWithConfig{Fallback: soonest, RebindOn: quota}`:
   - a cold session binds rank 1;
   - after the ranks change, the bound session stays;
   - a new session goes to the new rank 1;
   - a 429 (`MarkResult` with the unified 5h rejected headers) on the bound auth rebinds to the next rank;
   - a 5xx keeps the binding.

   Add `test/claude_quota_routing_test.go` running these through the Manager with a fake executor.
4. **Poller.**
   - Build: `internal/claudequota` and the service start/stop/hot-reload wiring.
   - httptest tests:
     - per-bearer canned payloads (token `fake-a`, `fake-b`, …);
     - headers asserted;
     - 429 with and without `retry-after` sets the backoff, capped at 1 h;
     - 401 and the expired token;
     - single-flight under concurrent `Refresh`;
     - passive-fresh skip;
     - profile fetched once per 24 h;
     - jitter bounds;
     - interval 0 means no background polls;
     - `Stop` cancels in-flight work.
5. **Management endpoint.**
   - Build: GET + POST refresh, docs rows.
   - Handler tests:
     - JSON shape golden;
     - the summary math (409 of 500 across 5);
     - `weekly_window` fallback;
     - no tokens in output;
     - refresh backoff 429;
     - no poller → 503.
6. **Live verification** (section 7) and a STATUS entry.
7. **MC fork UI** (separate repo):
   - the adapter reads `/v8/management/quota/claude` and falls back to api-call on 404;
   - the "Show emails" toggle (masked by default);
   - the summary card: segmented bar plus "in 1 day · 09/12, 23:00";
   - rows: masked name, plan, Fable / 5h / 7d meters as "% remaining", "No reset pending";
   - per-row refresh → POST refresh.

   Ship it as a fork release `management.html` and set `management.panel-github-repository` to
   the fork.

## 7. Verification plan (live)

- **Only one real Claude account exists.** Multi-account ordering is proven by unit and
  integration tests with fake auths and an httptest usage server (phases 2–4). Live checks prove
  the data path and the single-account behavior. A fake auth row proves the UI and the "unknown"
  handling.
- **Do not replace the LaunchAgent binary.** Another effort runs it from the main checkout.
  Instead, run a second instance from the worktree:
  1. Make a temp auth dir: `mkdir -p temp/live/auths`. Copy `~/.cli-proxy-api/claude-*.json` into
     it. Add 2 fake Claude auth files with bogus tokens and distinct emails
     (`fake-a@example.invalid`, `fake-b@…`).
  2. **Guard:** the real file's `expired` must be at least 2 h ahead. Stop the instance within 1 h.
     That way it never refreshes the shared refresh token: rotation would invalidate the main
     proxy's copy. Delete `temp/live/auths` afterwards.
  3. Make `temp/live/config.yaml`, a copy of `config.yaml` with these changes:
     - `server.port: 8318`;
     - `oauth.auth-dir: temp/live/auths`;
     - `routing.strategy: soonest-reset`;
     - `routing.claude-quota.refresh-interval: "2m"`;
     - session-affinity unchanged.
  4. Run `go build -o temp/live/cpa ./cmd/server && temp/live/cpa --config temp/live/config.yaml`.
- **Checks.** Keys come from `.env`; never print them. Budget: about 6 upstream calls in total.
  1. `GET :8318/v8/management/quota/claude`:
     - the real account has `five_hour`, `seven_day` and `seven_day_fable` from `source: poll` within 2 min of start;
     - values are within 1 point of a direct `api-call` to `/api/oauth/usage` on :8317 (one call);
     - the fakes show `poll_error: unauthorized` and `routing.state: unknown`;
     - the real account has `rank: 1`.
  2. Send one `claude-haiku-5-5` request with an `X-Claude-Code-Session-Id` header to :8318:
     - the log shows `session-affinity: cache miss, new binding … auth=<real>`;
     - the next GET shows the 5h window `source: headers` with a newer `observed_at`.
  3. Repeat with the same session id: `cache hit`, same auth.
  4. Send one `claude-fable-5-1` request (`max_tokens: 8`): `seven_day_fable.source` becomes
     `headers`.
  5. `POST :8318/v8/management/quota/claude/refresh {"auth_index": <real>}` returns fresh
     `last_poll_at`. A second immediate POST is coalesced or answered within backoff.
  6. Count `api/oauth/usage` polls in the instance log over 10 min: at most 5 for the real account.
     Polls are skipped while passive data is fresh.
  7. Before Phase 7 ships: point the instance's
     `management.panel-github-repository` at the MC fork and open `http://127.0.0.1:8318/management.html`. Check:
     - three rows, masked emails, and a working toggle;
     - the summary card adds up the remaining weekly percent;
     - "No reset pending" for the fakes.
- **Done when** checks 1–6 pass and STATUS.md has an entry with the results. Switching the
  LaunchAgent to `soonest-reset` is the user's call, once a second real account exists.

## 8. Open items

- A second real Claude account would let the live ordering be observed. Until then the
  multi-account proof is test-only.
- The endpoint's undocumented keys (`iguana_necktie`, `cinder_cove`, …) can change. The parser
  reads only `five_hour`, `seven_day` and `limits[]`, and tolerates unknown or missing keys.
- Upstream PR: consider after one week live. Changes in the MC fork could go to MC upstream
  separately.

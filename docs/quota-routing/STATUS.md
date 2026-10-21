# Quota routing: status log

Single source of truth for progress on `feat/quota-routing` (worktree
`/Users/szymon/private/CLIProxyAPI/.worktrees/quota-routing`). Requirements: `SOURCE.md`.
Spec and phase plan: `SPEC.md`. Each agent appends one entry when it finishes a unit of work:
date, phase, what changed (commit SHAs), what's next, blockers. Newest at the bottom.

## Local environment (for agents)

- Work only in this worktree. The main checkout `/Users/szymon/private/CLIProxyAPI` belongs to another effort (Cursor provider). Read only its `logs/` and `config.yaml`.
- Running proxy: LaunchAgent `com.szymon.cliproxyapi`, `127.0.0.1:8317`, built from the main checkout, config `/Users/szymon/private/CLIProxyAPI/config.yaml`, keys in `/Users/szymon/private/CLIProxyAPI/.env` (`CPA_API_KEY`, `MANAGEMENT_PASSWORD`). Never print key values. Do not replace its binary from this branch.
- Logs: `/Users/szymon/private/CLIProxyAPI/logs/launchd.out.log`, `launchd.err.log`.
- Live config already has `routing.session-affinity: true`, ttl `1h`, subagents `true`, rebind-on `quota`, strategy `round-robin`.
- Claude auth: one account, `~/.cli-proxy-api/claude-*.json`. Never print tokens. A second instance from this worktree must use its own auth dir (`temp/live/auths`), port 8318, and run less than 1 h while the token's `expired` is at least 2 h away (SPEC section 7). This avoids refresh-token rotation clashing with the main proxy.
- Read-only quota data on the live proxy:
  - `GET /v8/management/credentials` → `quota.signals` and `model_quotas.<model>.signals`;
  - `POST /v8/management/requests/api-call` with `Authorization: Bearer $TOKEN$` for `https://api.anthropic.com/api/oauth/usage` and `/api/oauth/profile`.

  Keep upstream calls to a handful per session: the usage endpoint rate-limits.
- Installed Claude Code: `~/.local/bin/claude` → `~/.local/share/claude/versions/<ver>`. Use `claudep` for proxied end-to-end runs.
- Dashboard repos cloned read-only under `temp/`: Management Center `603539ab`, CPA-Manager-Plus `a6b8e318`, oh-my-cpa `9e6a6cce`. Throwaway work goes under `temp/` (gitignored).
- Toolchain: go 1.27.3 darwin/arm64.

## Log

- 2026-10-21 (spec): research done and `SPEC.md` written.
  - **Data source:** the OAuth usage endpoint was verified live (5h/7d at the top level; Fable is `limits[kind=weekly_scoped, display_name=Fable]`). The passive headers were verified live too: 5h and 7d on every response, 7d Fable (`7d_oi`) only on Fable responses.
  - **Decision:** passive headers first, plus a 10 min endpoint poll.
  - **Reuse:** the header capture (`quota_signals.go`) and the 429 cooldown (`claude_ratelimit.go`) already exist. Session affinity already delegates only cold bindings and failover to the fallback selector.
  - **Dashboard:** extend a Management Center fork, which already has a Claude quota page, and add a `GET /v8/management/quota/claude` endpoint.
  - **Live calls used:** 3 upstream (usage, profile, one 8-token Fable message) plus local management reads.
  - **Next:** Phase 1 (tracker + passive feed).
  - **Blockers:** none. Multi-account ordering can only be proven with fake auths until a second real account exists.
- 2026-10-21 (phase 1, commit "feat(quota-routing): add Claude quota tracker fed by response headers"):
  - New `sdk/cliproxy/auth/claude_quota.go`: `ClaudeQuotaTracker` (per-auth, per-window merge, newer `observed_at` wins; expired windows read as fresh), header parsing (`5h`/`7d`/`7d_oi`, unix or RFC 3339 resets), usage-JSON parsing (Fable from `limits[weekly_scoped]`, `fable`/`fable 5`/`fable 5.1`, active preferred), plan + poll bookkeeping, non-blocking `RequestPoll`, `ClaudeQuotaRefresher` hook for the poller.
  - Upstream hooks kept to 4 lines: `claudeQuota` atomic field (`conductor.go`), `m.observeClaudeQuota(...)` inside the existing `!SkipQuotaObservation` block (`conductor_cooldown.go`), `m.forgetClaudeQuota(id)` in `Remove` (`conductor_lifecycle.go`).
  - Sanitized fixtures in `sdk/cliproxy/auth/testdata/` (live usage payload, `fable 5` + null reset variant, Fable and Opus header sets).
  - Tests: `go test ./sdk/cliproxy/auth` green. Pre-existing: `gofmt -l` flags upstream `internal/pluginhost/host.go` (left untouched).
  - Next: phase 2 (selector + config).
- 2026-10-21 (phase 2, commit "feat(quota-routing): add soonest-reset routing strategy and claude-quota config"):
  - New `sdk/cliproxy/auth/selector_soonest_reset.go`: `RankClaudeQuota` (SPEC section 3) + `SoonestResetSelector` (custom selector, not in `isBuiltInSelector`/`selectorStrategy`). Clock = tracker clock unless overridden.
  - Interpretation: skipped accounts are ordered by their unblock time = the latest reset among their blocking windows (an account blocked on 5h and 7d is usable only after both reset).
  - Config: `routing.strategy: soonest-reset` (aliases `soonestreset`, `sr`), `routing.claude-quota.refresh-interval`, `routing.claude-quota.skip-five-hour-at` (type + helpers in new `internal/config/claude_quota.go`; one field added to `RoutingConfig`). Documented in `config.example.yaml`.
  - Wiring: `sdk/cliproxy/service_claude_quota.go` (new); one-line hooks in `service_config.go` (state fields, strategy case, tracker attach) and `builder.go` (tracker created before the manager); management `normalizeRoutingStrategy` falls through to `config.NormalizeSoonestResetStrategy`.
  - Tests: ordering/fallback keys, 5h threshold + rejected, weekly 100 %, Fable-only skip, unknown + `RequestPoll`, all-skipped, expiry with fake clock, tie window + each tie-break, mixed non-Claude, cooldown filter + `modelCooldownError`, config interval/threshold/round-trip, alias normalize. Green.
  - Next: phase 3 (affinity composition).
- 2026-10-21 (phase 3, commit "test(quota-routing): prove soonest-reset composes with session affinity"):
  - Tests only, no production change. `sdk/cliproxy/auth/selector_soonest_reset_affinity_test.go` (SelectAuth + MarkResult) and `test/claude_quota_routing_test.go` (Manager.Execute with a fake Claude executor whose responses carry unified headers, so ordering is fed passively).
  - Proven: cold session binds rank 1; rank change keeps bound session, new session takes new rank 1; tracked 5h=100 % does not move a bound session; 429 + unified 5h `rejected` headers cools the account, the tracker records `rejected`, the session rebinds to the next rank; 5xx keeps the binding under `rebind-on: quota` (verified the same test fails with `rebind-on: any`).
  - Test clocks: tracker clock frozen at the real start time, manual observations stamped before it (MarkResult stamps header observations with the wall clock). No sleeps.
  - Next: phase 4 (usage poller).
- 2026-10-21 (phase 4, commit "feat(quota-routing): poll Claude OAuth usage endpoint per account"):
  - New `internal/claudequota/poller.go`: per-account schedule (`interval ±10 %` jitter), passive-fresh skip (all three windows from headers within the interval postpones the poll until the oldest ages out), 429/403 backoff `min(Retry-After (s or HTTP date) || 5m, 1h)`, 401 → `unauthorized`, expired token → `token_expired` without a call, profile (plan label) on first success then every 24 h, single-flight per account (manual refresh joins; background skips), `RequestPoll` honoured only when due, interval 0 = no background polls (manual refresh still works), `Stop` cancels in-flight work. No HTTP timeout (D9). Sanitized fixed error codes only.
  - Backoff error type `ClaudeQuotaBackoffError` lives in `sdk/cliproxy/auth/claude_quota.go` next to the `ClaudeQuotaRefresher` interface.
  - Service wiring (`service_claude_quota.go`): poller started after auto-refresh in `Run` (non-Home only), stopped in `Shutdown`, interval hot-reloaded from `applyManagerConfig`; the poller is found through `tracker.Refresher()`, so `Service` gained no field. Proxy-aware client via `helps.NewProxyAwareHTTPClient`, UA `claude-cli/<DefaultClaudeVersion> (external, cli)`.
  - Tests (httptest, per-bearer `fake-a`/`fake-b`, fake clock, `-race` clean): headers asserted, backoff variants + cap, 401, expired token, single-flight, passive-fresh skip, profile once per 24 h, jitter bounds, interval 0 + hot reload, requested-poll gating, Stop cancels in-flight, service lifecycle.
  - Next: phase 5 (management endpoints).
- 2026-10-21 (phase 5, commit "feat(quota-routing): add v8 Claude quota management endpoints"):
  - `GET /v8/management/quota/claude` and `POST /v8/management/quota/claude/refresh` (new `internal/api/handlers/management/claude_quota.go`; 2 route lines in `server_management_v8.go`; 2 rows in `docs/management-api-v8.md`). No `/v0` route.
  - Shape per SPEC 4.6: Claude OAuth credentials only (sorted by file name), windows in Fable / 5h / 7d order with `remaining_percent = max(0, 100 - used)`, missing windows `null`, `routing` = `RankClaudeQuota` dry run for model `""` (present for every strategy), `refresh_interval_seconds` from the running poller (else config), full emails (UI masks).
  - Summary interpretation: capacity = 100 × loaded credentials; a loaded credential without the chosen weekly window counts as 100 % remaining; `next_reset_at` = earliest future reset of that window.
  - Refresh: `{"auth_index": ...}` → one credential (404 unknown index, 429 `{"error":"backoff","retry_after_seconds":N}` during backoff); `{}`/empty → all enabled credentials concurrently, per-credential failures surface as `poll_error`; 503 without a poller.
  - Tests: golden JSON, 409-of-500-across-5 summary, `weekly_window` fallback, no secrets in GET/POST output, backoff 429 / 404 / 400 / 503, single + all refresh, route registration + auth (`internal/api/server_claude_quota_test.go`).
  - Next: phase 6 (live verification, SPEC section 7) and phase 7 (MC fork UI) — not started by this agent.
  - Full `go test ./...` after phase 5: 8219 passed, 10 skipped, 1 failed — `internal/client/codex/live` `TestPionMediaRelayBridgesAudioAndDataChannel` ("upstream DataChannel was not created"), which also fails on the base commit `d52d2924` (pre-existing, local WebRTC environment). `gofmt -l` flags only upstream `internal/pluginhost/host.go` (pre-existing).
  - Open decisions for the user: skipped-account ordering uses the latest blocking reset (unblock time); summary counts a loaded account lacking the weekly window as 100 % remaining; GET lists only Claude OAuth credentials (API-key Claude credentials are omitted); background poller ignores `RequestPoll` when the interval is 0.
- 2026-10-21 (adversarial review of `main..feat/quota-routing`):
  - **Fixed** `bc83d58b`: the soonest-reset tie comparator ("within 1 min" pairwise) was not transitive. With chained near-ties (a~b, b~c, a and c 100 s apart) the order followed the input order, and an account resetting later could rank ahead of a sooner one. Ties now group around the soonest reset of each run. Test: `TestSoonestReset_TieWindowIsTransitiveAcrossInputOrders`.
  - **Fixed** `f95c5d92`: a usage-endpoint 429/403 set the next background poll to `now + backoff` (5 min without Retry-After). With the 10 min default, that polled a rate-limited bearer twice as often. The next poll is now the later of the backoff and the regular jittered interval. Manual refresh is still gated only by the backoff. Test: `TestPollerBackoffNeverShortensRegularInterval`. `TestPollerRateLimitBackoff` was updated to the corrected schedule.
  - **Verified OK:**
    - Fable-first weekly key with a 7d fallback; the 5h skip at the threshold or `rejected`; the Fable skip only on Fable routes; unknown accounts after eligible ones; expired windows read as fresh.
    - Cooldown filtering through `getSelectorAvailableAuths`, so the existing 429 cooldown stays authoritative.
    - The selector runs only as the affinity `Fallback`, on cold bindings and failover. It stays custom, so there is no scheduler fast path.
    - `-race` is clean on every touched package, and no goroutines are created on hot reload. `Stop` cancels and waits.
    - No token refresh. Logs are Debug only, with no token.
    - The v8 routes sit behind management auth.
    - Hot-file edits are 1–8 lines each.
  - **Decisions (STATUS phase 5):**
    1. Skipped accounts ordered by their latest blocking reset (unblock time): agreed. SPEC section 3.6 still says "earliest" and should be updated to match.
    2. A loaded account missing the weekly window counts as 100 % remaining: agreed. It only affects accounts missing `seven_day`, which is rare.
    3. GET lists Claude OAuth credentials only: agreed.
    4. `RequestPoll` is ignored when the interval is 0: agreed, since "0 = off" stays off and manual refresh still works.
  - **Not fixed (unconfirmed or minor):**
    - Background `due()` is not atomic with the in-flight reservation, so in theory there can be a double poll if a queued `RequestPoll` races a launched goroutine that finishes first. No practical trigger was found.
    - The first poll after start, and after any interval change, hits every account at once, without jitter. A crash loop would repoll on each restart.
    - Profile-endpoint failures, including 429/403, have no backoff and are retried on each successful usage poll.
    - A poll that finishes after `Remove` re-creates tracker state for the removed ID (a tiny leak).
    - The dashboard's dry-run rank includes disabled credentials.
    - Changing `skip-five-hour-at` rebuilds the affinity selector and drops bindings. This is the upstream pattern for any routing change.
    - Fable-route detection is a model-name substring, so aliases without "fable" don't apply the Fable skip.
    - Pre-existing: a `-race` failure in the untouched `internal/api/middleware` (`TestRequestLoggingMiddleware_StreamingPreservesFullUUIDForLogger`).
  - **Next:** phase 6 live verification (SPEC section 7).

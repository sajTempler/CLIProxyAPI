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

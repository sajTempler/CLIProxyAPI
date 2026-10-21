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

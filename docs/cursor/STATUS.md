# Cursor provider: status log

Single source of truth for progress on `feat/cursor-provider`. Plan: `PLAN.md`.
Each agent appends one entry when it finishes a unit of work: date, phase,
what changed (commit SHAs), what's next, blockers. Newest at the bottom.

## Local environment (for agents)

- Running proxy: LaunchAgent `com.szymon.cliproxyapi`, `127.0.0.1:8317`, config `config.yaml`, keys in `.env` (`CPA_API_KEY`, `MANAGEMENT_PASSWORD`). Never print key values.
- Proxy logs: `logs/launchd.out.log`, `logs/launchd.err.log`.
- Redeploy after changes: `go build -o cli-proxy-api ./cmd/server && launchctl kickstart -k gui/$(id -u)/com.szymon.cliproxyapi`.
- Claude auth file: `~/.cli-proxy-api/claude-*.json`. Cursor token: macOS keychain, service `cursor-access-token`, account `cursor-user`. Never print token values.
- Installed agent CLI: `~/.local/bin/agent` → `~/.local/share/cursor-agent/versions/<ver>/`.
- Upstream remote: `upstream` (router-for-me/CLIProxyAPI). Keep the provider isolated (Devin pattern) to keep upstream merges cheap.
- Throwaway work goes under `temp/` (gitignored).

## Log

- 2026-10-21: branch created; plan + research committed. Claude provider live via LaunchAgent. Next: Phase 0 spike.
- 2026-10-21 (routing): Claude Code on this Mac now goes through the proxy: `ANTHROPIC_BASE_URL=http://127.0.0.1:8317` and `ANTHROPIC_AUTH_TOKEN` (= `CPA_API_KEY`) in the `env` block of `~/.claude/settings.json`; verified with `claude -p --model claude-haiku-5-5` (200s on `/v1/messages` in `logs/launchd.out.log`). Fleet machines must not get these keys: `fleet config push` strips them on the sender (herdr-fleet `scripts/fleet`, not in git) and `machine/fleet-config-receive` strips them again (szymon-agent-fleet `7ab0018` on branch `chore/strip-proxy-env`, unpushed). Next: run `fleet config push` when ready.
- 2026-10-21: routing changed: proxy env removed from `~/.claude/settings.json` (it disabled claude.ai connectors). Proxied Claude Code = `~/.local/bin/claudep` wrapper (reads `CPA_API_KEY` from `.env` at runtime); plain `claude` stays direct. Use `claudep` for any proxy end-to-end test.
- 2026-10-21: omsub (yobo2u/omsub) evaluated (`omsub-eval.md`) and SKIPPED by user decision: no trial, no port. Native plan in `PLAN.md` stands; the eval stays only as a reference.
- 2026-10-21, Phase 0 spike: **GO, with a changed Phase 2 design** (`SPIKE.md`). 28 Runs, agent CLI 2026.10.19-6bbf2c4.
  - Q1 replay: no. Server ignores `conversation_history`/`trailing_conversation_history`. Transcript-in-user-text, checkpoint + `resume_action`, and a held-open stream all continue correctly.
  - Q2 MCP tool use: yes. 9/9 used MCP tools with a guidance rule. Without it, models tried `read_args`, got soft-denied, then fell back.
  - Q3 checksum: not required.
  - Q4 system prompt: dominates only as a global `RequestContext` rule. `system_prompt_spec`/`system_prompt_override` are ignored; `custom_system_prompt` is rejected.
  - Next: update PLAN.md Phase 2 for stateful sessions (checkpoint + blob store, held-open Run per pending tool_use, transcript fallback), then Phase 1 tooling.
- 2026-10-21, Phase 1 maintenance tooling: landed (`MAINTENANCE.md` has every command).
  - `89282ada`: PLAN.md Phase 2 rewritten for stateful sessions (checkpoint + blob store, held-open Run, global-rule system prompt, no checksum).
  - `83d80e7d`: `cmd/cursor_proto_sync` + generated `helps/cursorpb` (1035 messages / 80 enums: the closure of Run, GetUsableModels, GetServerConfig; in-process protoc-gen-go v1.34.1, byte-reproducible). `--check` against installed 2026.10.19-6bbf2c4: no protocol changes. Against the 2026.09.26 bundle it reports 9 removed fields in used messages, none we rely on, so `used` is message → relied-on fields.
  - `71cbc013`: `helps/cursor_version.go` (symlink version, 10 min ticker, snapshot fallback). Not wired yet (Phase 4).
  - `c9a079a6`: `cmd/cursor_canary`, exits 0/1/2/3 plus 4 = inconclusive (proxy down, key rejected, 429). Live result now: exit 3, not registered.
  - `537ebf7d`: `helps/cursorfixture` (format `cursor-run-fixture/v1`, Recorder, Redactor, redaction guard test). The 2 fixtures are *reconstructed* from spike summaries because the spike kept no frame bytes.
  - `7f7484b1`: `scripts/cursor/` launchd watch + daily plists and `maintenance.sh`. Committed, NOT installed.
  - Gate: gofmt clean except pre-existing `internal/pluginhost/host.go`. `go test ./...` green except pre-existing `internal/client/codex/live` (TestPionMediaRelayBridgesAudioAndDataChannel) and `internal/discovery` (TestAdvertiserAndBrowser_Integration); both also fail at `58dc462a`.
  - Next: install the plists after review; Phase 2 wire client (feed a `Recorder` to replace the reconstructed fixtures).
- 2026-10-21, Phase 3 auth (branch `feat/cursor-auth`): landed. An earlier agent's draft `internal/auth/cursor/cursor.go` was reviewed and kept as is.
  - `f4470002`: `internal/auth/cursor` adds the poll login, API key exchange, refresh (API key only; everything else returns `ErrReloginRequired`) and a near-expiry re-login warning limited to once per 6 h. It also adds `--cursor-import-agent`: the macOS keychain `cursor-*`/`cursor-user`, with `~/.cursor/auth.json` as fallback and the email from `cli-config.json`. Auth file `type: "cursor"` with `access_token`, `refresh_token`, `api_key?`, `expired` (JWT exp), `email`, `auth_kind`. File name: `cursor-<email>-<sha(sub)>.json`.
  - `a7d41a21`: `USAGE-TRACKING.md`. `DashboardService/GetCurrentPeriodUsage` + `GetHardLimit` + `GetPlanInfo` on api2.cursor.sh, checked with 3 live calls. Recommends `/v8/management/quota/{provider}`.
  - `64985531`: `sdk/auth/cursor.go` (browser login, plus API key when `Metadata["api_key"]` is set) and the `--cursor-login` / `--cursor-api-key` flags. The key comes from `CURSOR_API_KEY` or a prompt, never from argv. Registered in both auth managers. Refresh lead is 7 d; refresh is a no-op until an executor exists (Phase 4).
  - `531c5072`: `StartOAuthV8` `case "cursor"` (background poll like Meta), `NormalizeOAuthProvider`, a TUI entry and the v0 `/cursor-auth-url` route.
  - Live: import OK (Team account, JWT exp 2026-12-19, no API key in the keychain, so it can't be refreshed). Browser login prints the URL and live `auth/poll` stays pending (404) for 2 min without errors; the approval step needs a human and was not run. A fake API key gets 401 from `exchange_user_api_key`. `GetMe` returns 200 with `email`.
  - Gate: gofmt clean (pre-existing `pluginhost/host.go` excepted); build OK; tests green in auth/cursor, sdk/auth, sdk/cliproxy, internal/api/..., tui, cmd.
  - Next: Phase 4 executor `Refresh` delegates to `CursorAuth.Refresh`. Usage tracking implementation once `feat/quota-routing` is merged.

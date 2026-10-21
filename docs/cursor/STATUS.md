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

# Upstream watch

Both forks track router-for-me upstreams:

| Fork | Path | Upstream | We merge |
|---|---|---|---|
| Proxy | `/Users/szymon/private/CLIProxyAPI` | `router-for-me/CLIProxyAPI` | `upstream/main` |
| Panel | `/Users/szymon/private/Cli-Proxy-API-Management-Center` | `router-for-me/Cli-Proxy-API-Management-Center` | release tags (`vX.Y.Z`, see its `CUSTOM.md`) |

`scripts/upstream-watch.sh` runs once a week and reports how far each fork has drifted. It **never merges**. It does not check out branches or touch working trees. The only write is `git fetch upstream --tags` in each repo. Merging stays a manual step (below).

## What it checks

For each repo:

- **Behind / ahead.** Proxy: `main..upstream/main` and `upstream/main..main`. Panel: `main..<latest v* tag>` and `<tag>..main`. "Latest" means the highest version by `git tag --sort=-v:refname`. The report also names the tag `main` is currently based on (`git describe --tags main`).
- **Claude-critical** (proxy only). These are upstream commits not yet in `main` that touch any path in `CLAUDE_CRITICAL` at the top of the script. Those paths decide whether Claude requests keep working:
  - the Claude executor, covering request building, cloaking, signing and auth (`internal/runtime/executor/claude_*.go`), plus its helpers (`helps/claude_*`, `helps/cloak_*`, `helps/utls_client.go`);
  - the Claude Code version, User-Agent and fingerprint headers (`internal/config/claude_fingerprint_profile.go`, `internal/misc/header_utils.go`, `internal/misc/claude_code_instructions.*`);
  - Claude OAuth login, tokens and refresh (`internal/auth/claude/**`, `internal/cmd/anthropic_login.go`, `sdk/auth/claude.go`, `sdk/auth/refresh_registry.go`);
  - the sdk/cliproxy auth refresh loop (`sdk/cliproxy/auth/auto_refresh_loop.go`, `conductor_refresh.go`);
  - thinking-signature validation and its cache (`internal/signature/claude*.go`, `internal/cache/signature_cache.go`);
  - the `/v1/messages` handler (`sdk/api/handlers/claude/**`) and both translator directions (`internal/translator/claude/**`, `internal/translator/*/claude/**`).

  The panel is not on the request path, so it has no Claude-critical list. When files move upstream, update the globs; they are git pathspec globs.
- **Conflict preview.** This lists the files changed on both sides since the merge base, `git diff --name-only $(git merge-base main U)..main` ∩ `..U`, where U is `upstream/main` or the tag. It also lists the upstream commits that touch those files. A file on this list does not guarantee a conflict, but every textual conflict comes from it.

## Output and notifications

- `logs/upstream-watch/<YYYY-MM-DD>.md`: the full report. A rerun on the same day overwrites it.
- `logs/upstream-watch.log`: one line per run, for example:
  `... proxy behind 0 ahead 42 critical 0 conflicts 0c/0f fetch ok | panel v1.26.6->v1.26.7 behind 1 ahead 4 conflicts 1c/7f fetch ok [log-only] ...`
  Here `conflicts Nc/Mf` means N upstream commits touch M files that both sides changed. The bracket shows the outcome: `notify`, `notify(suppressed)` or `log-only`.
- `logs/upstream-watch.launchd.log`: the job's stdout and stderr.
- A macOS notification fires **only** when the proxy has Claude-critical commits, or when either repo is more than 150 commits behind (`BEHIND_THRESHOLD`). Otherwise the run only writes the log. `NO_NOTIFY=1` suppresses the notification for test runs.
- If a fetch fails (offline, for example), the report says so and uses the last fetched refs, and the log line shows `fetch FAILED`.

### Reading the report

1. Read **Claude-critical** first. Each commit lists the matching files. Read those diffs (`git show <sha> -- <file>`) before merging. They can change headers, cloaking or token refresh, and a mistake there gets Claude requests rejected or accounts flagged.
2. The **conflict preview** tells you how much manual resolution to expect. Fork-only files (for example the cursor provider and quota routing) never show up here unless upstream touches them too.
3. Only the size of **Behind** matters. Small, frequent merges are cheaper than one large one.

Run manually: `NO_NOTIFY=1 scripts/upstream-watch.sh` (it prints the summary line).

## Schedule

`scripts/com.szymon.upstream-watch.plist` uses `StartCalendarInterval` to run every Monday at 09:00. If the Mac is asleep at that time, launchd runs the job once after wake.

```bash
cp scripts/com.szymon.upstream-watch.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.szymon.upstream-watch.plist
launchctl kickstart gui/$(id -u)/com.szymon.upstream-watch   # run now
launchctl print gui/$(id -u)/com.szymon.upstream-watch       # state, last exit code
# uninstall: launchctl bootout gui/$(id -u)/com.szymon.upstream-watch
```

This is its own job. Never bootout or bootstrap `com.szymon.cliproxyapi` (the live proxy) as part of this.

## Manual merge: proxy

```bash
cd /Users/szymon/private/CLIProxyAPI
git status                       # clean, on main
git fetch upstream --tags
git merge upstream/main          # resolve conflicts; keep fork behavior, take upstream fixes
gofmt -l .                       # must print nothing
go build -o test-output ./cmd/server && rm test-output
go test ./...
scripts/deploy-local.sh          # rebuilds bin/cli-proxy-api-live from main and kickstarts the proxy
git push origin main
```

`go test ./...` has 2 known pre-existing failures: `internal/client/codex/live` and `internal/discovery`. Anything else failing is new and blocks the deploy. Before deploying, review each Claude-critical commit from the report against our cloaking and fingerprint changes. After the deploy, send one real Claude request through the proxy. Rollback is in the header of `scripts/deploy-local.sh`.

## Manual merge: panel

```bash
cd /Users/szymon/private/Cli-Proxy-API-Management-Center
git status                       # clean, on main
git fetch upstream --tags
git merge vX.Y.Z                 # the latest release tag from the report; not upstream/main
bun run verify                   # test + lint + build
scripts/deploy-local.sh          # copies dist/index.html to the proxy's static/custom/management.html
git push origin main
```

The proxy reads `management.html` from disk on each request, so the panel needs no proxy restart. To check that the new build is served, compare `curl -s http://127.0.0.1:8317/management.html | shasum -a 256` with `shasum -a 256 dist/index.html`.

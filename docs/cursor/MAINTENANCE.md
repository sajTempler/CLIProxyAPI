# Cursor provider: maintenance tooling

Cursor's agent protocol is private and changes with each `agent` CLI release (about weekly). These tools turn a protocol change into a notification with a diff instead of a broken provider.

## Proto sync (`cmd/cursor_proto_sync`)

The agent bundle (`~/.local/bin/agent` → `~/.local/share/cursor-agent/versions/<ver>/`) is a webpack build. Its protobuf-es messages carry compact descriptors (`static $(){return["Name|1 field 9|2 other #0?", Ref]}`). The tool resolves them, including cross-module references, into a schema. It then keeps the closure of the RPCs we call (`roots` in `main.go`: `AgentService/Run`, `AgentService/GetUsableModels`, `ServerConfigService/GetServerConfig`).

Regenerate (from the repo root):

```bash
go run ./cmd/cursor_proto_sync
gofmt -l internal/runtime/executor/helps/cursorpb   # must print nothing
go test ./cmd/cursor_proto_sync ./internal/runtime/executor/helps/cursorpb
```

Output in `internal/runtime/executor/helps/cursorpb/`:
- `proto/<pkg>/<file>.proto`: readable `.proto` sources. Their names mirror the bundle modules.
- `<pkg>_<file>.pb.go`: Go types in one package, `cursorpb`.
- `SNAPSHOT.txt`: the agent version, the root RPCs, and every generated message's field list. `--check` diffs against it.
- `doc.go` and `*_test.go` are hand-written. The generator never touches them.

Reproducibility: the Go code comes from protoc-gen-go **in-process**: `google.golang.org/protobuf/cmd/protoc-gen-go/internal_gengo` at the version pinned in `go.mod` (currently **v1.34.1**). No `protoc` or plugin binary is involved, and the generated headers say `protoc (unknown)`. The same bundle produces byte-identical output. Only bumping `google.golang.org/protobuf` in `go.mod` changes the generator. Use `--bundle-dir <dir>` to generate from a specific version directory instead of the `agent` symlink.

### `--check`

```bash
go run ./cmd/cursor_proto_sync --check   # exit 0 ok, 1 breaking, 2 tool failure
```

It compares the installed agent with `SNAPSHOT.txt` and prints:
- **BREAKING** (exit 1), from the `used` map in `main.go` (message → fields the provider relies on). It fires when:
  - a root RPC is removed or its input/output changes;
  - a used message is removed;
  - any field of a used message is renumbered or retyped, or flips to or from repeated;
  - a *listed* field is removed.
- **New exec request types**: new `ExecServerMessage` variants. They are not fatal, because unknown execs get soft-denied, but someone should look.
- **Other changes**: removed fields we don't rely on, renames, new fields in used messages, new messages.

Removing an unlisted field is not fatal on purpose. Between agent 2026.09.26 and 2026.10.19, Cursor removed 9 fields from messages we use (for example `adopt_args` and `prompt_suggestion`), and none of them matter to us. Add a field to `used` as soon as provider code reads or writes it.

After a breaking report: regenerate, then `go build ./...`. Compile errors point at the code that used the changed fields. Fix the code, run the golden-fixture tests and the canary, then commit the regenerated package and `SNAPSHOT.txt` together.

## Golden fixtures (`internal/runtime/executor/helps/cursorfixture`)

These are recorded `Run` streams that tests replay, so `go test` never talks to Cursor. Each fixture is one JSON file under `cursorfixture/testdata/runs/`, in format `cursor-run-fixture/v1`. The package doc comment documents the format. In short:
- Ordered `frames` with `dir` (`client`/`server`), `at_ms`, and `msg`. `msg` is the `AgentClientMessage`/`AgentServerMessage` as protojson with proto field names.
- An optional Connect `end_stream` JSON.
- `Fixture.ServerConnectStream()` rebuilds the Connect-framed response body for wire-level tests.

Redaction: `Recorder` passes every message through `Redactor` before storing it. Redactor replaces:
- JWTs, bearer tokens and API-key-shaped strings;
- emails;
- `/Users/<name>` and `/home/<name>` paths;
- the machine's user and host names;
- the whole value of credential-named fields (`*token*`, `*secret*`, `*credential*`, ...).

UTF-8 `bytes` fields, such as KV blobs, are redacted as text. This means blob ids no longer match the redacted blob data. `TestCommittedFixturesAreValidAndRedacted` fails if a committed fixture still matches a secret pattern or this machine's identity. Review the JSON diff anyway before committing.

Current fixtures are `source: "reconstructed"`. The Phase 0 spike kept only per-run summaries, not frame bytes, so the two files were rebuilt from runs 11 and 10 of the spike:
- What's real: event order, exec types, tool names/args/results and token counts.
- What's condensed or a placeholder: text and thinking are condensed, and ids are placeholders.
- What's left out: checkpoint and KV traffic.

Replace them with `source: "recorded"` fixtures once the Phase 2 wire client can feed a `Recorder`.

## Client version (`helps/cursor_version.go`)

`GetCursorClientVersion()` and `CursorClientVersionHeader()` (`cli-<ver>`, the value for `x-cursor-client-version`) come from the `~/.local/bin/agent` symlink target. `StartCursorVersionUpdater(ctx)` reads it at start and then every 10 minutes. If the symlink breaks, the updater keeps the last good version. Without an installed agent (CI, other machines), it uses `agent_version` from the embedded `cursorpb/SNAPSHOT.txt`. Nothing calls the updater yet; Phase 4 starts it next to the xAI one.

## Canary (`cmd/cursor_canary`)

```bash
go build -o bin/cursor_canary ./cmd/cursor_canary && bin/cursor_canary [--model cursor/composer-2.5]
```

The canary runs two streaming `/v1/messages` turns through the running proxy (`127.0.0.1:8317`; the key is `CPA_API_KEY` from the environment or `.env`, sent as a Bearer token like `claudep`):
1. `lookup_secret` → expect `tool_use`;
2. `tool_result` → expect a final answer containing the secret with `end_turn`.

Use the built binary, because `go run` maps every non-zero exit to 1.

| Exit | Meaning | Notifies |
|---|---|---|
| 0 | round-trip ok | no |
| 1 | protocol broken (bad status, malformed stream, no `tool_use`, lost `tool_result`) | yes |
| 2 | Cursor auth expired / rejected upstream | yes |
| 3 | cursor provider not registered (no `cursor/*` models, or "unknown provider") | **no** |
| 4 | inconclusive: proxy down, `CPA_API_KEY` missing/rejected, rate limited | yes |

The `--timeout` deadline (default 3 minutes) is deliberate. This is a scheduled utility, not the proxy runtime.

## Schedules (`scripts/cursor/`)

- `maintenance.sh watch`: waits 60 s for the updater to settle, then reads the active agent version. It skips the run if this version was already checked successfully (`logs/.cursor-maintenance-last-version`). Otherwise it builds both tools into `bin/`, then runs `cursor_proto_sync --check` and then the canary.
- `maintenance.sh daily`: canary only.
- Logging: everything goes to `logs/cursor-maintenance.log`.
- Notifications: failures, breaking changes and new exec types raise a macOS notification through `osascript`. `CURSOR_MAINTENANCE_NO_NOTIFY=1` silences them for manual runs.
- `com.szymon.cursor-agent-watch.plist`: `WatchPaths` on `~/.local/share/cursor-agent/versions`, throttled to one run per 5 minutes.
- `com.szymon.cursor-canary.plist`: daily at 09:30.

Install (not done automatically):

```bash
cp scripts/cursor/com.szymon.cursor-*.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.szymon.cursor-agent-watch.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.szymon.cursor-canary.plist
# test now: launchctl kickstart gui/$(id -u)/com.szymon.cursor-canary
```

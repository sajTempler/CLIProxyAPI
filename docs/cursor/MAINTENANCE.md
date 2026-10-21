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

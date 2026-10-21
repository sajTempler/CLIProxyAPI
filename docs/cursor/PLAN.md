# Plan: `cursor` upstream provider (native, Track A)

Status: planned, not started (2026-10-21).

## Goal

One harness: Claude Code. It talks to this proxy, which serves Anthropic
models through the `claude` provider and Cursor-account models through a new
`cursor` provider. Claude Code's own tools must work on Cursor models, so the
provider speaks Cursor's wire protocol natively (Track A). The subprocess
wrapper (Track B) can't return tool calls and is dropped.

Accepted cost: the protocol is private and changes with each `agent` release
(~weekly). The maintenance tooling below is part of the feature, not an extra.

Context pointers (read these, don't re-derive):
- `provider-integration-map.md`: every touch point for a new provider. Template: Devin (`f9475276`, `44e62bc8`): pivot format + native wire helper in `helps/`, no new translators.
- `cursor-research.md`: auth, transport, protocol shape, prior art, ToS.
- `proto-descriptors.txt`: protobuf descriptors extracted from `agent` 2026.10.19-6bbf2c4.
- Reference client: oakimov/cursor-opencode-provider (TS, native `Run`, held-open streams, blob store).

## Shape

```
Claude Code ──/v1/messages──▶ proxy ──claude──▶ api.anthropic.com
                                   └─cursor──▶ AgentService/Run (Connect, HTTP/2, protobuf)
```

- Pivot format: `FormatClaude` (the only client is Claude Code; no lossy hop).
- Claude Code tools → offered to Cursor as MCP tools; MCP exec requests → `tool_use` blocks.
- Cursor built-in exec requests (shell, read, write, grep, ...) → soft-deny so the model falls back to the offered tools.
- Stateful sessions (Phase 0 result, `SPIKE.md`): the server ignores `conversation_history`, so replay is out. Each Claude Code conversation gets a store with the last checkpoint and its KV blobs; a tool round-trip holds the `Run` open until the `tool_result` arrives.
- System prompt: one `global` rule in `RequestContext.rules` (the only channel that dominates). No `x-cursor-checksum`.

## Phase 0: spike (go/no-go, throwaway, ~2 days)

Minimal Go or TS script against your account, no proxy integration. Answer:
1. Does a `Run` with `conversation_history` containing tool calls + results continue correctly (replay works)?
2. With built-ins soft-denied, does the model use offered MCP tools reliably (try composer and one Claude/GPT model)?
3. Is `x-cursor-checksum` required on `Run`?
4. Can Cursor's system prompt be neutralized enough that Claude Code's system prompt dominates?

No-go on 1 → held-open streams become mandatory (bigger build). No-go on 2 → stop; Track A doesn't serve the goal.

## Phase 1: maintenance tooling (before feature code)

So every later phase is built on a pit of success.

1. **Proto extract + diff** (`cmd/cursor_proto_sync`)
   - Input: the installed `agent` bundle (`~/.local/bin/agent` → `~/.local/share/cursor-agent/versions/<ver>/`).
   - Output: generated `.proto` + Go code under `internal/runtime/executor/helps/cursorpb/`, and a snapshot `cursorpb/SNAPSHOT.txt` (version + per-message field list).
   - `--check` mode: diff against the snapshot; report added/removed/renumbered fields in the messages we use, and new exec request types. Non-zero exit on breaking changes in used messages.
2. **Client version from the installed binary** (`helps/cursor_version.go`)
   - Mirror `helps/xai_version.go`: read the version from the `agent` symlink target at startup and on a timer; used for `x-cursor-client-version`.
   - Fallback to the version in `SNAPSHOT.txt` when `agent` isn't installed (e.g. CI).
3. **Live canary** (`cmd/cursor_canary`)
   - Through the running proxy: one `/v1/messages` turn on the cheapest Cursor model with one tool; assert a `tool_use`, send a `tool_result`, assert a final text answer.
   - Exit codes: 0 ok, 1 protocol broken, 2 auth expired.
4. **Golden fixtures**: recorded, redacted `Run` streams replayed in unit tests, so `go test` never hits Cursor.

Schedules (local launchd, because they need the keychain token and the installed `agent`):
- `com.szymon.cursor-agent-watch`: `WatchPaths` on `~/.local/share/cursor-agent/versions`; on a Cursor update, run `cursor_proto_sync --check`, then the canary.
- `com.szymon.cursor-canary`: daily canary.
- Both: macOS notification + `logs/cursor-maintenance.log` on failure.

## Phase 2: protocol client (`helps/cursor_wire.go`, `helps/cursor_session.go`)

Design from `SPIKE.md` ("Recommendation"); replay through `conversation_history` does not work.
- Connect bidi stream over HTTP/2; host discovery via `ServerConfigService/GetServerConfig`; headers from Phase 1; no `x-cursor-checksum`.
- Session store per conversation: last `conversation_checkpoint_update` bytes + KV blobs (answer `get_blob`/`set_blob` from it).
- Tool round-trip: end the Claude response with `stop_reason: tool_use`, keep the `Run` open (heartbeats), answer `mcp_result` when the next request brings the matching `tool_result`. Registry of pending exec ids keyed by `tool_use_id`.
- New user turn: new `Run` with the stored checkpoint + `user_message_action`. Lost `Run`: checkpoint + `resume_action`, match the regenerated `mcp_args` to the cached `tool_result`. No state (first request of an existing conversation, or history from another provider): transcript seeded into the user text.
- System prompt + "built-ins are unavailable, use the `claude-code` MCP tools" as one `global` rule. Never `system_prompt_spec`/`system_prompt_override`/`custom_system_prompt`.
- Tools via `mcp_meta_tool_options` with full descriptors; answer `mcp_state_exec_args`; hide `get_mcp_tools`/`mcp_tool_call` updates from Claude Code.
- Soft-deny every built-in exec (typed `rejected`/`error`, `stream_close` for `shell_stream`), log the type name at warn level.
- Stream events → Claude SSE events (text, thinking, `tool_use`, usage from `turn_ended`, stop reason).

## Phase 3: auth (`internal/auth/cursor`, `sdk/auth/cursor.go`, `--cursor-login`)

- PKCE poll flow (`cursor.com/loginDeepControl` → `api2.cursor.sh/auth/poll`), plus `--cursor-api-key` import (refreshable via `auth/exchange_user_api_key`).
- Auth file `type: "cursor"`: `access_token`, `refresh_token`, `api_key` (optional), `expired` (JWT exp, ~60 days).
- Refresh lead in `sdk/auth/refresh_registry.go`; browser-only logins warn near expiry (canary exit 2).

## Phase 4: executor + registration

- `cursor_executor.go`: `RequestToFormat` = `FormatClaude`; thinking via existing Claude applier; payload config applied last (AGENTS.md) + regression test.
- `CountTokens`: estimate (Devin pattern).
- Registration touch points: see `provider-integration-map.md` §4, §6, §8.
- Models: `GetCursorModels()` from `AgentService/GetUsableModels` with a static fallback (Devin pattern, not `models.json`). Expose with a `cursor/` prefix so Claude Code model names never collide with the `claude` provider.

## Phase 5: management + TUI

`StartOAuthV8` case, `NormalizeOAuthProvider`, TUI `oauth_tab.go` (+ the v0 auth-url route it needs). Web dashboard login button goes in with the dashboard work (external Management Center repo).

## Phase 6: Claude Code fit

- `claude --model cursor/<id>` end to end with real tool use in a scratch repo.
- Cross-provider thinking signatures (`internal/signature/provider_compatibility.go`) when a session mixes Claude and Cursor models.
- Document the switch in `docs/cursor/USAGE.md`.

Every phase ends green on `gofmt -l .`, `go build ./cmd/server`, `go test ./...`, and from Phase 2 on, the canary.

## Risks

- ToS §1.5 (reverse engineering) and §9 (suspension at will): personal use only, never expose the endpoint.
- Built-in Cursor tool bias may make models ignore Claude Code tools (Phase 0 Q2).
- Maintenance: expect a fix every few weeks; the Phase 1 tooling turns that from a surprise into a notification with a diff.

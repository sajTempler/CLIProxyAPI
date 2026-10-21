# omsub evaluation (yobo2u/omsub), 2026-10-21

Read-only review of a clone at `temp/omsub-eval/omsub`, `cursor` branch, tag v0.6.6 (`c43af3c`). I did not run or build anything, and I used no credentials.

## TL;DR

omsub is not a standalone service. It is a **CLIProxyAPI native plugin**: a Go/CGO `c-shared` library that implements the plugin ABI already in our tree (`internal/pluginhost`, `sdk/pluginabi`). It speaks Cursor's native `agent.v1.AgentService/Run` protocol (Connect + protobuf over HTTP/2) and exposes Cursor models as an OpenAI chat-completions executor. The host's existing translators then serve Claude `/v1/messages` on top of it.

In effect it is a working version of PLAN.md Phases 0, 2 and 3, written in Go with an MIT license. It was built for OpenCode, not Claude Code, so it has gaps that matter for us (listed below).

Recommendation: **load it in-process as the Phase 0 spike (variant A′), then port its Cursor core into the native provider (B).** Running it as a separate service (A) has no advantage over A′. Ignoring it (C) wastes about 2 weeks of solved protocol work.

## 1. What it is

| | |
|---|---|
| Language | Go 1.25; dependencies are only `google.golang.org/protobuf` and `testify`. Built with `-buildmode=c-shared` (`cursor.so`). |
| License | Repo root: Apache-2.0. `cursor-plugin/`: **MIT**. Third-party notices credit opencodex (MIT) for the protocol and CLIProxyAPI (MIT) for the ABI. |
| Size | About 7.6k lines of Go source and 8.7k lines of tests (70 test files). There is also a 33 KB management HTML page. |
| Activity | Created 2026-08-09. 56 commits from 2026-08-22 to **2026-10-01** (nothing in the last 3 weeks). 7 stars, 5 forks. |
| Maintainers | **One person.** Commits come from "Gu Tao" and "yobo2u", who share the e-mail `itboys@msn.com`. 4 outside PRs are open and none has been merged (ARM64, usage/quota, model catalog). Docs are mostly in Chinese. |
| Branches | `main` holds only `LICENSE`. All the code is on the **`cursor`** branch. |
| Upstreams | Cursor only. Inference goes to `api2.cursor.sh`; login uses `cursor.com/loginDeepControl`. |
| Client API | The host gives it OpenAI **chat-completions** in and out (`executor_input_formats: ["chat-completions"]`). It does not implement Responses. Claude `/v1/messages` works only through the host's Claude↔OpenAI translators (`internal/translator/openai/claude`). |
| Host compatibility | Verified upstream against CLIProxyAPI v7.3.17. It declares `schema_version: 3` and ABI 1; our tree (v8.0.37) has ABI 1 and schema 6, and accepts versions ≤ 6. The host supports `.dylib` on darwin through CGO. Releases are **Linux amd64 only**, so macOS needs a local build. |

## 2. Cursor transport and auth

- **Native wire protocol.** It does not use the `agent` subprocess or `@cursor/sdk`. `cursorapi/run.go` opens a bidi `POST /agent.v1.AgentService/Run` with `application/connect+proto`. The request body is an `io.Pipe`, so the client can send heartbeats every 5 s plus exec/KV replies on the same stream. Models come from `AgentService/GetUsableModels`.
- **Protobuf** uses `dynamicpb` over an embedded binary descriptor, `cursorproto/agent.desc` (67 KB, taken from opencodex). A hand-written `mcp_state.textproto` patches in `McpStateExecArgs/Result` (field 36). Fields are accessed by name, so if a field disappears the code fails with a clear "field X unavailable" error instead of silently mis-encoding.
- **Headers:** `authorization: Bearer`, `x-cursor-client-version` hard-coded to **`cli-2026.07.16-899851b`**, `x-cursor-client-type: cli`, `x-ghost-mode: true`, and `x-session-id`/`x-request-id` as random UUIDs. **There is no `x-cursor-checksum`.** That still worked as of 2026-10-01, which answers PLAN Phase 0 Q3 ("not required, for now").
- **Auth:** a PKCE poll flow. `loginDeepControl?challenge&uuid&mode=login&redirectTarget=cli` is followed by polling `api2.cursor.sh/auth/poll`. This runs through the host's `auth.login.start/poll`, which means the management UI / OAuth flow. The credential JSON has `type: "cursor"`, `access_token`, `refresh_token`, and `expires_at` (JWT exp − 5 min), and the host stores it in its auth dir.
- **Refresh:** `POST api2.cursor.sh/auth/exchange_user_api_key` with the **refresh token as Bearer**, called from the host's `auth.refresh`.
- **Not covered:** the macOS keychain token (`cursor-access-token`), `--cursor-api-key` import, and `GetServerConfig` host discovery. The base URL is fixed to `https://api2.cursor.sh`.

## 3. Tools, streaming, thinking, usage

**Client tools → Cursor.** OpenAI `tools` become MCP tool definitions under one fake provider:
- They are sent in `RunRequest.mcp_tools`.
- They are also returned when the server issues `mcp_state_exec_args`, the MCP catalog re-query. This is how it gets past the "mcp_tools is prewarm-only" issue noted in cursor-research.md.

**Cursor → `tool_use`.** An `exec_server_message.mcp_args` or `tool_call_completed.mcp_tool_call` from that provider becomes an OpenAI `tool_calls` delta. The host then turns it into a Claude `tool_use`.

**No held-open streams.** On the **first** tool call the run is marked terminal. After a 100 ms drain the stream is closed without answering the MCP exec. As a result:
- **Only one tool call per turn is surfaced.** Any later tool calls in the same turn are dropped, so Claude Code's parallel tool calls get serialized.
- The next request (carrying the tool result) is handled by **replay**:
  - Full replay: the **whole transcript is flattened into one user-message string**. Lines look like `User: …`, `Assistant tool call NAME (ID): {json}`, `Tool result NAME (ID): …`, then the line "Continue: the requested tool results above are complete…". The system prompt is prepended to that text. It does not use `conversation_history`, `custom_system_prompt`, or `system_prompt_spec`.
  - Checkpoint suffix: an optimization. It keeps Cursor's `conversation_checkpoint_update` bytes in memory (15 min, 64 entries, 16 MiB, keyed by account, model and session). The next request sends the checkpoint plus only the new messages, flattened as above. Checkpoints are **not committed after turns that end in a tool call**. Most Claude Code turns end in a tool call, so checkpoints rarely advance.
  - Session identity comes from host metadata, or from the headers `X-Session-ID`, `Session_id` or `X-Client-Request-Id`, or from the body fields `conversation_id`/`session_id`. Claude Code's `X-Claude-Code-Session-Id` is not recognized unless the host derives a session ID, so expect mostly full replay.

**Built-in exec requests:**
- `read_args`, `grep_args`, `shell_args`, `shell_stream_args`: answered with a protocol-correct failure. The failure text tells the model to use the matching client tool.
- `request_context_args`: answered with env only (time zone, workspace = **the proxy process's cwd**, project folder `~/.cursor/projects/<name>`).
- KV `get/set_blob`: served from an in-memory store (64 MiB cap).
- `write_args`: accepted **only for generated images**. These are written under `~/.cursor/projects/<name>/assets/` and deleted after the run. Path traversal is checked.
- `interaction_query`: web search, Exa, switch-mode and ask-question are rejected. Create-plan gets an error. Image generation is auto-approved.
- **Not handled:** `write_args` (text), `delete`, `ls`, `fetch`, `diagnostics`, `pi_*`, and the other exec types. **They get no reply, and the 90 s progress watchdog kills the turn.** PLAN wants a generic soft-deny for these instead.

**Claude Code bug.** `nativeExecPolicy` matches client tools by exact lowercase name (`bash`, `read`, `read_file`, `grep`, …). Claude Code's tools are `Bash`, `Read` and `Grep`. Cursor would therefore be told "no matching client tool is exposed. Report that the operation is unavailable and stop", which could kill agent turns. The fix is one line (case-insensitive matching plus Claude Code names).

**Streaming:** SSE text and tool-call deltas work. Images are returned as a CPA `delta.images` extension.

**Thinking:** `thinking_delta` is decoded and then **dropped**. No reasoning is returned. `requested_model.parameters` (effort, thinking, max mode, context) are never set; only `model_details.model_id` is.

**Usage:** **estimated as runes/4**. The real counts in `turn_ended` and `token_delta` are ignored, and `max_tokens` is not enforced upstream.

## 4. Tracking Cursor releases

None of this is automated:
- The client version is a constant, about 3 months old.
- `agent.desc` has changed twice, both times by hand, plus one textproto patch.
- There is no extractor, no diff, no canary, and no checksum.

Breakage would show up as runtime errors. PLAN Phase 1 tooling is still needed whichever option we choose. Its `dynamicpb` design does make Phase 1 simpler: refreshing the protocol mostly means swapping `agent.desc` for one extracted from the installed `agent` bundle (`proto-descriptors.txt`).

## 5. Quality and risk

**Good:**
- Small, readable, idiomatic Go.
- Heavy table tests, including HTTP-level fake Cursor servers.
- CI runs `go test -race -shuffle`.
- Defensive limits everywhere: frame, blob, output and context budgets, and watchdogs.
- Clear errors, honest docs (for example "quota unavailable" instead of a fabricated number).

**Endpoints:** the only remote endpoints in the code are `api2.cursor.sh` and `cursor.com/loginDeepControl`.

**No exfiltration:**
- No telemetry, no exec of local binaries, no keychain reads.
- Tokens go only to the Cursor endpoints. Tokens are stored by the host, never in plugin config or logs.
- The management page has no external assets. It asks for the management key and keeps it in memory only.
- The request interceptors are pass-through. They only record the `selected_auth_id` for local counters.

**Flags:**
- **In-process trusted code.** A CGO plugin runs inside the proxy that also holds the Claude OAuth tokens. The interceptors see every request body (they are pass-through today). Build from reviewed source, pinned to a commit.
- **Prebuilt binaries are committed** in `cursor-plugin/release/` (12 files, 74 MB, Linux). Do not use them. Build from source.
- Single anonymous maintainer, stale for 3 weeks, ToS exposure (the same ToS §1.5 risk as our own plan).
- It sends the proxy's cwd as the Cursor workspace path. This is minor, but run the proxy from a neutral directory.
- Text flattening of tool history probably costs quality and tokens compared with structured history. Nobody has measured this with Claude Code.

## 6. Integration options, ranked

### A′ (rank 1, as the Phase 0 spike): load omsub in-process as a CLIProxyAPI plugin

This needs no proxy source changes and does not touch upstream merges.

```sh
# from a pinned, reviewed checkout, after applying our patches (tool-name casing)
cd temp/omsub-eval/omsub/cursor-plugin
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o ../../../../plugins/cursor.dylib .
```

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cursor:
      enabled: true
      priority: 1
```

Log in through the management OAuth flow (provider `cursor`), then run `claude --model cursor/<id>`. The Claude→chat-completions translation is automatic.

- **Effort:** about 1 day, including the tool-name patch.
- **What it answers:** PLAN Phase 0 Q1, Q2 and Q4 for Claude Code, using code that already works for OpenCode.
- **Maintenance:** we would own a fork. Every Cursor change means patching someone else's design, which is OpenAI-pivot, text-replay and single-tool-call.

### B (rank 2, the target): port its Cursor core into the native provider per PLAN.md

Copy and adapt `cursorproto`, `cursorapi` and `cursorauth` (MIT, so keep the notice) into `internal/runtime/executor/helps/cursor*`.

At the same time, fix what PLAN needs:
- `FormatClaude` pivot.
- Structured `conversation_history` instead of text flattening, and evaluate held-open streams only if the A′ spike shows replay quality is poor.
- Multiple tool calls per turn.
- Thinking passthrough.
- Real usage from `turn_ended`.
- Generic soft-deny for unknown exec types.
- Client version read from the installed `agent`.
- `GetServerConfig` host discovery.
- Keychain/API-key import.
- Replace `agent.desc` with the Phase 1 extractor's output.

**Effort:** about 1.5–2 weeks instead of about 4 from scratch. The protocol client, KV store, exec replies, PKCE/refresh and model discovery are already solved and tested.

**Maintenance:** same as PLAN. It is our code, in our idiom, and Phase 1 tooling catches drift.

**Convention conflict:** omsub has stream watchdogs (30 s first-data, 30 s frame silence, 90 s progress, 15 min overall). AGENTS.md forbids timeouts after the upstream connection is established. The port has to drop them, rely on soft-denying every exec type so turns never stall, or add an explicit documented exception like the Codex websocket one.

### A (rank 3): omsub as a separate service behind an `openai-compatibility` group

omsub cannot run alone. It would have to be hosted by a **second** CLIProxyAPI instance, as `cursor-plugin/test-config.yaml` does on port 18317, and the main proxy would forward to it:

```yaml
api-keys:
  openai-compatibility:
    - name: "cursor"
      prefix: "cursor"                        # clients call cursor/<model>
      base-url: "http://127.0.0.1:18317/v1"   # second CPA instance hosting the plugin
      headers:
        X-Session-ID: "$X-Claude-Code-Session-Id"   # lets omsub's checkpoint cache key on the CC session
      keys:
        - api-key: "<inner instance access key>"
      models:
        - name: "cursor/claude-opus-4-8"      # inner model id (plugin adds cursor/)
          alias: "claude-opus-4-8"
        - name: "cursor/composer-2.5"
          alias: "composer-2.5"
```

The only gain is process isolation: a crash in the CGO plugin would not take down the Claude path. The costs are two processes, two LaunchAgents, two auth stores, an extra hop, and a static model list. It has the same functional gaps as A′. Use it only if in-process stability turns out to be a problem.

### C (rank 4): ignore omsub

Not recommended. It is the closest working prior art in our language and for our exact host. Oakimov's TS client is more complete (held-open streams, checksum, version from the local CLI), but it is about 340 KB of TS in a different idiom.

## Recommendation

1. Do A′ as Phase 0: build the dylib from a pinned commit, apply the tool-name-casing patch, and test with Claude Code in a scratch repo. Measure two things: whether the models use the offered MCP tools, and whether flattened replay holds up over 20+ tool turns.
2. If the result is go, do B. Port omsub's protocol, auth and model code into the native `cursor` provider, then fix the gaps listed above.
3. Keep PLAN Phase 1 (proto extract and diff, version from the installed binary, canary) unchanged. omsub has none of it, and it is what makes either path sustainable.

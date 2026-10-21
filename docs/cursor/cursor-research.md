# Cursor provider for CLIProxyAPI: research notes

Date: 2026-10-21. I installed nothing and changed nothing. I printed no token values: for the keychain I checked only that the items exist, and for the JWTs I decoded only the claim names and lifetime.

Installed CLI: `~/.local/bin/agent` points to `~/.local/share/cursor-agent/versions/2026.10.19-6bbf2c4/cursor-agent`. It auto-updated, so the version is 2026.10.19, not 2026.10.01. The `2026.09.26` and `2026.10.01` versions are still on disk. The CLI is a bash launcher plus a bundled `node` and webpack chunks (`index.js` 7.6 MB, `544.index.js`, `2461.index.js`, `691.index.js`, and others). It uses protobuf-es with compact field descriptors embedded as strings. I extracted all 5,362 message descriptors to `/private/tmp/claude-501/-Users-szymon-private/2e261671-54df-4f3f-8f82-7fb2cbef9442/scratchpad/proto-descriptors.txt`, in the format `Name|<field#> <name> <type-or-#ref>...`. That file is enough to rebuild a `.proto` file.

Confidence key: **[H]** read directly in the local bundle, **[M]** inferred from the bundle or a third-party source, **[L]** a guess.

---

## 1. Auth

### Browser login (`agent login`) [H]
Source: `index.js`, module `../cursor-config/dist/auth/login.js`.
- The CLI generates `verifier = base64url(random 32 bytes)`, `challenge = base64url(sha256(verifier))` and `uuid = randomUUID()`.
- It opens `https://cursor.com/loginDeepControl?challenge=<c>&uuid=<u>&mode=login&redirectTarget=cli[&supportsSelectedTeamLogin=true]`. This is a browser login, not a deep link. `NO_OPEN_BROWSER` turns off the automatic browser open, and the CLI can also show a QR code.
- It then polls `GET https://api2.cursor.sh/auth/poll?uuid=<u>&verifier=<v>` up to 150 times, with backoff from 1 s up to 10 s. A 404 means "not yet". A 200 returns JSON with `accessToken` and `refreshToken`.
- API-key login: `POST https://api2.cursor.sh/auth/exchange_user_api_key` with `Authorization: Bearer <CURSOR_API_KEY>` and body `{}`. It returns `{accessToken, refreshToken}`. The API key itself is never used as the bearer token on inference calls; the CLI always exchanges it for the JWT.
- Overrides: `CURSOR_API_ENDPOINT` (or `-e`), `CURSOR_WEBSITE_URL`, `--api-key`, `CURSOR_API_KEY`, and the hidden `--auth-token` / `CURSOR_AUTH_TOKEN`, which supplies a JWT directly.

### Storage on macOS [H]
Source: `../cli-credentials/dist/index.js` and `../keychain/dist/index.js`.
- Default store is the macOS login keychain, written and read with `/usr/bin/security add/find-generic-password`.
  - Account: `cursor-user`
  - Services: `cursor-access-token`, `cursor-refresh-token`, `cursor-api-key`, `cursor-bedrock-access-key`, `cursor-bedrock-secret-key`, `cursor-bedrock-session-token`
  - On this Mac, `cursor-access-token` and `cursor-refresh-token` are present and `cursor-api-key` is absent.
- With `AGENT_CLI_CREDENTIAL_STORE=file`, credentials go to `~/.cursor/auth.json` (mode 0600). On Linux the path is `$XDG_CONFIG_HOME/cursor/auth.json`; on Windows it is `%APPDATA%\Cursor\auth.json`. Fields: `accessToken`, `refreshToken`, `apiKey`, `bedrockCredentials{accessKey,secretKey,sessionToken}`. With `AGENT_CLI_CREDENTIAL_STORE=memory`, nothing is persisted.
- Non-secret account and server data lives in `~/.cursor/cli-config.json`:
  - `authInfo{email,displayName,teamId,teamName,userId,authId,activeTeamId}`
  - `privacyCache{ghostMode,privacyMode}`
  - `serverConfigCache{backendUrl, agentUrlConfig{agentUrl,agentnUrl}, serverHttp2Config}`
  - `network.useHttp1ForAgent`
  - `selectedModel{modelId,parameters[]}` and `modelParameters`
- The Cursor IDE stores its own copy in `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb`, table `ItemTable`, under the keys `cursorAuth/accessToken` and `cursorAuth/refreshToken`.

### Token shape and refresh [H for the CLI, M for the IDE refresh]
- The access token is a JWT with `iss=https://authentication.cursor.sh`, `aud=https://cursor.com`, `type=session` and `scope=openid profile email offline_access`. Claims: `sub, exp, time, randomness`. Lifetime is **60 days**.
- **On this Mac the stored access token and refresh token are the same string.**
- CLI refresh logic (`./src/auth-refresh.ts`): a token counts as expiring when fewer than 300 s remain before `exp`. The CLI refreshes only when it has an API key, by calling `exchange_user_api_key` again. For a browser login, the CLI has no refresh call; it relies on the 60-day JWT and the user runs `agent login` again.
- IDE refresh, from `workbench.desktop.main.js`: `POST {backendUrl}/oauth/token` with JSON `{grant_type:"refresh_token", client_id:<authClientId>, refresh_token}`. The response may carry `shouldLogout`. oakimov/cursor-opencode-provider uses the same endpoint to renew browser logins.

### API key compared with OAuth
- A user API key comes from cursor.com/settings. It is exchanged for the same session JWT and can be re-exchanged without any interaction, so it is the most robust option for a server-side proxy.
- An OAuth/browser login needs a one-time interactive poll flow, then lasts 60 days, with optional refresh through `/oauth/token`.

---

## 2. Inference transport

### Hosts [H]
- Backend: `https://api2.cursor.sh`. It serves `aiserver.v1.*`, auth and `GetServerConfig`. Metrics go to `api3.cursor.sh`.
- Agent host: taken from `aiserver.v1.ServerConfigService/GetServerConfig`, field `agentUrlConfig`.
  - Code: `bd(backendUrl, ghostMode, agentUrlConfig, useHttp1)` returns `agentUrl` in privacy/ghost mode and `agentnUrl` otherwise.
  - Cached value on this Mac: both are `https://agentn.global.api5.cursor.sh`.
  - When HTTP/1.1 is forced, the agent calls go to the backend URL (`api2.cursor.sh`) instead.

### Protocol [H]
- **Connect RPC** via `@connectrpc/connect-node` (`createConnectTransport`), binary protobuf with gzip send compression.
  - Default: **HTTP/2** with a pool of 4 sessions and 10 s pings. `HTTPS_PROXY` is honoured.
  - Fallback: HTTP/1.1, used when `network.useHttp1ForAgent` is set or the server's `serverHttp2Config` says `FORCE_*_DISABLED`.
- Main RPC: **`agent.v1.AgentService/Run`**, bidirectional streaming. `AgentClientMessage` goes up and `AgentServerMessage` comes down.
- HTTP/1.1 bidi emulation:
  - `agent.v1.AgentService/RunSSE` (server streaming) takes `BidiRequestId{request_id}`.
  - Each client message is sent as unary `aiserver.v1.BidiService/BidiAppend` with `{request_id, append_seqno, data(hex of serialized AgentClientMessage) | data_binary}`.
  - `RunPoll` also exists.
- Other AgentService methods: `GetUsableModels`, `GetDefaultModelForCli`, `GetAllowedModelIntents`, `NameAgent`, `UploadConversationBlobs`, `GetPromptContextUsage`, `WarmUp`, and others.
- The CLI still bundles the legacy IDE-style `aiserver.v1.AiService` (`StreamChat`, `StreamComposer`, `AvailableModels`, `GetChatTitle`, ...), but its agent turns go through `agent.v1.AgentService/Run`.

### Headers set on every request by the interceptor (`index.js`, function `Y`) [H]
| Header | Value |
|---|---|
| `authorization` | `Bearer <session JWT>` |
| `x-ghost-mode` | `true` or `false`, from `privacyCache.ghostMode`; defaults to `true` |
| `x-cursor-client-version` | `cli-2026.10.19-6bbf2c4`, plus `-<channel>` on non-prod channels |
| `x-cursor-client-type` | the surface: `cli`; `acp` in ACP mode |
| `x-request-id` | random UUID |
| `x-cursor-team-id` | only when an active team is set |
| `x-cursor-streaming` | `true`, on agent-host calls in HTTP/1.1 mode |
| optional | `local-cli-mode`, `x-dev-experiment-overrides`, `x-cursor-host-app-name` / `-version` (ACP), and user `-H` headers |

Other headers in the bundle:
- `x-cursor-agent-allowed-tools` and `x-cursor-agent-exclude-tools`, behind the hidden flags `--allowed-tools` / `--exclude-tools`, described as "internal only". They take names from the ToolCall oneof. The server may gate them. [M]
- **Checksum:** `x-cursor-checksum` uses the well-known scheme: 6-byte timestamp `floor(now/1e6)`, a rolling XOR with seed 165, base64, then `machineId[/macMachineId]`.
  - In CLI 2026.10.19 it is computed **only in worker/bridge mode** (`691.index.js`). The normal interactive and headless agent requests do not send it. [H for the code path; M that the server does not require it]
  - oakimov's provider sends it anyway, which is cheap insurance.

---

## 3. Shape of the protocol: agent-shaped, not chat-completion-shaped [H]

`AgentClientMessage` oneof: `run_request`, `exec_client_message`, `exec_client_control_message`, `kv_client_message`, `conversation_action`, `interaction_response`, `client_heartbeat`, `prewarm_request`.

`AgentServerMessage` oneof: `interaction_update`, `exec_server_message`, `exec_server_control_message`, `conversation_checkpoint_update`, `kv_server_message`, `interaction_query`, `ttft_breakdown`.

### Flow
1. The client sends `run_request`, an `AgentRunRequest` with these fields:
   - `conversation_state`, a `ConversationStateStructure` of content-addressed blob IDs
   - `action`, a `ConversationAction` holding a `user_message_action`
   - `model_details` and `requested_model{model_id,max_mode,parameters[]}`
   - `mcp_tools`, `conversation_id`, `custom_system_prompt`
   - `system_prompt_spec{replace|append}`, `exclude_workspace_context`, `harness`
2. The server streams `interaction_update` messages:
   - `text_delta`, `thinking_delta` / `thinking_completed`, `token_delta`
   - `tool_call_started` / `partial_tool_call` / `tool_call_delta` / `tool_call_completed`
   - `turn_ended{input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens}`
   - `step_started` / `step_completed`, `heartbeat`, `summary*`
3. **The server drives tool execution on the client.** It sends `exec_server_message` with one of 40+ arg variants:
   - core tools: `shell_args`, `shell_stream_args`, `read_args`, `write_args`, `delete_args`, `grep_args`, `ls_args`, `diagnostics_args`, `fetch_args`, `subagent_args`, `git_diff_request`
   - context: `request_context_args`, which asks the client for its rules, env, repo info, tool list, MCP instructions and skills
   - MCP: `mcp_args{name, args map, tool_call_id, provider_identifier, tool_name}`
   - alternative harness tool sets: `pi_read/bash/edit/write/grep/find/ls_args`, `mini_swe_agent_bash_args`

   The client must reply on the same stream with `exec_client_message{id, exec_id, <variant>_result}`. For MCP the reply is `mcp_result{success{content[text|image], is_error}|error|rejected|...}`.
4. KV blob store: `kv_server_message{get_blob_args|set_blob_args}`. The server stores conversation-state blobs on the client and reads them back, so the client must keep a per-conversation blob store. Checkpoints (`conversation_checkpoint_update`) reference those blobs.
5. `interaction_query` covers web_search, ask_question, switch_mode, create_plan, web_fetch, mcp_auth, and so on. The client answers with `interaction_response`.

### Hooks that make a mapping to OpenAI or Claude messages feasible
- `UserMessageAction` has **`conversation_history`** and `trailing_conversation_history` fields of type `ConversationHistory`:
  - `ConversationHistory{messages[]}`, where each message is `user{content[text|image]}`, `assistant{content[text|reasoning{text,signature}|redacted_reasoning|tool_call{tool_call_id,tool_name,args_json}]}` or `tool{tool_call_id,tool_name,content[text|image],is_error}`.
  - This is close to the OpenAI and Claude message models. The CLI exposes it through the hidden flag `--conversation-history-file <json>`, which takes protobuf-JSON of `agent.v1.ConversationHistory`.
- Client-defined tools can be advertised as MCP tools, through `McpToolDefinition{name, provider_identifier, tool_name, description, input_schema, input_schema_json}` and/or `RequestContext.tools`, and arrive back as `mcp_args`. Per oakimov's notes, `mcp_tools` on `RunRequest` is "prewarm-only, empty on real turns". Real turns advertise tools through the `request_context` reply. [M]
- Cursor's built-in tools are part of the server-side harness and cannot simply be turned off:
  - The client can decline or soft-deny them.
  - It can try the internal `x-cursor-agent-allowed-tools` header.
  - It can set `web_search_enabled=false` / `web_fetch_enabled=false` in `RequestContext`.
  - The server's system prompt is Cursor's own. `custom_system_prompt` and `--system-prompt` are labelled "Anysphere/OpenAI team only" and are probably enforced server-side. [M]

### Consequences for OpenAI-style client-side tools
A tool call arrives mid-stream as an `exec` that must be answered on the same open `Run`. A stateless proxy has two options:
- **Hold the bidi stream open** across HTTP requests: end the OpenAI response with `finish_reason=tool_calls`, keep the Run alive with heartbeats, and resume it when the next request carries the `tool` message. oakimov does this, with leases of up to 4 h.
- **Cancel and start a new Run** that replays everything through `conversation_history`. This is cleaner but untested. It is also unclear how the server treats a history that ends in a tool result rather than a user message; `trailing_conversation_history` may cover that case. [L-M]

---

## 4. Models [H for the endpoints, M for the list]
- The CLI calls `agent.v1.AgentService/GetUsableModels` with request `{custom_model_ids[]}` and response `{models[] ModelDetails{model_id, display_model_id, display_name, display_name_short, aliases[], thinking_details, max_mode}}`. It also calls `GetDefaultModelForCli`.
- The IDE and oakimov use `aiserver.v1.AiService/AvailableModels`.
- Parameterized models take bracket overrides, for example `claude-opus-4-8[context=1m,effort=high,fast=false]`. Parameters are sent as `RequestedModel.parameters[]` with `{id,value}`, such as `thinking`, `context`, `effort`, `fast`.
- The IDE cache on this Mac (`availableDefaultModels2`) lists 46 models:
  - Meta: auto-smart
  - Cursor: composer-2.5
  - Anthropic: claude-opus-5-5, claude-opus-5, claude-opus-4-8, claude-opus-4-7, claude-opus-4-6, claude-opus-4-5, claude-fable-5-1, claude-fable-5, claude-sonnet-5-5, claude-sonnet-5, claude-sonnet-4-6, claude-sonnet-4-5, claude-sonnet-4, claude-haiku-5-5, claude-haiku-4-5
  - OpenAI: gpt-5.6-sol, gpt-5.6-terra, gpt-5.6-luna, gpt-5.5, gpt-5.4, gpt-5.4-mini, gpt-5.4-nano, gpt-5.3-codex, gpt-5.2, gpt-5.1, gpt-5-mini
  - Google: gemini-4-argon, gemini-3.8-flash, gemini-3.7-flash, gemini-3.6-flash, gemini-3.5-flash, gemini-3.1-pro, gemini-3-flash, gemini-2.5-flash
  - xAI: grok-4.7, grok-4.6, grok-4.5
  - Meta: muse-wonder-1.0, muse-spark-1.3
  - Others: kimi-k3, kimi-k2.7-code, glm-5.2, glm-5p3, glm-5p3-flash
- The CLI model history on this Mac: claude-opus-5-5, composer-2.5, default, claude-opus-4-8.
- `agent --list-models` and `agent models` print the live list. I did not run them because they make an authenticated call.

---

## 5. Open-source prior art

| Repo | Lang | Last push | Approach | Notes |
|---|---|---|---|---|
| [oakimov/cursor-opencode-provider](https://github.com/oakimov/cursor-opencode-provider) | TS | 2026-10-20 | **Native `agent.v1.AgentService/Run`** over HTTP/2 Connect; hand-written sparse protobuf decoder | The best reference. It covers PKCE login, API-key exchange and `/oauth/token` refresh; `x-cursor-checksum`, device ID, and a client version read from the local CLI install; KV blob store with gzip snapshots; holding the Run open across host tool calls; mapping Cursor exec read/shell/edit/grep to host tools; and resume via `ResumeAction`. `src/language-model.ts` alone is about 340 KB, which shows how much work this is. `docs/cli-parity.md` is a parity matrix against CLI `2026.08.25`. Includes an EU-interoperability DISCLAIMER: "Cursor may change or restrict the API without notice." |
| [Nomadcxx/opencode-cursor](https://github.com/Nomadcxx/opencode-cursor) | TS | 2026-10-16, 713 stars | Was a `cursor-agent` / ACP subprocess; 2.x uses the official **`@cursor/sdk`** behind a local OpenAI-compatible proxy | Falls back to `cursor-agent` when the key is rejected. |
| [M507/curapi](https://github.com/M507/curapi) | **Go** | 2026-09-19 | Subprocess `agent` with stream-json mapped to `/v1/chat/completions` and `/v1/responses` | Per-request `--workspace`/`--data-dir`, concurrency cap of 16, images passed via `--image`. A close Go analogue for option (b). |
| [code-yeongyu/cursor-proxy](https://github.com/code-yeongyu/cursor-proxy) | TS | 2026-07-25 | Subprocess `cursor agent`, stream-json to SSE | |
| `@cursor/sdk` ([docs](https://cursor.com/docs/api/sdk/typescript)) | TS, official | public beta since April 2026 | `Agent.create({local:{cwd}})`, then `agent.send()` returns a streamed run | Officially supported, but it runs Cursor's own agent loop and is Node-only. |
| wisdgod/cursor-api (Rust), zhx47/cursor-api (JS), 7836246/cursor2api (TS) | various | archived or stale | Legacy IDE `aiserver.v1` `StreamChat` / `StreamUnifiedChatWithTools`, or the cursor.com web docs chat | Old protocol with checksum games, and mostly archived. A cautionary example of breakage. |

### Risks
- ToS §1.5 forbids attempts to "reverse engineer, disassemble, decompile, decode"; renting, leasing, lending or selling the Service; knowingly letting third parties do so; and using output to train competing models. §9 lets Cursor suspend access at any time.
- Exposing one subscription as a shared API endpoint is the highest-risk use. The safest framing is a personal, single-user proxy.
- Breakage risk with the native protocol is high:
  - The CLI ships about weekly. Three versions arrived in 3 weeks here.
  - Cursor adds exec variants often (`pi_*`, `mini_swe_agent_*`, `conversation_search`, `agent_store_conflict`, ...). An unknown exec has to be soft-denied, or turns break.
  - `GetServerConfig` can switch hosts or HTTP versions.
  - The server may start requiring `x-cursor-checksum` or a newer client version.
- The subprocess approach inherits CLI auto-updates, which helps with protocol drift but adds the risk of behaviour changes.

---

## 6. Subprocess wrapper (`agent -p --output-format stream-json`)

### Relevant flags
From `agent --help` plus hidden options found in the bundle:
- `-p/--print` (has all tools, including write and shell), `--output-format text|json|stream-json`, `--stream-partial-output`
- `--model <id[params]>`, `--mode plan|ask`, `--resume [chatId]`, `--continue`, `create-chat`
- `-f/--force` / `--yolo`, `--sandbox enabled|disabled`, `--trust`, `--approve-mcps`, `--workspace`, `--add-dir`
- `--api-key` / `CURSOR_API_KEY`, `-H/--header`, `-e/--endpoint`
- Hidden: `--auth-token`, `--conversation-history-file <file>` (protobuf-JSON `ConversationHistory`, headless only), `--exclude-workspace-context`, `--single-turn`, `--allowed-tools` / `--exclude-tools` ("internal only"), `--system-prompt <file>` ("Anysphere/OpenAI team only"), `--harness`, `--background-shell-timeout`
- Also `agent acp`: an ACP JSON-RPC stdio server with `session/new|load|prompt|cancel|set_model|request_permission`, `fs/read_text_file`, `fs/write_text_file` and `terminal/*`. It is a long-lived subprocess alternative.

### stream-json NDJSON events
Source: `544.index.js` and [docs](https://cursor.com/docs/cli/reference/output-format).
```
{"type":"system","subtype":"init","apiKeySource":"login|env|flag","cwd":...,"session_id":...,"model":"<display name>","permissionMode":"default"}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":...}]},"session_id":...}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":<delta or full>}]},"session_id":...,["timestamp_ms"],["model_call_id"]}
{"type":"thinking","subtype":"delta","text":...,...} / {"type":"thinking","subtype":"completed",...}
{"type":"tool_call","subtype":"started"|"completed","call_id":...,"tool_call":{"shellToolCall"|"readToolCall"|"editToolCall"|"mcpToolCall"|...:{"args":{...},"result":{...}}},"model_call_id":...}
{"type":"interaction_query","subtype":"request"|"response","query_type":...,...}
{"type":"retry","subtype":"starting"|"resuming",...} / {"type":"connection",...reconnecting...}
{"type":"system","subtype":"task_notification"|"background_shell_timeout",...}
{"type":"result","subtype":"success","is_error":false,"duration_ms":...,"duration_api_ms":...,"result":"<full text>","session_id":...,"request_id":...,"usage":{inputTokens,outputTokens,cacheReadTokens,cacheWriteTokens}}
```

Delta handling with `--stream-partial-output`:
- An `assistant` event with `timestamp_ms` and no `model_call_id` is a real delta.
- An event with both fields is a duplicate flush before a tool call.
- An event with neither field is the final duplicate.

On failure, the process exits non-zero, writes to stderr, and emits no `result` event.

### Tradeoffs
Pros:
- No protocol work: auth, refresh, checksum, KV blobs, exec handling and server config are all the CLI's job.
- Auto-updates track Cursor's changes.
- Officially documented output.
- Usage and tokens are available in `result.usage`.
- Thinking deltas are available.
- Full history can be passed per request via `--conversation-history-file`, so the proxy can stay stateless.

Cons:
- **No client-side tools.** The model only sees Cursor's built-in tools, which run locally in the proxy host's workspace. OpenAI `tools` / `tool_calls` cannot be returned to the caller. A workaround is to expose the caller's tools as an MCP server that blocks, which effectively reimplements the hold-open problem.
- Cursor's agent system prompt and harness are always present; `--system-prompt` is not available to us.
- Per-request process spawn costs about 0.5–1.5 s of Node start-up plus workspace scanning. `--exclude-workspace-context`, an empty temp workspace and `--sandbox enabled` help.
- Requires Node and the CLI on the proxy host, so it does not fit a pure-Go, container-friendly setup.
- Concurrency means N processes.
- Behaviour of the hidden flags may change without notice.

---

## Recommendation

**Build (b), the subprocess wrapper, first.** Use it for plain chat: text and reasoning streaming, usage, and model selection. Run `agent -p --output-format stream-json --stream-partial-output --model <m> --exclude-workspace-context --sandbox enabled` in an empty per-request temp workspace, and pass prior turns through `--conversation-history-file`. Leave `--force` and `--trust` off by default, because `-p` gives the agent shell and write access on the proxy host. The cost is a few hundred lines of Go, there is no protocol risk, and the CLI's auto-updates absorb drift. The limitation is that it cannot serve OpenAI or Claude client-side tools, so Claude Code or Codex agents cannot run through it. Confidence: high that it works, and medium on the hidden flags `--conversation-history-file` and `--exclude-workspace-context` staying available.

**Build (a), the native `agent.v1.AgentService/Run` executor, only if client-side tools are a hard requirement.** It requires:
- Connect-RPC over HTTP/2 bidi in Go (`connectrpc.com/connect` plus a `.proto` rebuilt from `proto-descriptors.txt`).
- Auth: keychain/file import, PKCE poll, API-key exchange, and `/oauth/token` refresh.
- `GetServerConfig` for host discovery.
- A per-conversation KV blob store.
- Answers to `request_context`, and a soft-deny for every unknown exec.
- Mapping OpenAI tools to MCP tool definitions and `mcp_args` to `tool_calls`.
- Either holding the Run open across HTTP requests or a `conversation_history` replay.

oakimov/cursor-opencode-provider shows it works, but it is a large and fast-moving surface. Confidence: high that it is feasible, and medium-low on long-term maintenance and the ToS/ban risk.

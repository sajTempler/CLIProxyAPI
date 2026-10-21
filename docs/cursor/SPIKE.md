# Cursor provider: Phase 0 spike results

Date: 2026-10-21. Verdict: **GO, with a changed Phase 2 design** (see the end of this doc).

## Setup

- Agent CLI: `2026.10.19-6bbf2c4`. Sent as `x-cursor-client-version: cli-2026.10.19-6bbf2c4`, read from the `~/.local/bin/agent` symlink.
- Host: `https://agentn.global.api5.cursor.sh`, `agent.v1.AgentService/Run`, Connect bidi over HTTP/2, `application/connect+proto`. Request frames were not compressed. Headers: `authorization`, `x-cursor-client-type: cli`, `x-cursor-client-version`, `x-ghost-mode: true`, `x-cursor-streaming: true`, `x-request-id`.
- Client: throwaway TS under `temp/cursor-spike/` (gitignored), run with bun 1.3.13. It reuses the protobuf schema (`messages.ts`) and the `struct.ts` Value encoder from oakimov/cursor-opencode-provider @ `710e173`. I added `ConversationHistory`, `UserMessageAction` #7/#8, `AgentRunRequest` #29 `system_prompt_spec` and `RequestContext` #53 `system_prompt_override` from `proto-descriptors.txt`.
- Tools were advertised the way the reference client does it: `RequestContext.mcp_meta_tool_options.mcp_descriptors` (server `claude-code`), and `mcp_state_exec_args` (exec #36) answered with the full `McpToolDefinition`s. `AgentRunRequest.mcp_tools` stayed empty.
- Every built-in exec was soft-denied (typed `rejected`/`error` result). Nothing Cursor sent was executed. MCP calls got canned results.
- Budget: **28 Runs** of the 40 allowed, run one at a time. Models: `composer-2.5`, `claude-haiku-4-5`, `gpt-5.4-mini`.
- The token was read from the keychain at runtime and never printed or written to disk.

## Q1: Does replay through `conversation_history` work?

**Verdict: no.** The server silently ignores `UserMessageAction.conversation_history` (#7) and `trailing_conversation_history` (#8). Two workarounds do work (below).

Evidence:

| Run | Setup | Result |
|---|---|---|
| history-probe, composer-2.5 | history `[user: "my favourite colour is teal", assistant: "Noted"]`, then ask for the colour | `"UNKNOWN"` |
| history-probe, claude-haiku-4-5 | same | `"UNKNOWN"` |
| replay-trailing, composer-2.5 | user = original prompt; trailing = `[tool_call lookup_secret{alpha}, tool_result "alias: look up beta"]` | calls `lookup_secret{"key":"alpha"}` again, so it never saw the result |
| replay-history, composer-2.5 | history = `[user, tool_call, tool_result]`, empty user text | `"Your last message was empty. What would you like help with..."` |

I checked the bytes: `UMA.7 → ConversationHistory.1 → Message.1 user → content.1 text → text.1`, which matches the descriptors and the CLI's own `--conversation-history-file` path. Input tokens did not grow with the history (8148 vs 8137 for a bare ping).

Workarounds that work:

1. **Transcript in the user text** (the reference client's seeding approach). Prior turns go into `<conversation_history>` with tool calls and results as text, followed by "Continue the task above".
   - Mid-turn, composer and haiku both called `lookup_secret{"key":"beta"}` next (the correct step), at about 1.6 s and 2.8 s.
   - With both results present, both models answered `PURPLE-42` without calling the tool again.
   - 4/4 correct. The cost is that the history is lossy: tool calls become prose, and there is no structured thinking or signatures.
2. **Checkpoint + `ResumeAction`.**
   - Run A was stopped at the first `mcp_args` with `cancel_action`. It produced 4 `conversation_checkpoint_update`s and 18 KV blobs.
   - Run B used the same `conversation_id`, `conversation_state` set to the last checkpoint, `resume_action{}` and the same blob store. The server fetched 14 blobs (all found), sent `request_context_args`, then **generated the tool call again** (a new `tool_call_id`, same args), and continued correctly to `beta` and then `PURPLE-42`.
   - Prompt cache worked well (input 26112, cache_read 21184).
   - The catch: the stopped call is regenerated, not re-delivered, so its args could in principle differ.
3. **Held-open stream.** Answering `mcp_args` with `mcp_result` on the same Run worked in all 9 Q2 trials and in resume-B. The model kept going and produced the final answer in the same Run.

## Q2: With built-ins soft-denied, do models use the offered MCP tools?

**Verdict: yes.**

The prompt asked for the first line of a file, with the tools `Read`, `Bash` and `lookup_secret`. This deliberately overlaps Cursor's built-in read. The "guidance" in the table is a global rule: "Cursor built-in tools are disabled ... the only working tools are the MCP tools of the `claude-code` server".

| Model | Guidance rule | Trials | Used MCP `Read` | Built-in attempts | Correct final |
|---|---|---|---|---|---|
| composer-2.5 | yes | 3 | 3/3 | 0 | 3/3 |
| claude-haiku-4-5 | yes | 3 | 3/3 | 0 | 3/3 |
| claude-haiku-4-5 | yes, full descriptors | 2 | 2/2 | 0 | 2/2 |
| gpt-5.4-mini | yes | 1 | 1/1 | 0 | 1/1 |
| composer-2.5 | no | 1 | 1/1 | 1 `read_args`, soft-denied, then fell back to MCP | 1/1 |
| claude-haiku-4-5 | no | 1 | 1/1 | 1 `read_args`, soft-denied, then fell back to MCP | 1/1 |

**Exec types seen across all 28 Runs:**
- `mcp_args`: 18
- `mcp_state_exec_args`: 12
- `read_args`: 2 (no-guidance runs only)
- `request_context_args`: 1 (only on the resume Run; fresh Runs used the inline RequestContext)

No shell, write, delete, grep or ls requests arrived, and no `interaction_query`.

How MCP calls behave:
- They go through Cursor's dynamic-tool flow. The `tool_call_started` sequence is `get_mcp_tools_tool_call`, then exec `mcp_state_exec_args`, then `mcp_tool_call` with exec `mcp_args`.
- On arrival, `mcp_args.name` is `claude-code-Read`, `tool_name` is `Read`, `provider_identifier` is `claude-code`, and `args` is a `google.protobuf.Value` map.
- The discovery step costs one extra model step. The first `mcp_args` arrived after:
  - composer: 2.3–5.4 s
  - haiku: 4.5–6.4 s
  - when a transcript already named the tool: 1.6 s

Argument quality:
- Haiku skipped discovery twice and guessed `{"path": ...}` instead of `file_path` (once with guidance, once without).
- With full descriptors in `mcp_meta_tool_options` (description and `input_schema`), args were correct 2/2. Discovery still happened, so full descriptors did not remove the extra step.

## Q3: Is `x-cursor-checksum` required on Run?

**Verdict: no.** All 28 Runs sent no `x-cursor-checksum` and returned HTTP 200 with normal streams. This matches the CLI, which sends the header only in worker/bridge mode.

## Q4: Can a caller system prompt dominate Cursor's?

**Verdict: yes, but only through a global rule.**

The test system prompt was: "You are Claude Code ... NOT Cursor ... begin every reply with 'ZQX> ', all lowercase, no markdown". The question asked was "Who are you, and which product are you running in?"

| Channel | composer-2.5 | claude-haiku-4-5 |
|---|---|---|
| `RequestContext.rules[{type: global}]` | `"ZQX> i am claude code, anthropic's official cli for claude. ..."`: full adherence | `"ZQX> i'm claude code, anthropic's official cli for claude. ..."`: full adherence |
| `AgentRunRequest.system_prompt_spec.replace` (#29) | `"I'm **Composer**, a language model trained by Cursor..."`: silently ignored | not run |
| `RequestContext.system_prompt_override.replace` (#53) | `"I'm **Composer** ... running inside **Cursor**"`: silently ignored | not run |
| `AgentRunRequest.custom_system_prompt` (#8) | rejected: `invalid_argument unknown option '--system-prompt'` | not run |

Caveat: the rule sits **on top of** Cursor's harness prompt and does not replace it.
- A bare one-word ping costs about 8.1k input tokens on composer and 7.8k on haiku before any user content.
- A Claude Code-sized system prompt (15–20k tokens) in a rule was not tested.

## Recommendation: GO, with Phase 2 changes

Q2 passes and the transport is straightforward: no checksum, the KV blob store works, and soft-deny works. Q1's planned mechanism fails, though, so Phase 2 cannot be "stateless replay through `conversation_history`". Changes to PLAN.md Phase 2:

1. **Stateful sessions are mandatory.** Use a per-conversation store holding the last checkpoint bytes and the KV blobs.
   - **Tool round-trip: hold the Run open.** End the Claude response with `stop_reason: tool_use`, keep the Run alive with heartbeats, and answer `mcp_result` when the next request brings the `tool_result` for that `tool_use_id`. This is proven here and the most faithful option. It needs a registry of pending exec ids keyed by `tool_use_id`, and must cope with several pending calls and with long Claude Code tools (heartbeats, idle limits).
   - **New user turn:** open a new Run with the stored checkpoint plus `user_message_action`, which is the CLI's normal path.
   - **Lost Run (proxy restart, stream drop):** use checkpoint plus `resume_action`, then match the regenerated `mcp_args` by tool name and args against the cached `tool_result`.
   - **No state (first request of an existing Claude Code conversation, or history from another provider):** seed a transcript in the user text (lossy, but continuation was correct 4/4).
2. **System prompt** goes in as one `global` rule in `RequestContext.rules`. Do not use `system_prompt_spec`, `system_prompt_override` or `custom_system_prompt`. The same rule should also say that built-ins are unavailable and that the `claude-code` MCP tools are the only tools (this removed every built-in attempt).
3. **Tools:**
   - Advertise through `mcp_meta_tool_options` with full descriptors, and answer `mcp_state_exec_args` with full definitions.
   - Map `mcp_args.tool_name` to `tool_use.name`.
   - Hide `get_mcp_tools` / `mcp_tool_call` `tool_call_started` updates from Claude Code.
   - Expect the occasional wrong argument name. Pass it through so Claude Code's validation error comes back as a `tool_result`.
4. **Soft-deny every built-in exec** with typed `rejected`/`error` results, `stream_close` for `shell_stream`, and logging of the type. It stays as the fallback, since without guidance models try `read_args` first.
5. **Omit `x-cursor-checksum`.** If the canary ever starts failing, add it back as an option.
6. **Budget:** about 8k of Cursor harness tokens per Run, plus one extra discovery step per first MCP use in a Run. Report usage from `turn_ended`, which looks cumulative across the steps of a Run.
7. **Phase 1 canary** should assert this exact flow: rule → `get_mcp_tools` → `mcp_args` → `mcp_result` → final text.

Untested (Phase 2 risks): parallel tool calls in one step, holding a Run for minutes while a tool runs, a 15–20k-token rule, thinking/signature handling, images, larger models (Sonnet/Opus/GPT-5.5), and the HTTP/1.1 fallback.

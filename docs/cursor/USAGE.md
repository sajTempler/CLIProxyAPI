# Using Cursor models from Claude Code

The local proxy (`127.0.0.1:8317`) serves Cursor-account models next to the
Anthropic ones. Claude Code talks to it through the `claudep` wrapper
(`~/.local/bin/claudep`: sets `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`
from `.env`). Plain `claude` stays direct and never sees Cursor models.

## Quick start

```bash
claudep --model cursor/composer-2.5            # interactive
claudep -p --model cursor/composer-2.5 "hi"    # one-shot
```

Inside a `claudep` session, `/model cursor/<id>` switches to a Cursor model and
`/model claude-opus-5-5` (or any Anthropic id) switches back. See "Mixed sessions" for switching in the
middle of a conversation.

Claude Code prints `Model "cursor/..." isn't recognized · context assumed to be
200k tokens` at startup. That is expected; see "Context window".

## Picking a model

Every Cursor model is `cursor/<cursor model id>`. List them:

```bash
k=$(sed -n 's/^CPA_API_KEY=//p' ~/private/CLIProxyAPI/.env)
curl -s -H "Authorization: Bearer $k" http://127.0.0.1:8317/v1/models \
  | python3 -c 'import json,sys; print("\n".join(m["id"] for m in json.load(sys.stdin)["data"] if m["id"].startswith("cursor/")))'
```

Send no `anthropic-version` header when listing: for clients that send it
(Claude Code does), the proxy cloaks non-Claude ids as
`claude-fable-5-dd-<id reversed>`. `/v1/messages` accepts both spellings.

The list is the account's live `GetUsableModels` catalog (about 265 ids on the
Team plan). The proxy fetches it when a Cursor login registers and
re-fetches it every ~6 h on a timer (failures back off from 10 min to 1 h). Until the first fetch succeeds the
proxy serves a 9-model static fallback. Ids encode the reasoning level
(`-low`, `-medium`, `-high`, `-xhigh`, `-max`), a `-thinking` variant and a
`-fast` (priority) variant. Cursor picks the reasoning level from the id;
Claude Code's effort/thinking settings do not change it. Ids the catalog marks
`max_mode` (the 33 Opus `*-fast` variants on the Team plan) are requested in
Cursor's max mode, as the agent CLI does. On this account they answer
`resource_exhausted` (HTTP 429) with or without max mode, so treat them as
unavailable.

Practical picks:
- `cursor/composer-2.5`: Cursor's own model, cheap and fast, good tool use. Default for tests.
- `cursor/gpt-5.4-mini-medium`, `cursor/gemini-3-flash`: other cheap options.
- `cursor/claude-*`, `cursor/gpt-5.5-*`: frontier models billed to the Cursor plan instead of the Anthropic subscription.
- `cursor/default`: Cursor's "Auto" routing.

## What works

- Text, streaming, and Claude Code's own tools (Read, Edit, Bash, Grep, MCP
  tools, ...). They are offered to Cursor as MCP tools of a `claude-code`
  server; Cursor's built-in tools are disabled (every built-in exec is
  answered "not available", and the model falls back to the offered tools).
- The system prompt: Claude Code's system prompt rides as one global Cursor
  rule, which dominates Cursor's own harness prompt.
- Thinking: when the request enables thinking, Cursor's thinking stream
  becomes `thinking` blocks (without signatures).
- Usage: input, output and cache token counts come from Cursor's `turn_ended`.
- Subagents and side requests (titles, summaries): they get their own state
  (keyed by session id plus the first message), so they never disturb the
  main conversation.

## Known limits

- One `tool_use` per response. Cursor dispatches the calls of one model step
  one at a time, so parallel tool calls arrive as consecutive turns. Correct,
  just slower.
- Conversation state lives in proxy memory. Cursor ignores client-sent history,
  so the proxy keeps the last checkpoint per conversation (1 h idle TTL, 128
  conversations). After a proxy restart (every deploy) or expiry, the next
  request seeds the whole transcript as text: correct but lossy (tool calls and
  results become text) and it costs a full re-read of the history.
- A tool that is running while the proxy restarts: the held Run is lost; the
  next request resumes from the checkpoint and answers the regenerated call
  from the cached result.
- Anthropic server tools (web search, code execution) do not exist on Cursor.
  Claude Code's WebSearch/WebFetch run their own model call on the session
  model; on a Cursor model expect them not to work.
- Images: forwarded, with limits (see "Images"). PDFs are not.
- `cache_control` markers are ignored; Cursor caches on its own and reports
  cache reads/writes.
- Prompt and output are billed against the Cursor plan (Team: included usage,
  then on-demand). The proxy never counts or limits it.
- Personal use only: the protocol is private (Cursor ToS §1.5/§9). Never expose
  the proxy beyond `127.0.0.1`.

## Context window

Claude Code takes a model's context window from its built-in catalog only.
Through a gateway it does not read `/v1/models` metadata (checked in Claude
Code 2.1.306: its served catalog is first-party only, the published one is
signed, and gateway model discovery reads only ids and labels), so the proxy
cannot announce windows. For an unrecognized id such as `cursor/*` it assumes
200k and auto-compacts there, which matches most Cursor models.

For the 1M variants (Cursor display names with "1M"; the proxy reports them
as `max_input_tokens: 1000000` in `/v1/models`), tell Claude Code on the
client side:

```bash
claudep --model 'cursor/claude-opus-5-5-high[1m]'                  # 1M window
CLAUDE_CODE_MAX_CONTEXT_TOKENS=400000 claudep --model cursor/<id>  # any other size
```

Claude Code strips `[1m]` before sending (verified: the proxy receives
`cursor/<id>`). `CLAUDE_CODE_MAX_CONTEXT_TOKENS` applies to unrecognized ids
that don't start with `claude-`, which covers every `cursor/*` id; Claude
models in the same session keep their own windows.

Which ids are 1M (decodes the cloaked ids Claude Code-style clients get):

```bash
curl -s -H "Authorization: Bearer $k" -H 'anthropic-version: 2023-06-01' 'http://127.0.0.1:8317/v1/models?limit=1000' \
  | python3 -c 'import json,sys; [print(m["id"][len("claude-fable-5-dd-"):][::-1] if m["id"].startswith("claude-fable-5-dd-") else m["id"]) for m in json.load(sys.stdin)["data"] if m.get("owned_by")=="cursor" and m.get("max_input_tokens")==1000000]'
```

The window only decides when Claude Code compacts. A compacted conversation
has a new first message, so the proxy starts a new Cursor conversation from
the summary.

## Images

Images are forwarded (png, jpeg, gif, webp, base64 as Claude Code sends them):
- pasted images (user message image blocks) go inline as the Run's selected
  images, as the agent CLI sends them;
- images in tool results (Claude Code's Read on an image file) go back as MCP
  image content.

Verified live: composer-2.5 and gpt-5.4-mini-medium named the color of a
pasted swatch, and gpt-5.4-mini-medium the color of one returned by a tool.
Image URLs and other image types are answered with HTTP 400. After a proxy
restart the transcript fallback keeps only the last message's images; earlier
ones become `[image]`. PDF/document blocks cannot be forwarded: the model sees
a note saying a document was attached and is not available.

## Mixed sessions

A `claudep` session can switch between `claude-*` and `cursor/*` models
(`/model`, or `--resume` with another `--model`).
- Claude to Cursor: the Cursor conversation starts from the transcript
  (Claude's thinking blocks are not forwarded).
- Cursor to Claude: Cursor's thinking blocks are unsigned (Claude Code stores
  `signature: ""`), and Anthropic would reject them, so the claude provider
  drops them before the request leaves the proxy. Text and tool calls stay.

Verified live: one `claudep` session went composer-2.5 → claude-haiku-5-5 →
composer-2.5 via `--resume <id> --model ...` (51, 52, 53), with Cursor
thinking blocks in the history and no upstream error.

## Quota

- Management panel: `http://127.0.0.1:8317/management.html`, quota view
  (Cursor next to Claude).
- API: `GET /v8/management/quota/cursor` (header `X-Management-Key:
  $MANAGEMENT_PASSWORD`), `POST /v8/management/quota/cursor/refresh` to poll
  now. The poller runs every 15 min (`routing.cursor-quota.refresh-interval`).
  Field contract: `USAGE-TRACKING.md` section 5.

## Login expiry and re-login

The proxy's Cursor login is `~/.cli-proxy-api/cursor-<email>-<hash>.json`.
Its `expired` field is the access token's JWT expiry (about 60 days after
login). A login imported from the agent CLI or a browser login cannot be
refreshed; an API-key login refreshes itself.

Check the expiry:

```bash
python3 -c 'import json,glob; [print(f, json.load(open(f)).get("expired")) for f in glob.glob("/Users/szymon/.cli-proxy-api/cursor-*.json")]'
```

The canary (`bin/cursor_canary`, `MAINTENANCE.md`) exits 2 with `Cursor login
expires in N days` within 7 days of expiry. Once expired, Cursor requests fail
with 401.

Re-login (from the repo root; each command writes the auth file and exits, and
the running proxy picks it up without a restart):

```bash
agent login && bin/cli-proxy-api-live --config config.yaml --cursor-import-agent   # reuse the agent CLI login
bin/cli-proxy-api-live --config config.yaml --cursor-login                          # browser login
CURSOR_API_KEY=... bin/cli-proxy-api-live --config config.yaml --cursor-api-key     # refreshable
```

## When something breaks

- `scripts/smoke-live.sh`: is the proxy up, are both providers answering?
- `bin/cursor_canary`: a full Cursor tool round-trip (exit codes in `MAINTENANCE.md`).
- `logs/launchd.err.log` / `logs/launchd.out.log`: proxy logs. `cursor: soft-denied built-in exec` warnings are normal.
- After an `agent` CLI update: `go run ./cmd/cursor_proto_sync --check`.

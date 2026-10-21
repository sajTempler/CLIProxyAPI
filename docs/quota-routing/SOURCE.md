# Source: "CLI Proxy fork: changes to replicate"

Snapshot of the user's doc (https://claude.ai/artifact/CNBiRjX6oPx5KMQjvG1jQ5, rev 21, 2026-10-21).
The doc is the requirement; this file is the agents' copy. Origin: Theo's token-maxing
video (https://www.youtube.com/watch?v=D8PikZ1KhUo&t=761s, 12:41–26:40). He shows no code.

## Change 1: route to the account that resets soonest
- Order accounts by weekly reset time, soonest first.
- Skip an account at its 5-hour limit; fall through to the next.
- Track only the 5-hour and 7-day windows. On Claude the weekly window that matters is the Fable one; ignore the separate Opus bucket.
- Needs: per-account usage + reset source (Claude Code's `/usage` reads an OAuth usage endpoint; unverified), a new `routing.strategy` value (e.g. `soonest-reset`, base on upstream `fill-first`), a usage refresh interval, and a cooldown on 429.
- Must respect session affinity: the ordering picks the account for a NEW session only; existing sessions stay until their account fails.

## Change 2: session/account affinity
Config only (done 2026-10-21 in the live `config.yaml`): `routing.session-affinity: true`, ttl `1h`, subagents `true`, rebind-on `quota`.
Check: cache-read vs cache-write tokens per session in logs; high writes mid-thread = sessions moving.

## Change 3: dashboard with quota per account ("Quota Management" page)
| Area | What it shows |
|---|---|
| Header | credential count / loaded, "Show emails" toggle (masked by default), Refresh |
| Provider tabs | one per provider with credential count; only Claude needed |
| Summary card | remaining weekly quota summed across accounts (e.g. 409% of 500% across 5), a bar with one segment per account, next reset ("in 1 day · 09/12, 23:00") |
| Row per credential | masked auth-file name, plan (Max), three meters: 7-day Fable, 5-hour, 7-day overall, each % remaining + reset time or "No reset pending" |
| Row action | "Refresh quota" re-reads usage for that account |

Percentages are REMAINING (100% = untouched). One usage poller feeds both Change 1 and Change 3.
Check first whether the upstream Management Center panel or a third-party dashboard (CPA-Manager-Plus, Oh-My-CPA) already has a quota view to extend.

## Out of scope / constraints
- Codex WebSocket transport (we don't use Codex).
- Tailscale multi-machine deployment: user chose to keep fleet machines off the proxy for now.
- ToS: keep Claude subscription accounts used by Claude Code only.

## Open questions (from the doc)
- Which endpoints return per-account usage and reset time for Claude, and how often can we poll?
- Fork, or contribute `soonest-reset` back upstream?
- Does an existing dashboard already have the quota view?

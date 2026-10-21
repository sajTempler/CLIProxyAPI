#!/usr/bin/env bash
# Weekly upstream watch for the proxy and panel forks. Reports only: it never merges,
# checks out, or changes a branch. The only write to either repo is `git fetch upstream --tags`.
#
#   scripts/upstream-watch.sh            fetch, write the report, notify if needed
#   NO_NOTIFY=1 scripts/upstream-watch.sh   same, without the macOS notification
#
# Output (logs/ is gitignored):
#   logs/upstream-watch/<YYYY-MM-DD>.md   full report
#   logs/upstream-watch.log               one summary line per run
# Notifies only when the proxy has Claude-critical upstream commits, or either repo is
# more than BEHIND_THRESHOLD commits behind. See docs/UPSTREAM.md.
# Written for macOS /bin/bash 3.2 (launchd's PATH): no mapfile, no associative arrays.
set -uo pipefail

# Proxy paths that decide whether Claude requests keep working (git pathspec globs).
# Keep in sync with docs/UPSTREAM.md.
CLAUDE_CRITICAL=(
  # Claude executor: request building, cloaking, signing, auth, streaming
  'internal/runtime/executor/claude_*.go'
  'internal/runtime/executor/helps/claude_*'
  'internal/runtime/executor/helps/cloak_*.go'
  'internal/runtime/executor/helps/utls_client.go'
  # Claude Code version, User-Agent and fingerprint headers
  'internal/config/claude_fingerprint_profile.go'
  'internal/misc/header_utils.go'
  'internal/misc/claude_code_instructions.*'
  # Claude OAuth login, token storage and refresh
  'internal/auth/claude/**'
  'internal/cmd/anthropic_login.go'
  'sdk/auth/claude.go'
  'sdk/auth/refresh_registry.go'
  # sdk/cliproxy auth refresh loop
  'sdk/cliproxy/auth/auto_refresh_loop.go'
  'sdk/cliproxy/auth/conductor_refresh.go'
  # Thinking-signature validation and cache used on Claude requests
  'internal/signature/claude*.go'
  'internal/cache/signature_cache.go'
  # /v1/messages handler and the Claude translators (claude -> X and X -> claude)
  'sdk/api/handlers/claude/**'
  'internal/translator/claude/**'
  'internal/translator/*/claude/**'
)

PROXY_REPO="${PROXY_REPO:-/Users/szymon/private/CLIProxyAPI}"
PANEL_REPO="${PANEL_REPO:-/Users/szymon/private/Cli-Proxy-API-Management-Center}"
BEHIND_THRESHOLD="${BEHIND_THRESHOLD:-150}"
LIST_CAP="${LIST_CAP:-80}" # max commits per list in the report

export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"
export GIT_TERMINAL_PROMPT=0

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
day="$(date '+%Y-%m-%d')"
report_dir="$repo/logs/upstream-watch"
report="$report_dir/$day.md"
log="$repo/logs/upstream-watch.log"
mkdir -p "$report_dir"

ts() { date '+%Y-%m-%dT%H:%M:%S%z'; }
notify() {
  # $1 title, $2 message. Quotes are stripped so they cannot break the AppleScript string.
  local title="${1//\"/}" msg="${2//\"/}"
  [ "${NO_NOTIFY:-}" = "1" ] && return 0
  /usr/bin/osascript -e "display notification \"${msg:0:200}\" with title \"${title}\" sound name \"Glass\"" >/dev/null 2>&1 || true
}

# Report body is accumulated here, then written in one go.
body="$(mktemp -t upstream-watch)"
trap 'rm -f "$body"' EXIT
out() { printf '%s\n' "$*" >>"$body"; }

# capped_list <git log args...>: markdown list of commits, at most LIST_CAP entries.
capped_list() {
  local lines n
  lines="$(git log --no-merges --date=short --format='- `%h` %ad %s (%an)' "$@")"
  if [ -z "$lines" ]; then
    out "_none_"
    return
  fi
  n="$(printf '%s\n' "$lines" | wc -l | tr -d ' ')"
  printf '%s\n' "$lines" | head -n "$LIST_CAP" >>"$body"
  [ "$n" -gt "$LIST_CAP" ] && out "- ... $((n - LIST_CAP)) more (LIST_CAP=$LIST_CAP)"
}

count_commits() { # count_commits <range> [-- paths...]
  git log --no-merges --format=%h "$@" | grep -c . || true
}

fetch() { # fetch <repo dir>; returns non-zero on failure, message on stdout
  local msg
  if ! msg="$(git -C "$1" fetch --quiet upstream --tags 2>&1)"; then
    printf '%s' "$msg" | tr '\n' ' ' | cut -c1-300
    return 1
  fi
}

# conflict_preview <ours> <theirs>: upstream commits that touch files our fork changed
# since the merge base. Sets conflict_files and conflict_commits.
conflict_preview() {
  local ours="$1" theirs="$2" base fork_files up_files overlap
  base="$(git merge-base "$ours" "$theirs")" || { out "_no merge base_"; conflict_files=0; conflict_commits=0; return; }
  fork_files="$(git diff --name-only "$base..$ours" | sort -u)"
  up_files="$(git diff --name-only "$base..$theirs" | sort -u)"
  overlap="$(comm -12 <(printf '%s\n' "$fork_files") <(printf '%s\n' "$up_files") | grep . || true)"
  if [ -z "$overlap" ]; then
    conflict_files=0
    conflict_commits=0
    out "No file is changed on both sides since the merge base \`$(git rev-parse --short "$base")\`."
    return
  fi
  conflict_files="$(printf '%s\n' "$overlap" | wc -l | tr -d ' ')"
  # Feed the overlap as pathspecs; literal so file names with glob characters stay exact.
  local paths=()
  while IFS= read -r f; do paths+=(":(literal)$f"); done <<<"$overlap"
  conflict_commits="$(count_commits "$ours..$theirs" -- "${paths[@]}")"
  out "Merge base \`$(git rev-parse --short "$base")\`. Files changed on both sides: **$conflict_files**; upstream commits touching them: **$conflict_commits**."
  out ""
  out "<details><summary>Overlapping files ($conflict_files)</summary>"
  out ""
  printf '%s\n' "$overlap" | sed 's/^/- `/; s/$/`/' >>"$body"
  out ""
  out "</details>"
  out ""
  capped_list "$ours..$theirs" -- "${paths[@]}"
}

notify_reasons=()

# ---------- proxy ----------
cd "$PROXY_REPO" || exit 1
out "## Proxy (\`$PROXY_REPO\`): main vs upstream/main"
out ""
if ! ferr="$(fetch "$PROXY_REPO")"; then
  out "> **fetch failed**, numbers use the last fetched refs: $ferr"
  out ""
  proxy_fetch="FAILED"
else
  proxy_fetch="ok"
fi
p_behind="$(git rev-list --count main..upstream/main)"
p_ahead="$(git rev-list --count upstream/main..main)"
out "- Upstream head: \`$(git rev-parse --short upstream/main)\` $(git log -1 --date=short --format='%ad %s' upstream/main)"
out "- Behind: **$p_behind** commits (\`git rev-list --count main..upstream/main\`)"
out "- Ahead: **$p_ahead** commits"
out ""

crit_paths=()
for g in "${CLAUDE_CRITICAL[@]}"; do crit_paths+=(":(glob)$g"); done
p_crit="$(count_commits main..upstream/main -- "${crit_paths[@]}")"
out "### Claude-critical upstream commits ($p_crit)"
out ""
out "Commits in upstream/main, not in main, touching the CLAUDE_CRITICAL globs (top of \`scripts/upstream-watch.sh\`). Matching files are listed under each commit."
out ""
if [ "$p_crit" -eq 0 ]; then
  out "_none_"
else
  git log --no-merges --date=short --name-only --format='%n- `%h` %ad %s (%an)' \
    main..upstream/main -- "${crit_paths[@]}" |
    awk 'NF==0{next} /^- `/{print; next} {print "  - `" $0 "`"}' | head -n 400 >>"$body"
fi
out ""
out "### Conflict preview"
out ""
conflict_files=0
conflict_commits=0
conflict_preview main upstream/main
p_conflict_files="$conflict_files"
p_conflict_commits="$conflict_commits"
out ""
[ "$p_crit" -gt 0 ] && notify_reasons+=("proxy: $p_crit Claude-critical")
[ "$p_behind" -gt "$BEHIND_THRESHOLD" ] && notify_reasons+=("proxy: $p_behind behind")

# ---------- panel ----------
cd "$PANEL_REPO" || exit 1
if ! ferr="$(fetch "$PANEL_REPO")"; then
  panel_fetch="FAILED"
else
  panel_fetch="ok"
fi
latest_tag="$(git tag -l 'v[0-9]*' --sort=-v:refname | head -n 1)"
cur_tag="$(git describe --tags --abbrev=0 --match 'v[0-9]*' main 2>/dev/null || echo none)"
out "## Panel (\`$PANEL_REPO\`): main vs latest release tag"
out ""
[ "$panel_fetch" = "FAILED" ] && { out "> **fetch failed**, numbers use the last fetched refs: $ferr"; out ""; }
if [ -z "$latest_tag" ]; then
  out "No \`v*\` tags found."
  n_behind=0
  n_ahead=0
  n_conflict_files=0
  n_conflict_commits=0
else
  n_behind="$(git rev-list --count "main..$latest_tag")"
  n_ahead="$(git rev-list --count "$latest_tag..main")"
  out "- Latest release tag: \`$latest_tag\` ($(git log -1 --date=short --format=%ad "$latest_tag")); main is based on \`$cur_tag\`"
  out "- Behind: **$n_behind** commits (\`git rev-list --count main..$latest_tag\`)"
  out "- Ahead: **$n_ahead** commits"
  out "- Claude-critical: n/a (the panel is not on the Claude request path)"
  out ""
  out "### Commits in $latest_tag not in main"
  out ""
  capped_list "main..$latest_tag"
  out ""
  out "### Conflict preview"
  out ""
  conflict_files=0
  conflict_commits=0
  conflict_preview main "$latest_tag"
  n_conflict_files="$conflict_files"
  n_conflict_commits="$conflict_commits"
  out ""
  [ "$n_behind" -gt "$BEHIND_THRESHOLD" ] && notify_reasons+=("panel: $n_behind behind")
fi

# ---------- write ----------
summary="proxy behind $p_behind ahead $p_ahead critical $p_crit conflicts ${p_conflict_commits}c/${p_conflict_files}f fetch $proxy_fetch | panel ${cur_tag}->${latest_tag:-none} behind $n_behind ahead $n_ahead conflicts ${n_conflict_commits}c/${n_conflict_files}f fetch $panel_fetch"
{
  printf '# Upstream watch %s\n\n' "$day"
  printf 'Generated %s by `scripts/upstream-watch.sh`. Read-only: nothing was merged. How to read this and how to merge: `docs/UPSTREAM.md`.\n\n' "$(ts)"
  printf '**Summary:** %s\n\n' "$summary"
  if [ "${#notify_reasons[@]}" -gt 0 ]; then
    printf '**Attention:** %s\n\n' "$(IFS=';'; echo "${notify_reasons[*]}")"
  fi
  cat "$body"
} >"$report"

if [ "${#notify_reasons[@]}" -gt 0 ]; then
  reasons="$(IFS=';'; echo "${notify_reasons[*]}")"
  notify "Upstream watch" "${reasons//;/, }. Report: logs/upstream-watch/$day.md"
  notified="notify"
  [ "${NO_NOTIFY:-}" = "1" ] && notified="notify(suppressed)"
else
  notified="log-only"
fi
printf '%s %s [%s] %s\n' "$(ts)" "$summary" "$notified" "logs/upstream-watch/$day.md" >>"$log"
printf '%s\n' "$summary"
exit 0

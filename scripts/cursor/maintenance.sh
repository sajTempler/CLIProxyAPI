#!/usr/bin/env bash
# Cursor provider maintenance runner, invoked by the launchd jobs in this directory.
#
#   maintenance.sh watch   agent CLI changed: cursor_proto_sync --check, then the canary
#   maintenance.sh daily   canary only
#
# Every run appends to logs/cursor-maintenance.log. Failures also raise a macOS
# notification. Canary exit 3 (cursor provider not registered) is logged only.
# Never logs secrets: the canary reads CPA_API_KEY from .env itself.
set -uo pipefail

mode="${1:-daily}"
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
log="$repo/logs/cursor-maintenance.log"
state="$repo/logs/.cursor-maintenance-last-version"
bin="$repo/bin"
agent="${CURSOR_AGENT_PATH:-$HOME/.local/bin/agent}"
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

mkdir -p "$repo/logs" "$bin"
cd "$repo" || exit 1

ts() { date '+%Y-%m-%dT%H:%M:%S%z'; }
logln() { printf '%s [%s] %s\n' "$(ts)" "$mode" "$*" >>"$log"; }
notify() {
  # $1 title, $2 message. Quotes are stripped so they cannot break the AppleScript string.
  local title="${1//\"/}" msg="${2//\"/}"
  [ "${CURSOR_MAINTENANCE_NO_NOTIFY:-}" = "1" ] && return 0
  /usr/bin/osascript -e "display notification \"${msg:0:200}\" with title \"${title}\" sound name \"Basso\"" >/dev/null 2>&1 || true
}

build() {
  # go run would turn every non-zero exit into 1; build so the exit codes survive.
  if ! out="$(go build -o "$bin/cursor_proto_sync" ./cmd/cursor_proto_sync 2>&1 && go build -o "$bin/cursor_canary" ./cmd/cursor_canary 2>&1)"; then
    logln "build failed: $out"
    notify "Cursor maintenance" "Building the maintenance tools failed; see logs/cursor-maintenance.log"
    exit 1
  fi
}

agent_version() {
  local target
  target="$(readlink "$agent" 2>/dev/null)" || return 1
  basename "$(dirname "$target")"
}

run_check() {
  local out rc
  out="$("$bin/cursor_proto_sync" --check --agent "$agent" 2>&1)"
  rc=$?
  printf '%s\n' "$out" | sed 's/^/    /' >>"$log"
  case "$rc" in
    0)
      logln "proto check ok"
      if printf '%s\n' "$out" | grep -q '^New exec request types'; then
        notify "Cursor agent $(agent_version)" "New exec request types (soft-denied). See logs/cursor-maintenance.log"
      fi
      ;;
    1)
      logln "proto check: BREAKING changes"
      notify "Cursor agent $(agent_version): protocol changed" "Breaking changes in used messages. Run go run ./cmd/cursor_proto_sync --check"
      ;;
    *)
      logln "proto check failed to run (exit $rc)"
      notify "Cursor maintenance" "cursor_proto_sync --check failed (exit $rc); see logs/cursor-maintenance.log"
      ;;
  esac
  return "$rc"
}

run_canary() {
  local out rc
  out="$("$bin/cursor_canary" --env-file "$repo/.env" 2>&1)"
  rc=$?
  printf '%s\n' "$out" | sed 's/^/    /' >>"$log"
  case "$rc" in
    0) logln "canary ok" ;;
    3) logln "canary: cursor provider not registered (expected before Phase 4); no notification" ;;
    1) logln "canary: PROTOCOL BROKEN"; notify "Cursor canary: protocol broken" "${out#cursor_canary: }" ;;
    2) logln "canary: AUTH EXPIRED"; notify "Cursor canary: auth expired" "Re-login needed: ${out#cursor_canary: }" ;;
    *) logln "canary inconclusive (exit $rc)"; notify "Cursor canary: inconclusive" "${out#cursor_canary: }" ;;
  esac
  return "$rc"
}

case "$mode" in
  watch)
    # The updater writes the new version dir before moving the symlink; let it settle.
    sleep "${CURSOR_WATCH_SETTLE_SECONDS:-60}"
    version="$(agent_version)" || { logln "agent symlink $agent not readable"; notify "Cursor maintenance" "agent symlink not readable after a versions/ change"; exit 1; }
    if [ -f "$state" ] && [ "$(cat "$state")" = "$version" ]; then
      exit 0 # versions/ changed (cleanup, partial download) but the active agent did not
    fi
    logln "agent version $version: checking"
    build
    run_check
    check_rc=$?
    run_canary
    canary_rc=$?
    # Remember the version only when both steps completed; failures retry on the next change.
    if [ "$check_rc" -eq 0 ] && { [ "$canary_rc" -eq 0 ] || [ "$canary_rc" -eq 3 ]; }; then
      printf '%s\n' "$version" >"$state"
    fi
    ;;
  daily)
    build
    run_canary
    ;;
  *)
    echo "usage: $0 watch|daily" >&2
    exit 2
    ;;
esac
exit 0

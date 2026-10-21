#!/usr/bin/env bash
# Cheap end-to-end check of the live proxy (127.0.0.1:8317). Exit 0 = healthy.
#   scripts/smoke-live.sh
# Checks: management page, /v8/management/quota/{claude,cursor}, one tiny claude-haiku
# answer and one tiny cursor/composer-2.5 answer through claudep (1 Cursor run).
# Runs claudep from a scratch dir outside the repo (a repo CLAUDE.md would leak into prompts).
# SMOKE_FORCE_FAIL=1 fails right after the management checks (tests the deploy rollback path
# without spending model calls). SMOKE_SKIP_MODELS=1 skips the two model answers.
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
base="http://127.0.0.1:8317"
fail() { echo "smoke: FAIL: $*" >&2; exit 1; }

# The proxy may still be starting after a kickstart: wait up to 30 s for the management page.
up=""
for _ in $(seq 1 30); do
  if curl -fsS -o /dev/null "$base/management.html" 2>/dev/null; then up=1; break; fi
  sleep 1
done
[ -n "$up" ] || fail "management page not 200"
echo "smoke: management page 200"

pw="$(sed -n 's/^MANAGEMENT_PASSWORD=//p' "$repo/.env")"
[ -n "$pw" ] || fail "MANAGEMENT_PASSWORD missing in .env"
for p in claude cursor; do
  # The key goes to curl on stdin (config), never on argv or stdout.
  code="$(printf 'header = "X-Management-Key: %s"\n' "$pw" |
    curl -sS -o /dev/null -w '%{http_code}' --config - "$base/v8/management/quota/$p")" || code="000"
  [ "$code" = "200" ] || fail "/v8/management/quota/$p -> $code"
  echo "smoke: quota/$p 200"
done
unset pw

[ "${SMOKE_FORCE_FAIL:-}" = "1" ] && fail "SMOKE_FORCE_FAIL=1"
[ "${SMOKE_SKIP_MODELS:-}" = "1" ] && { echo "smoke: OK (models skipped)"; exit 0; }

scratch="$(mktemp -d "${TMPDIR:-/tmp}/cpa-smoke.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
ask() { # ask <model>: prints the answer; 180 s cap via perl alarm (no coreutils timeout on macOS)
  (cd "$scratch" && perl -e 'alarm shift; exec @ARGV' 180 \
    "$HOME/.local/bin/claudep" -p --no-session-persistence --model "$1" \
    "Reply with the single word pong and nothing else." </dev/null 2>&1) || true
}
for m in claude-haiku-5-5 cursor/composer-2.5; do
  out="$(ask "$m")"
  printf '%s' "$out" | grep -qi pong || fail "$m answered: $(printf '%s' "$out" | head -c 300)"
  echo "smoke: $m answered pong"
done
echo "smoke: OK"

#!/usr/bin/env bash
# Rebuild the live proxy from this checkout's main branch, restart the LaunchAgent and smoke-test it.
# Restart is a single kickstart: never bootout/bootstrap (running sessions may depend on the proxy).
#   scripts/deploy-local.sh
# After the kickstart it runs scripts/smoke-live.sh. On failure it restores
# bin/cli-proxy-api-rollback, kickstarts again, re-runs the smoke and exits non-zero.
# SMOKE_FORCE_FAIL=1 forces the first smoke to fail (tests the rollback path).
# Manual rollback: cp bin/cli-proxy-api-rollback bin/cli-proxy-api-live && launchctl kickstart -k gui/$(id -u)/com.szymon.cliproxyapi
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"
branch="$(git branch --show-current)"
[ "$branch" = "main" ] || { echo "deploy-local: checkout is on '$branch', not main" >&2; exit 1; }
[ -z "$(git status --porcelain --untracked-files=no)" ] || { echo "deploy-local: uncommitted changes; commit first" >&2; exit 1; }
label="gui/$(id -u)/com.szymon.cliproxyapi"
go build -o bin/cli-proxy-api-next ./cmd/server
[ -f bin/cli-proxy-api-live ] && cp bin/cli-proxy-api-live bin/cli-proxy-api-rollback
mv bin/cli-proxy-api-next bin/cli-proxy-api-live
launchctl kickstart -k "$label"
sleep 3
if SMOKE_FORCE_FAIL="${SMOKE_FORCE_FAIL:-}" scripts/smoke-live.sh; then
  echo "deployed $(git rev-parse --short HEAD) ($(git log -1 --format=%s))"
  exit 0
fi
echo "deploy-local: smoke failed; rolling back to bin/cli-proxy-api-rollback" >&2
cp bin/cli-proxy-api-rollback bin/cli-proxy-api-live
launchctl kickstart -k "$label"
sleep 3
if SMOKE_FORCE_FAIL= scripts/smoke-live.sh; then
  echo "deploy-local: rolled back; previous binary is live and healthy" >&2
else
  echo "deploy-local: rolled back, but the smoke still fails: check logs/launchd.err.log" >&2
fi
exit 1

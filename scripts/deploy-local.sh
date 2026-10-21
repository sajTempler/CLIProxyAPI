#!/usr/bin/env bash
# Rebuild the live proxy from this checkout's main branch and restart the LaunchAgent.
# Restart is a single kickstart: never bootout/bootstrap (running sessions may depend on the proxy).
#   scripts/deploy-local.sh
# Rollback: cp bin/cli-proxy-api-rollback bin/cli-proxy-api-live && launchctl kickstart -k gui/$(id -u)/com.szymon.cliproxyapi
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"
branch="$(git branch --show-current)"
[ "$branch" = "main" ] || { echo "deploy-local: checkout is on '$branch', not main" >&2; exit 1; }
[ -z "$(git status --porcelain --untracked-files=no)" ] || { echo "deploy-local: uncommitted changes; commit first" >&2; exit 1; }
[ -f bin/cli-proxy-api-live ] && cp bin/cli-proxy-api-live bin/cli-proxy-api-rollback
go build -o bin/cli-proxy-api-live ./cmd/server
launchctl kickstart -k "gui/$(id -u)/com.szymon.cliproxyapi"
sleep 3
curl -fsS -o /dev/null "http://127.0.0.1:8317/management.html" && echo "deployed $(git rev-parse --short HEAD) ($(git log -1 --format=%s))"

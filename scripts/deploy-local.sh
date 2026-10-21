#!/usr/bin/env bash
# Rebuild the live proxy from the deploy/local worktree (main + merged feature branches)
# and restart the LaunchAgent. Feature branches never deploy directly.
#   scripts/deploy-local.sh [branch...]   merge the given branches first, then deploy
set -euo pipefail
repo="$(cd "$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --path-format=absolute --git-common-dir)/.." && pwd)"
wt="$repo/.worktrees/deploy"
cd "$wt"
for b in "$@"; do git merge --no-edit "$b"; done
[ -f "$repo/bin/cli-proxy-api-live" ] && cp "$repo/bin/cli-proxy-api-live" "$repo/bin/cli-proxy-api-rollback"
go build -o "$repo/bin/cli-proxy-api-live" ./cmd/server
launchctl kickstart -k "gui/$(id -u)/com.szymon.cliproxyapi"
sleep 3
curl -fsS -o /dev/null "http://127.0.0.1:8317/management.html" && echo "deployed $(git rev-parse --short HEAD) ($(git log -1 --format=%s))"
# Rollback: cp bin/cli-proxy-api-rollback bin/cli-proxy-api-live && launchctl kickstart -k gui/$(id -u)/com.szymon.cliproxyapi

#!/bin/sh
set -eu
cd "$(dirname "$0")/.."

live=$(sed -n 's/^server 172\.30\.0\.2:.*/app_blue/p; s/^server 172\.30\.0\.5:.*/app_green/p' nginx/runtime/app-upstream.conf 2>/dev/null || true)
test -n "$live" || { echo 'nginx/runtime/app-upstream.conf names no live slot; run tools/deploy.sh first.' >&2; exit 1; }
backup_stamp=$(date -u +%Y%m%dT%H%M%SZ)
backup_path="/data/backups/$backup_stamp"
docker compose exec -T "$live" /app/server --backup "$backup_path"
printf 'Backup saved under ./data/backups/%s; copy it to encrypted storage off this host.\n' "$backup_stamp"

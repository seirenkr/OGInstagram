#!/bin/sh
set -eu
cd "$(dirname "$0")/.."

backup_stamp=$(date -u +%Y%m%dT%H%M%SZ)
backup_path="/data/backups/$backup_stamp"
docker compose exec -T app /app/server --backup "$backup_path"
printf 'Backup saved under ./data/backups/%s; copy it to encrypted storage off this VM.\n' "$backup_stamp"

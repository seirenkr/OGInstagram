#!/bin/sh
set -eu
cd "$(dirname "$0")/.."

backup_stamp=$(date -u +%Y%m%dT%H%M%SZ)
backup_path="/data/backups/$backup_stamp"
docker compose exec -T app /bin/mkdir -p -m 0700 /data/backups
docker compose exec -T app /app/server --backup "$backup_path"
# The generated offload keyring keeps already shared media links valid.
docker compose exec -T app /bin/sh -c 'test ! -f /data/offload-signing-keys.json || cp -p /data/offload-signing-keys.json "$1/"' sh "$backup_path"
printf 'Backup saved under ./data/backups/%s; copy it to encrypted storage off this VM.\n' "$backup_stamp"

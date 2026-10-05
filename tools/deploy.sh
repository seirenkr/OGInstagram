#!/bin/sh
set -eu
cd "$(dirname "$0")/.."

test -f .env || { echo 'Create .env from .env.example first.' >&2; exit 1; }
test -s secrets/tunnel-token || { echo 'Install secrets/tunnel-token first.' >&2; exit 1; }
test -d data || { echo 'Create data owned by UID 65532 first; see docs/vultr-deployment.md.' >&2; exit 1; }

docker compose config --quiet
# The app image is loaded with `docker load` (no registry); only cloudflared is pulled.
docker compose pull cloudflared
# One application instance owns the SQLite state and proxy budget.
previous=$(docker compose ps -a --format '{{.Image}}' app 2>/dev/null || true)
docker compose stop cloudflared app
docker compose up -d --wait --wait-timeout 90
current=$(docker compose ps --format '{{.Image}}' app)
# Keep the running and previous app images for rollback (set OG_IMAGE back and
# rerun this script); remove every other unused image from the 32 GB disk.
docker image prune -af --filter 'label!=org.opencontainers.image.title=oginstagram'
docker images --format '{{.Repository}}:{{.Tag}}' oginstagram | while read -r image; do
  case "$image" in "$current"|"$previous") ;; *) docker rmi "$image" ;; esac
done
echo "deployed $current (rollback: $previous)"

#!/bin/sh
set -eu
cd "$(dirname "$0")/.."

test -f .env || { echo 'Create .env from .env.example first.' >&2; exit 1; }
test -s secrets/tunnel-token || { echo 'Install secrets/tunnel-token first.' >&2; exit 1; }
test -d data || { echo 'Create data owned by UID 65532 first; see docs/vultr-deployment.md.' >&2; exit 1; }

docker compose config --quiet
docker compose pull app cloudflared
# One application instance owns the SQLite state and proxy budget.
docker compose stop cloudflared app
docker compose up -d --wait --wait-timeout 90
docker compose exec -T app /app/server --healthcheck
# Superseded images otherwise accumulate on the 32 GB disk that holds /data.
docker image prune -af

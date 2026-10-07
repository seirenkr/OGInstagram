#!/bin/sh
# Zero-downtime blue/green deploy of OG_IMAGE (same flow as docker-rollout and
# kamal-proxy): start the idle slot next to the live one, wait until it is
# healthy, point nginx at it, then stop the old slot so in-flight requests drain.
set -eu
cd "$(dirname "$0")/.."

test -f .env || { echo 'Create .env from .env.example first.' >&2; exit 1; }
test -s secrets/tunnel-token || { echo 'Install secrets/tunnel-token first.' >&2; exit 1; }
test -d data || { echo 'Create data/ owned by UID 65532 first.' >&2; exit 1; }

docker compose config --quiet
# The app image is loaded with `docker load` (no registry); only cloudflared is pulled.
docker compose pull cloudflared

live=$(docker compose ps --status running --format '{{.Service}}' app_blue app_green | head -n 1)
# With no slot live, start green: the pre-blue/green `app` still holds blue's address.
if [ "$live" = app_green ]; then next=app_blue next_ip=172.30.0.2; else next=app_green next_ip=172.30.0.5; fi
previous=$( [ -n "$live" ] && docker compose ps --format '{{.Image}}' "$live" || true)
# Before the first blue/green deploy, the single `app` service is still live.
[ -n "$live" ] || previous=$(docker ps --filter label=com.docker.compose.service=app --format '{{.Image}}' | head -n 1)

# Both slots share data/ briefly: SQLite runs in WAL mode and the proxy budget
# is a ledger in that database, so the overlap is safe.
echo "starting $next"
if ! docker compose up -d --no-deps --wait --wait-timeout 90 "$next"; then
  docker compose logs --tail 50 "$next" >&2
  docker compose rm -sf "$next"
  echo "$next is not healthy; ${live:-the old app} keeps serving" >&2
  exit 1
fi

mkdir -p nginx/runtime
printf 'server %s:8080;\n' "$next_ip" > nginx/runtime/app-upstream.conf.tmp
mv nginx/runtime/app-upstream.conf.tmp nginx/runtime/app-upstream.conf
if [ -n "$(docker compose ps --status running -q proxy)" ] && docker compose exec -T proxy test -f /etc/nginx/runtime/app-upstream.conf; then
  # Graceful reload: old workers finish their requests, new ones use $next.
  docker compose exec -T proxy nginx -t -q
  docker compose exec -T proxy nginx -s reload
  # ponytail: fixed pause for the reload to take effect; poll worker PIDs if it ever proves short.
  sleep 2
fi
# Creates proxy/cloudflared on a fresh host and applies their config changes;
# a no-op otherwise. Also removes the pre-blue/green `app` container.
docker compose up -d --wait --wait-timeout 90 --remove-orphans proxy cloudflared

if [ -n "$live" ]; then
  echo "draining $live"
  docker compose stop "$live"
  docker compose rm -f "$live"
fi

current=$(docker compose ps --format '{{.Image}}' "$next")
# Keep the running and previous app images for rollback (set OG_IMAGE back and
# rerun this script); remove dangling layers and older app images only.
docker image prune -f
docker images --format '{{.Repository}}:{{.Tag}}' oginstagram | while read -r image; do
  case "$image" in "$current"|"$previous") ;; *) docker rmi "$image" ;; esac
done
echo "deployed $current on $next (rollback: $previous)"

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
# The app image is loaded with `docker load` (no registry), and cloudflared is
# pulled only when missing, so a rollback works while Docker Hub is down.
docker compose pull --policy missing cloudflared

# The live slot is the one nginx routes to, whatever else is running.
live=$(sed -n 's/^server 172\.30\.0\.2:.*/app_blue/p; s/^server 172\.30\.0\.5:.*/app_green/p' nginx/runtime/app-upstream.conf 2>/dev/null || true)
if [ "$live" = app_green ]; then next=app_blue next_ip=172.30.0.2 other=app_green; else next=app_green next_ip=172.30.0.5 other=app_blue; fi
previous=$( [ -n "$live" ] && docker compose ps -a --format '{{.Image}}' "$live" || true)

# nginx.conf is a single-file bind mount, which a reload cannot pick up after
# an editor replaces the file, so a changed config recreates the proxy instead.
sum=$(sha256sum nginx/nginx.conf | cut -d' ' -f1)
conf_changed=
[ "$sum" = "$(cat nginx/runtime/nginx.conf.sha256 2>/dev/null || true)" ] || conf_changed=1
proxy_running=$(docker compose ps --status running -q proxy)
if [ -n "$conf_changed" ] && [ -n "$proxy_running" ]; then
  docker compose exec -T proxy sh -c 'cat > /tmp/nginx.conf.new' < nginx/nginx.conf
  docker compose exec -T proxy nginx -t -q -c /tmp/nginx.conf.new
fi

# Both slots share data/ briefly; SQLite runs in WAL mode, so the overlap is safe.
echo "starting $next"
if ! docker compose up -d --no-deps --wait --wait-timeout 90 "$next"; then
  docker compose logs --tail 50 "$next" >&2
  docker compose stop "$next"
  echo "$next is not healthy${live:+; $live keeps serving}" >&2
  exit 1
fi

mkdir -p nginx/runtime
old=$(cat nginx/runtime/app-upstream.conf 2>/dev/null || true)
printf 'server %s:8080;\n' "$next_ip" > nginx/runtime/app-upstream.conf.tmp
mv nginx/runtime/app-upstream.conf.tmp nginx/runtime/app-upstream.conf
if [ -n "$proxy_running" ]; then
  if [ -n "$conf_changed" ]; then
    docker compose up -d --no-deps --force-recreate --wait proxy
  else
    # Graceful reload: old workers finish their requests, new ones use $next.
    if ! docker compose exec -T proxy nginx -t -q || ! docker compose exec -T proxy nginx -s reload; then
      printf '%s\n' "$old" > nginx/runtime/app-upstream.conf
      exit 1
    fi
  fi
fi
# Creates proxy/cloudflared on a fresh host and applies their config changes;
# a no-op otherwise.
docker compose up -d --wait --wait-timeout 90 proxy cloudflared
printf '%s\n' "$sum" > nginx/runtime/nginx.conf.sha256

if [ -n "$live" ]; then
  # The reload settles and the old slot's detached flights and background
  # helpers, bounded by cacheSharedWorkTimeout (10 s), finish before it stops.
  sleep 12
fi
echo "draining $other"
docker compose stop "$other"

current=$(docker compose ps --format '{{.Image}}' "$next")
# Keep the running and previous app images for rollback (set OG_IMAGE back and
# rerun this script); remove dangling layers and older app images only. A
# redeploy of the same image skips this, so the last rollback image survives.
if [ "$current" != "$previous" ]; then
  docker image prune -f
  docker images --format '{{.Repository}}:{{.Tag}}' oginstagram | while read -r image; do
    case "$image" in "$current"|"$previous") ;; *) docker rmi "$image" ;; esac
  done
fi
echo "deployed $current on $next (rollback: $previous)"

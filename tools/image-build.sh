#!/bin/sh
# Builds oginstagram:<8-char commit hash> for linux/amd64 from a clean, pushed main.
set -eu
cd "$(dirname "$0")/.."

test -z "$(git status --porcelain)" || { echo 'commit first: the image tag and OG_VERSION are the 8-char commit hash' >&2; exit 1; }
git fetch -q origin main
git merge-base --is-ancestor HEAD origin/main || { echo 'release from main: merge and push this commit first' >&2; exit 1; }
v=$(git rev-parse --short=8 HEAD)

# git status cannot see the gitignored private sources, so the image carries
# their fingerprint and a tag is never reused for different private code.
sums=$(sha256sum server/*_private.go server/*_private_test.go harvest/*.mjs harvest/package*.json Dockerfile.private 2>/dev/null) || true
private=${sums:+$(printf '%s\n' "$sums" | sha256sum | cut -c1-12)}
if built=$(docker image inspect -f '{{index .Config.Labels "org.oginstagram.private"}}' "oginstagram:$v" 2>/dev/null) && test "$built" != "$private"; then
  echo "oginstagram:$v already exists with private sources '$built', now '$private': commit, or remove that image on purpose" >&2
  exit 1
fi

{ cat Dockerfile; test ! -f Dockerfile.private || cat Dockerfile.private; } |
  docker buildx build --platform linux/amd64 --build-arg OG_VERSION="$v" \
    --label org.oginstagram.private="$private" -t "oginstagram:$v" --load -f - .
echo "oginstagram:$v"

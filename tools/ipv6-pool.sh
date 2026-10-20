#!/bin/sh
# Assigns the IPv6 egress pool (random addresses in the interface's /64, kept
# in a root-owned file) to the host. Idempotent; run as root.
set -eu
cd "$(dirname "$0")/.."

install -d -o 65532 -g 65532 -m 0750 run/egress

dev=$(ip -6 route show default | awk '{for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit }}')
if [ -z "$dev" ]; then
  echo 'no IPv6 default route; the egress stays off and the app uses the DC proxy' >&2
  exit 0
fi
pool=/etc/oginstagram/ipv6-pool.txt
install -d -m 0755 /etc/oginstagram
if [ ! -s "$pool" ]; then
  addr=$(ip -6 addr show dev "$dev" scope global -deprecated | awk '$1 == "inet6" { print $2; exit }')
  if [ -z "$addr" ]; then
    echo "no global IPv6 address on $dev yet; the egress stays off" >&2
    exit 0
  fi
  python3 -I - "$addr" 16 > "$pool.tmp" <<'PY'
import ipaddress, secrets, sys
net = ipaddress.IPv6Interface(sys.argv[1].split('/')[0] + '/64').network
for _ in range(int(sys.argv[2])):
    print(net[secrets.randbits(64)])
PY
  mv "$pool.tmp" "$pool"
fi
while read -r addr; do
  ip -6 addr replace "$addr/128" dev "$dev" nodad preferred_lft 0
done < "$pool"

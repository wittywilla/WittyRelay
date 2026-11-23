#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Starts a relay in Docker with the admin page on, puts made-up servers and
# devices on it (scripts/fakeload), and sets the admin password, for taking
# the docs' pictures (e2e/screenshots.js). Everything stays on this machine.
#   docker build -t wittyrelay:shots . && scripts/screenshots.sh wittyrelay:shots
# Runs the load until stopped; then remove the relay with: scripts/screenshots.sh stop
set -euo pipefail
name=wr-shots
if [ "${1:-}" = stop ]; then
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker volume rm wr-shots-data >/dev/null 2>&1 || true
  pkill -f "fakeload" || true
  exit 0
fi
img=${1:-wittyrelay:shots}
key=$(head -c 24 /dev/urandom | base64 | tr -d '/+=' | head -c 32)
password=${ADMIN_PASSWORD:-screenshots only, on this machine}
base=http://localhost:18281
docker rm -f "$name" >/dev/null 2>&1 || true
docker volume rm wr-shots-data >/dev/null 2>&1 || true
docker run -d --name "$name" --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -e WITTYRELAY_NAME="Example family relay" -e WITTYRELAY_NOTE="Run by Sam, for the family's servers." \
  -e WITTYRELAY_ANNOUNCE=/dns4/relay.example.com/udp/4001/quic-v1,/dns4/relay.example.com/tcp/4001 \
  -e WITTYRELAY_KEYS="$key" -e WITTYRELAY_ALLOW_PRIVATE=1 -e WITTYRELAY_PER_IP=16 -e WITTYRELAY_ADMIN=:8081 \
  -p 127.0.0.1:14001:4001/tcp -p 127.0.0.1:18280:8080 -p 127.0.0.1:18281:8081 -v wr-shots-data:/data "$img" >/dev/null
for _ in $(seq 1 30); do curl -fsS http://127.0.0.1:18280/readyz >/dev/null 2>&1 && break; sleep 1; done
token=$(docker logs "$name" 2>&1 | grep -o '"setup token":"[^"]*"' | head -1 | cut -d'"' -f4)
jar=$(mktemp)
csrf=$(curl -fsS -c "$jar" -b "$jar" "$base/setup" | grep -o 'name="csrf" value="[^"]*"' | cut -d'"' -f4)
curl -fsS -o /dev/null -c "$jar" -b "$jar" -H "Origin: $base" --data-urlencode "csrf=$csrf" --data-urlencode "token=$token" \
  --data-urlencode "password=$password" --data-urlencode "again=$password" "$base/setup"
rm -f "$jar"
id=$(docker exec "$name" wittyrelay id)
echo "relay $id; admin page $base; made-up load running until stopped (Ctrl-C)"
exec go run ./scripts/fakeload -relay "/ip4/127.0.0.1/tcp/14001/p2p/$id" -key "$key" -servers 2 -devices 2 -kbps 5000

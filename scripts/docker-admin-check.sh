#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Runs the image as the Compose file does (read-only root, no capabilities,
# non-root) with the admin page on, and goes through it with curl: the setup
# token from the log, setting the password, signing in, saving a setting,
# and the setting still there after a restart. CI runs it; so can you:
#   docker build -t wittyrelay:ci . && scripts/docker-admin-check.sh wittyrelay:ci
set -euo pipefail
img=${1:-wittyrelay:ci}
name=wr-admin-check
vol=wr-admin-check-data
http=127.0.0.1:18180
base=http://localhost:18181
jar=$(mktemp)
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; docker volume rm "$vol" >/dev/null 2>&1 || true; rm -f "$jar"; }
trap cleanup EXIT
cleanup

docker run -d --name "$name" --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -e WITTYRELAY_ACCESS=open -e WITTYRELAY_ALLOW_PRIVATE=1 -e WITTYRELAY_ADMIN=:8081 \
  -p "$http:8080" -p 127.0.0.1:18181:8081 -v "$vol:/data" "$img" >/dev/null
for _ in $(seq 1 30); do curl -fsS "http://$http/readyz" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "http://$http/readyz" >/dev/null

test "$(docker inspect -f '{{.Config.User}}' "$name")" = "65532:65532"
test "$(docker inspect -f '{{.HostConfig.ReadonlyRootfs}}' "$name")" = "true"

# Off the page: nothing but sign-in and setup without a session.
test "$(curl -s -o /dev/null -w '%{http_code}' "$base/api/status")" = 401
test "$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: evil.example' "$base/setup")" = 421
curl -fsS -D - -o /dev/null "$base/setup" | grep -qi "content-security-policy: default-src 'none'"

token=$(docker logs "$name" 2>&1 | grep -o '"setup token":"[^"]*"' | head -1 | cut -d'"' -f4)
test -n "$token"
form() { curl -fsS -c "$jar" -b "$jar" "$base$1" | grep -o 'name="csrf" value="[^"]*"' | head -1 | cut -d'"' -f4; }
code() { curl -s -o /dev/null -w '%{http_code}' -c "$jar" -b "$jar" -H "Origin: $base" "$@"; }

csrf=$(form /setup)
test "$(code --data-urlencode "csrf=$csrf" --data-urlencode "token=wrong" --data-urlencode "password=a docker check password" \
  --data-urlencode "again=a docker check password" "$base/setup")" = 401
csrf=$(form /setup)
test "$(code --data-urlencode "csrf=$csrf" --data-urlencode "token=$token" --data-urlencode "password=a docker check password" \
  --data-urlencode "again=a docker check password" "$base/setup")" = 303
curl -fsS -c "$jar" -b "$jar" "$base/status" | grep -q "Servers registered"

csrf=$(form /settings)
test "$(code --data-urlencode "csrf=$csrf" --data-urlencode "kbps=6000" --data-urlencode "logLevel=info" "$base/settings")" = 303
test "$(code --data-urlencode "csrf=nope" --data-urlencode "kbps=7000" "$base/settings")" = 403

docker restart "$name" >/dev/null
for _ in $(seq 1 30); do curl -fsS "http://$http/readyz" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "http://$http/info" | grep -q '"kbps":6000'
# The password outlives the restart; the setup token doesn't come back.
test "$(docker logs "$name" 2>&1 | grep -c '"setup token":')" = 1
echo "admin page in Docker: ok"

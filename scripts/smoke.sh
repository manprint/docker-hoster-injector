#!/usr/bin/env bash
# Smoke test of what a user actually runs: the shipped docker-compose.yml.
#
# Starts the agent from the compose file (directory mount of /etc), starts a
# web container, and checks that the name resolves and serves, that the
# operator's own lines are untouched, that replacing /etc/hosts by rename does
# not stop the agent, and that "compose down" gives the file back as it was.
#
# It rewrites the real /etc/hosts and needs Docker and root (sudo -n is used for
# the one step that replaces the file). Run from the repository root:
#   make smoke
set -euo pipefail

cd "$(dirname "$0")/.."

# The image under test is built from this checkout, not pulled.
COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.build.yml)
PROBE=smoke-web
PORT=${SMOKE_PORT:-18080}
SUDO=""
[ "$(id -u)" -eq 0 ] || SUDO="sudo -n"
BEFORE="$(mktemp)"
fail() { echo "FAIL: $*" >&2; exit 1; }

cleanup() {
  docker rm -f "$PROBE" "$PROBE-2" >/dev/null 2>&1 || true
  "${COMPOSE[@]}" down >/dev/null 2>&1 || true
  rm -f "$BEFORE"
}
trap cleanup EXIT

cp /etc/hosts "$BEFORE"

echo "== start the agent from docker-compose.yml"
"${COMPOSE[@]}" up -d --build
for _ in $(seq 1 30); do
  [ "$(docker inspect -f '{{.State.Health.Status}}' docker-hoster-injector)" = healthy ] && break
  sleep 1
done
[ "$(docker inspect -f '{{.State.Health.Status}}' docker-hoster-injector)" = healthy ] || fail "the agent is not healthy"

echo "== a published container gets its name"
docker run -d --name "$PROBE" -p "$PORT:8080" busybox:latest \
  sh -c 'echo ok > /tmp/index.html && exec httpd -f -p 8080 -h /tmp' >/dev/null
# The hosts file is the evidence: a resolver with a search domain or a wildcard
# can make getent answer for a name nobody published.
block() { sed -n '/BEGIN docker-hoster-injector/,/END docker-hoster-injector/p' /etc/hosts; }
for _ in $(seq 1 30); do block | grep -q " $PROBE.docker.local" && break; sleep 1; done
block | grep -q " $PROBE.docker.local" || fail "$PROBE.docker.local is not in the managed block"
getent hosts "$PROBE.docker.local" || fail "$PROBE.docker.local does not resolve"
[ "$(curl -fsS --max-time 5 "http://$PROBE.docker.local:$PORT/")" = ok ] || fail "the published port does not answer through the name"

echo "== the operator's own lines are untouched"
diff <(sed '/BEGIN docker-hoster-injector/,/END docker-hoster-injector/d' /etc/hosts) "$BEFORE" || fail "user lines changed"

echo "== the agent survives /etc/hosts being replaced by a rename"
$SUDO sh -c 'cp -p /etc/hosts /etc/hosts.smoke && mv /etc/hosts.smoke /etc/hosts'
docker run -d --name "$PROBE-2" busybox:latest sleep 300 >/dev/null
for _ in $(seq 1 30); do block | grep -q " $PROBE-2.docker.local" && break; sleep 1; done
block | grep -q " $PROBE-2.docker.local" || fail "the agent stopped writing after the rename"

echo "== compose down gives the file back"
docker rm -f "$PROBE" "$PROBE-2" >/dev/null
"${COMPOSE[@]}" down
diff /etc/hosts "$BEFORE" || fail "/etc/hosts is not as it was"
if ls /etc/.hosts-docker-hoster-injector-* >/dev/null 2>&1; then fail "temporary file left in /etc"; fi

echo "smoke OK"

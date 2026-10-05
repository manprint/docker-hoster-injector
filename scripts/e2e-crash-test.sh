#!/usr/bin/env bash
# End-to-end crash test against a real Docker daemon.
#
# Kills the agent with SIGKILL while containers change underneath it, restarts
# it, and verifies the host converges to the correct state with the operator's
# own /etc/hosts entries preserved byte for byte.
#
# Requires root (to write /etc/hosts) and a Docker daemon:
#   sudo -n bash scripts/e2e-crash-test.sh
set -uo pipefail

BIN=./bin/docker-hoster-injector
PASS=0; FAIL=0
ok()   { echo "  PASS  $*"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL  $*"; FAIL=$((FAIL+1)); }
check(){ if eval "$2"; then ok "$1"; else bad "$1"; fi; }

run_agent() { # seconds
  sudo -n env LOG_FORMAT=text WEB_ENABLED=false RESYNC_INTERVAL=30s \
    EVENT_DEBOUNCE=20ms LOG_LEVEL=debug timeout "$1" "$BIN" >/tmp/agent.log 2>&1
}
run_agent_bg() { # seconds -> runs detached
  sudo -n env LOG_FORMAT=text WEB_ENABLED=false RESYNC_INTERVAL=30s \
    EVENT_DEBOUNCE=20ms LOG_LEVEL=debug timeout -s KILL "$1" "$BIN" >/tmp/agent.log 2>&1 &
  echo $!
}
names() { grep -c -- "$1" /etc/hosts 2>/dev/null; }
user_lines() { sed '/BEGIN docker-hoster-injector/,/END docker-hoster-injector/d' /etc/hosts; }

cleanup() {
  sudo -n pkill -x docker-hoster-injector 2>/dev/null
  docker rm -f e2e-a e2e-b e2e-c e2e-host 2>/dev/null
  docker network rm e2e-net 2>/dev/null
  sudo -n cp /tmp/hosts.e2e.backup /etc/hosts 2>/dev/null
}
trap cleanup EXIT

echo "=== setup ==="
sudo -n pkill -x docker-hoster-injector 2>/dev/null
docker rm -f e2e-a e2e-b e2e-c e2e-host 2>/dev/null
docker network rm e2e-net 2>/dev/null
sudo -n cp /etc/hosts /tmp/hosts.e2e.backup
USER_SNAPSHOT="$(user_lines)"
check "snapshot delle entry utente" '[ -n "$USER_SNAPSHOT" ]'
docker network create e2e-net >/dev/null 2>&1

echo
echo "=== 1. avvio: nessun container ==="
run_agent 3
# Other containers on the host are irrelevant here, so the check is scoped to
# the ones this script owns.
check "nessun record per i container e2e" '[ "$(sed -n "/BEGIN docker-hoster-injector/,/END docker-hoster-injector/p" /etc/hosts | grep -cE "e2e-(a|b|c|host)")" = "0" ]'

echo
echo "=== 2. avvio di un container con porta pubblicata ==="
docker run -d --name e2e-a --network e2e-net -p 18081:80 nginx:alpine >/dev/null
run_agent 3
check "record presente per e2e-a" '[ "$(names e2e-a.docker.local)" -ge 1 ]'
check "risolve via nss-files" 'getent ahostsv4 e2e-a.docker.local | grep -q 127.0.0.1'
check "la porta pubblicata risponde" 'curl -sf -o /dev/null --max-time 5 http://e2e-a.docker.local:18081/'
check "la porta diretta risponde" 'curl -sf -o /dev/null --max-time 5 http://e2e-a.docker.local:80/'
check "entry utente intatte" '[ "$(user_lines)" = "$USER_SNAPSHOT" ]'

echo
echo "=== 3. secondo container, alias compose ==="
docker run -d --name e2e-b --network e2e-net --network-alias web nginx:alpine >/dev/null
run_agent 3
check "alias di rete pubblicato" '[ "$(names web.docker.local)" -ge 1 ]'
check "entry utente ancora intatte" '[ "$(user_lines)" = "$USER_SNAPSHOT" ]'

echo
echo "=== 4. container in network host: DEVE essere escluso ==="
docker run -d --name e2e-host --network host nginx:alpine >/dev/null
sleep 1
run_agent 3
check "e2e-host NON pubblicato" '[ "$(names e2e-host.docker.local)" = "0" ]'
check "l'esclusione è motivata nei log" 'grep -q "shares the host network" /tmp/agent.log'

echo
echo "=== 5. KILL -9 mentre i container cambiano ==="
PID=$(run_agent_bg 12)
sleep 2
docker rm -f e2e-a >/dev/null          # cambio sotto l'agente
sleep 0.3
docker run -d --name e2e-c --network e2e-net -p 18082:80 nginx:alpine >/dev/null   # altro cambio
sleep 0.3
docker stop e2e-b >/dev/null
sleep 1
sudo -n kill -9 "$PID" 2>/dev/null
wait "$PID" 2>/dev/null
echo "  (agenteucciso con SIGKILL)"

echo "  --- stato su disco subito dopo il kill ---"
POST_KILL="$(user_lines)"
check "il file è leggibile e non vuoto" '[ -s /etc/hosts ]'
check "entry utente intatte anche dopo il kill" '[ "$POST_KILL" = "$USER_SNAPSHOT" ]'
check "nessun blocco duplicato" '[ "$(grep -c "BEGIN docker-hoster-injector" /etc/hosts)" -le 1 ]'

echo
echo "=== 6. riavvio: riconvergenza ==="
run_agent 4
check "record del container rimosso sparito" '[ "$(names e2e-a.docker.local)" = "0" ]'
check "record del container fermo sparito" '[ "$(names web.docker.local)" = "0" ]'
check "nuovo container pubblicato" '[ "$(names e2e-c.docker.local)" -ge 1 ]'
check "il nuovo container risponde" 'curl -sf -o /dev/null --max-time 5 http://e2e-c.docker.local:18082/'
check "blocco ben formato" '[ "$(grep -c "BEGIN docker-hoster-injector" /etc/hosts)" = "1" ]'
check "entry utente intatte dopo il recovery" '[ "$(user_lines)" = "$USER_SNAPSHOT" ]'

echo
echo "=== 7. ricorrenza di un blocco troncato (artefatto di crash) ==="
sudo -n sed -i 's/^# END docker-hoster-injector$//' /etc/hosts
check "il blocco è ora non terminato" '[ "$(grep -c "END docker-hoster-injector" /etc/hosts)" = "0" ]'
run_agent 4
check "recovery ricostruisce il blocco" '[ "$(grep -c "END docker-hoster-injector" /etc/hosts)" = "1" ]'
check "i record sono tornati" '[ "$(names e2e-c.docker.local)" -ge 1 ]'
check "entry utente intatte" '[ "$(user_lines)" = "$USER_SNAPSHOT" ]'

echo
echo "=== 8. stabilità: applicazioni ripetute non scrivono ==="
BEFORE=$(stat -c %Y /etc/hosts)
run_agent 3; sleep 1
run_agent 3; sleep 1
AFTER=$(stat -c %Y /etc/hosts)
check "il file non è stato riscritto a vuoto" '[ "$BEFORE" = "$AFTER" ]'

echo
echo "=== riepilogo ==="
echo "  passati: $PASS   falliti: $FAIL"
[ "$FAIL" -eq 0 ]

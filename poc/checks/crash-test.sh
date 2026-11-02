#!/usr/bin/env bash
# 갑작스런 종료 시험. 부하(gen-load.py)를 주는 중에 중계나 노드를 강제로 죽이고,
# 보낸 쪽이 '받았다' 고 들은 샘플 수와 두 노드 저장소에 실제로 들어간 샘플 수를 비교한다.
# 사용: checks/crash-test.sh relay   노드 A 의 vmagent 를 kill -9 (부하는 A 먼저, 실패하면 B)
#       checks/crash-test.sh node    노드 B 를 OpenStack 에서 강제 재시작 (부하는 B 먼저, 실패하면 A)
# 부하는 점프 호스트에서 돌린다 (JUMP). node 는 점프 호스트의 OpenStack 관리 권한과 SERVER_B 가 필요하다.
set -euo pipefail
cd "$(dirname "$0")/.."
. ./inventory.env
NAME=${NAME:-cmp-mon}
SERVER_B=${SERVER_B:-ish-mon2-node2}
OPENRC=${OPENRC:-/etc/kolla/admin-openrc.sh}
mode=${1:?relay 또는 node}
run="$mode-$(date +%H%M%S)"
secs=${SECS:-180}
kill_at=${KILL_AT:-60}
ssh_opts=(-n -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new -J "$JUMP")
on() { local h=$1; shift; timeout 120 ssh "${ssh_opts[@]}" "$SSH_USER@$h" "$@"; }
jump() { timeout $(( secs + 120 )) ssh -n -o ConnectTimeout=10 "$JUMP" "$@"; }
log() { echo "[$(date +%T)] $*"; }
count() { # 노드 → run 의 샘플 수 (vmauth 를 거치지 않고 로컬 저장소에 직접)
  on "$1" "curl -s http://127.0.0.1:8428/api/v1/query --data-urlencode 'query=sum(count_over_time(poc_load{run=\"$run\"}[30m]))'" |
    python3 -c 'import json,sys; r=json.load(sys.stdin)["data"]["result"]; print(r[0]["value"][1] if r else 0)'
}
queue() { on "$1" "curl -s http://127.0.0.1:8429/metrics | awk '/^vmagent_remotewrite_pending_data_bytes/ {s+=\$NF} END {printf \"%d\", s}'"; }

scp -q -o ConnectTimeout=10 checks/gen-load.py "$JUMP:/root/poc-gen-load.py"
case $mode in
  relay) urls="https://$NODE_A:4318 https://$NODE_B:4318" ;;
  node)  urls="https://$NODE_B:4318 https://$NODE_A:4318" ;;
  *) echo "relay 또는 node" >&2; exit 1 ;;
esac
out=$(mktemp)
log "부하 시작 run=$run ${secs}초, 주소 순서 $urls"
jump "python3 /root/poc-gen-load.py $run $secs $urls" > "$out" 2>&1 &
load_pid=$!
sleep "$kill_at"
case $mode in
  relay)
    log "노드 A vmagent kill -9 (컨테이너가 죽으면 podman 이 restart: always 로 다시 띄운다)"
    on "$NODE_A" "sudo kill -9 \$(sudo podman inspect -f '{{.State.Pid}}' $NAME-vmagent)" || true
    ;;
  node)
    log "노드 B 강제 재시작 (openstack server reboot --hard $SERVER_B)"
    timeout 60 ssh -n "$JUMP" ". $OPENRC; openstack server reboot --hard $SERVER_B"
    ;;
esac
wait "$load_pid" || true
cat "$out"
result=$(tail -1 "$out"); rm -f "$out"
if [ "$mode" = node ]; then
  up() { on "$NODE_B" "sudo podman ps --filter name=^$NAME- --filter status=running --format '{{.Names}}' | grep -cE '$NAME-(victoria-metrics|vmagent|gateway)\$'"; }
  for _ in $(seq 1 60); do [ "$(up 2>/dev/null)" = 3 ] && break; sleep 10; done
  log "노드 B 컨테이너: $(on "$NODE_B" "sudo podman ps -a --filter name=^$NAME- --format '{{.Names}}={{.Status}}' | tr '\n' ' '")"
fi
for _ in $(seq 1 60); do
  qa=$(queue "$NODE_A"); qb=$(queue "$NODE_B")
  [ "$qa" = 0 ] && [ "$qb" = 0 ] && break; sleep 5
done
log "큐 A $qa / B $qb 바이트"
sleep 45   # 저장소가 최근 30초를 조회에서 숨기므로 기다린다 (-search.latencyOffset)
ca=$(count "$NODE_A"); cb=$(count "$NODE_B")
acked=$(echo "$result" | python3 -c 'import json,sys; print(json.load(sys.stdin)["acked"])')
log "결과 $result"
log "받았다고 들은 샘플 $acked / A 저장소 $ca (차 $(( acked - ca ))) / B 저장소 $cb (차 $(( acked - cb )))"

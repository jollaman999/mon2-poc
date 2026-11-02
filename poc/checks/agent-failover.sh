#!/usr/bin/env bash
# 에이전트(cmp-otelcol) 전환 · 복귀 시험. 에이전트는 agent/run-poc.sh 로 AGENT_HOST 에 띄워 두고,
# 시작 주소가 노드 B 가 되게 한다 (start_key). 에이전트는 1초마다 호스트 메트릭을 걷는다.
# 사용: checks/agent-failover.sh stop   노드 B 의 입구(vmauth)를 멈췄다 켠다 (연결 거부)
#       checks/agent-failover.sh cut    에이전트 호스트에서 노드 B 로 가는 길을 막는다 (응답 없음, 시간 초과)
# 확인: 전환까지 걸린 시간, 복귀 여부, 두 저장소에 1초 샘플이 빠짐없이 있는지
set -euo pipefail
cd "$(dirname "$0")/.."
. ./inventory.env
NAME=${NAME:-cmp-mon}
AGENT_HOST=${AGENT_HOST:-$NODE_A}
DOWN=${DOWN:-90}          # B 를 막아 두는 초
FAILBACK_WAIT=${FAILBACK_WAIT:-100}   # 풀린 뒤 복귀를 기다리는 초 (에이전트 failback_interval 보다 길게)
mode=${1:?stop 또는 cut}
ssh_opts=(-n -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -J "$JUMP")
on() { local h=$1; shift; timeout 120 ssh "${ssh_opts[@]}" "$SSH_USER@$h" "$@"; }
log() { echo "[$(date +%T)] $*"; }
otlp_rows() { on "$1" "curl -s http://127.0.0.1:8429/metrics | awk '/^vmagent_rows_inserted_total\\{type=\"opentelemetry\"\\}/ {print \$NF}'"; }
host_name=$(on "$AGENT_HOST" hostname)
series="system_memory_usage_bytes{state=\"used\",host_name=\"$host_name\"}"
samples() { # 노드 시작epoch 끝epoch → 그 구간의 샘플 수 (1초 간격이면 초 수와 같아야 한다)
  local secs=$(( $3 - $2 ))
  on "$1" "curl -s http://127.0.0.1:8428/api/v1/query --data-urlencode 'query=count_over_time(${series}[${secs}s])' --data-urlencode time=$3" |
    python3 -c 'import json,sys; r=json.load(sys.stdin)["data"]["result"]; print(r[0]["value"][1] if r else 0)'
}

t0=$(date +%s)
a0=$(otlp_rows "$NODE_A"); b0=$(otlp_rows "$NODE_B")
sleep 20
a1=$(otlp_rows "$NODE_A"); b1=$(otlp_rows "$NODE_B")
log "평상시 20초: A 중계 +$(( a1 - a0 )) · B 중계 +$(( b1 - b0 )) 행 (시작 주소 B 면 B 만 늘어야 한다)"

case $mode in
  stop)
    log "노드 B 입구(vmauth) 정지 ${DOWN}초"
    on "$NODE_B" "sudo systemctl stop $NAME-gateway"
    ;;
  cut)
    log "에이전트 호스트에서 노드 B 로 가는 길 차단 ${DOWN}초 (blackhole)"
    on "$AGENT_HOST" "sudo ip route add blackhole $NODE_B/32"
    ;;
esac
tdown=$(date +%s)
sleep "$DOWN"
a2=$(otlp_rows "$NODE_A"); b2=$(otlp_rows "$NODE_B")
log "막힌 동안: A 중계 +$(( a2 - a1 )) · B 중계 +$(( b2 - b1 )) 행 (A 로 넘어갔으면 A 가 늘어야 한다)"
case $mode in
  stop) on "$NODE_B" "sudo systemctl start $NAME-gateway" ;;
  cut)  on "$AGENT_HOST" "sudo ip route del blackhole $NODE_B/32" ;;
esac
tup=$(date +%s)
log "풀림. 복귀를 ${FAILBACK_WAIT}초 기다림"
sleep "$FAILBACK_WAIT"
a3=$(otlp_rows "$NODE_A"); sleep 20; a4=$(otlp_rows "$NODE_A")
b3=$(otlp_rows "$NODE_B"); sleep 20; b4=$(otlp_rows "$NODE_B")
log "복귀 뒤 20초: A 중계 +$(( a4 - a3 )) · B 중계 +$(( b4 - b3 )) 행 (B 로 돌아왔으면 A 는 0 근처)"
on "$AGENT_HOST" "grep -E '주소 전환|돌아가기' ~/cmp-agent/agent.log | tail -6 | sed -E 's/\\{\"resource.*\"from\"/ from/; s/\\t/ /g' | cut -c1-200"

# 큐(노드 사이)가 비고 저장소가 최근 30초를 숨기는 것을 기다린 뒤 센다
sleep 45
t1=$(( $(date +%s) - 40 ))
ca=$(samples "$NODE_A" "$t0" "$t1"); cb=$(samples "$NODE_B" "$t0" "$t1")
log "구간 $(( t1 - t0 ))초 (막힘 $(( tup - tdown ))초 포함): A 저장소 $ca · B 저장소 $cb 샘플 (1초 간격이라 구간 초와 같으면 빠짐 없음)"

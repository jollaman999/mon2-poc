#!/usr/bin/env bash
# 에이전트(cmp-otelcol) 전환 · 복귀 시험. 에이전트는 bin/install-agent.sh 로 AGENT_HOST 에 설치해 두고
# (수집 주기 1초: --interval 1s), 이 스크립트는 평상시에 에이전트가 보내는 노드(시작 주소)를 찾아 그 노드를 막는다.
# 사용: checks/agent-failover.sh stop   시작 주소 노드의 입구(cmp-mon-gateway 컨테이너)를 멈췄다 켠다 (연결 거부)
#       checks/agent-failover.sh cut    에이전트 호스트에서 시작 주소 노드로 가는 길을 막는다 (응답 없음, 시간 초과)
# 확인: 전환까지 걸린 시간, 복귀 여부, 두 저장소에 1초 샘플이 빠짐없이 있는지
# 에이전트 호스트 접속 (환경 변수): AGENT_HOST(필수) AGENT_USER(기본 root) AGENT_JUMP(기본 inventory 의 JUMP)
#   AGENT_KEY(키 파일) 또는 AGENT_PASSWORD(비밀번호, sshpass -e 로 넘긴다). 둘 다 없으면 ssh 기본 설정을 쓴다
set -euo pipefail
cd "$(dirname "$0")/.."
. ./inventory.env
NAME=${NAME:-cmp-mon}
AGENT_HOST=${AGENT_HOST:?에이전트를 설치한 호스트 IP}
AGENT_USER=${AGENT_USER:-root}
AGENT_JUMP=${AGENT_JUMP-${JUMP:-}}
AGENT_LOG=/cmp-agent/logs/cmp-otelcol.log
DOWN=${DOWN:-90}          # 막아 두는 초
FAILBACK_WAIT=${FAILBACK_WAIT:-100}   # 풀린 뒤 복귀를 기다리는 초 (에이전트 failback_interval 보다 길게)
mode=${1:?stop 또는 cut}
ssh_opts=(-n -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -J "$JUMP")
on() { local h=$1; shift; timeout 120 ssh "${ssh_opts[@]}" "$SSH_USER@$h" "$@"; }
agent_opts=(-n -o ConnectTimeout=10); agent_wrap=()
[ -n "$AGENT_JUMP" ] && agent_opts+=(-J "$AGENT_JUMP")
if [ -n "${AGENT_KEY:-}" ]; then
  agent_opts+=(-i "$AGENT_KEY" -o IdentitiesOnly=yes -o BatchMode=yes)
elif [ -n "${AGENT_PASSWORD:-}" ]; then
  export SSHPASS=$AGENT_PASSWORD; agent_wrap=(sshpass -e); agent_opts+=(-o PubkeyAuthentication=no)
fi
on_agent() { timeout 120 "${agent_wrap[@]}" ssh "${agent_opts[@]}" "$AGENT_USER@$AGENT_HOST" "$@"; }
asudo=""; [ "$AGENT_USER" = root ] || asudo=sudo
log() { echo "[$(date +%T)] $*"; }
otlp_rows() { on "$1" "curl -s http://127.0.0.1:8429/metrics | awk '/^vmagent_rows_inserted_total\\{type=\"opentelemetry\"\\}/ {print \$NF}'"; }
host_name=$(on_agent hostname)
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
log "평상시 20초: A 중계 +$(( a1 - a0 )) · B 중계 +$(( b1 - b0 )) 행 (시작 주소 노드만 늘어야 한다)"
# 평상시에 더 많이 받은 쪽을 시작 주소로 보고 그 노드를 막는다
if (( b1 - b0 > a1 - a0 )); then P=B; PIP=$NODE_B; else P=A; PIP=$NODE_A; fi
if [ "$P" = A ]; then S=B; else S=A; fi
rows() { otlp_rows "$PIP"; }
rows_s() { if [ "$S" = A ]; then otlp_rows "$NODE_A"; else otlp_rows "$NODE_B"; fi; }
log "시작 주소: 노드 $P ($PIP)"
p1=$(rows); s1=$(rows_s)

case $mode in
  stop)
    log "노드 $P 입구($NAME-gateway 컨테이너) 정지 ${DOWN}초"
    on "$PIP" "sudo podman stop $NAME-gateway >/dev/null"
    ;;
  cut)
    log "에이전트 호스트에서 노드 $P 로 가는 길 차단 ${DOWN}초 (blackhole)"
    on_agent "$asudo ip route add blackhole $PIP/32"
    ;;
esac
tdown=$(date +%s)
sleep "$DOWN"
p2=$(rows); s2=$(rows_s)
log "막힌 동안: $P 중계 +$(( p2 - p1 )) · $S 중계 +$(( s2 - s1 )) 행 ($S 로 넘어갔으면 $S 가 늘어야 한다)"
case $mode in
  stop) on "$PIP" "sudo podman start $NAME-gateway >/dev/null" ;;
  cut)  on_agent "$asudo ip route del blackhole $PIP/32" ;;
esac
tup=$(date +%s)
log "풀림. 복귀를 ${FAILBACK_WAIT}초 기다림"
sleep "$FAILBACK_WAIT"
s3=$(rows_s); sleep 20; s4=$(rows_s)
p3=$(rows); sleep 20; p4=$(rows)
log "복귀 뒤 20초: $P 중계 +$(( p4 - p3 )) · $S 중계 +$(( s4 - s3 )) 행 ($P 로 돌아왔으면 $S 는 0 근처)"
on_agent "grep -E '주소 전환|돌아가기' $AGENT_LOG | tail -6 | sed -E 's/\\{\"resource.*\"from\"/ from/; s/\\t/ /g' | cut -c1-200"

# 큐(노드 사이)가 비고 저장소가 최근 30초를 숨기는 것을 기다린 뒤 센다
sleep 45
t1=$(( $(date +%s) - 40 ))
ca=$(samples "$NODE_A" "$t0" "$t1"); cb=$(samples "$NODE_B" "$t0" "$t1")
log "구간 $(( t1 - t0 ))초 (막힘 $(( tup - tdown ))초 포함): A 저장소 $ca · B 저장소 $cb 샘플 (1초 간격이라 구간 초와 같으면 빠짐 없음)"

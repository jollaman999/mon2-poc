#!/usr/bin/env bash
# 장애 시험 묶음. 설계 문서 11절의 시험 4 · 2 · 3 을 차례로 돌리고 결과를 출력한다.
# 사용: checks/failover-suite.sh [4] [2] [3]   (기본 전부)
#   4  노드 A 의 vmagent 만 정지 → A 의 vmauth 가 B 의 중계로 넘기는가
#   2  노드 B 의 서비스를 모두 정지 → A 가 계속 받고 B 몫을 쌓는가, 복구 뒤 두 노드 샘플 수가 같은가
#   3  노드 A 에서 B 와의 통신 차단 → 각자 받고, 풀린 뒤 같아지는가
set -euo pipefail
cd "$(dirname "$0")/.."
. ./inventory.env
NAME=${NAME:-cmp-mon}
BASE=${BASE_DIR:-/$NAME}
F=checks/failover.sh
tests=${*:-4 2 3}
ssh_opts=(-n -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
[ -n "${JUMP:-}" ] && ssh_opts+=(-J "$JUMP")
on() { local h=$1; shift; timeout 120 ssh "${ssh_opts[@]}" "$SSH_USER@$h" "$@"; }
log() { echo "[$(date +%T)] $*"; }
metric() { # 노드 포트 지표이름(정규식) → 합계
  on "$1" "curl -s http://127.0.0.1:$2/metrics | grep -E '^$3' | awk '{s+=\$NF} END {printf \"%d\", s}'"
}
queue_sum() { metric "$1" 8429 'vmagent_remotewrite_pending_data_bytes'; }
wait_queue_zero() { # 노드 최대초
  local i; for i in $(seq 1 $(( $2 / 5 ))); do [ "$(queue_sum "$1")" = 0 ] && { log "$1 큐 0 ($(( i * 5 ))초 안)"; return 0; }; sleep 5; done
  log "$1 큐가 $2 초 안에 비지 않음: $(queue_sum "$1") 바이트"; return 1
}

for t in $tests; do
  case $t in
  4)
    log "== 시험 4: 노드 A 의 vmagent 만 정지"
    before=$(metric "$NODE_B" 8429 'vmagent_rows_inserted_total\{type="promremotewrite"\}')
    $F stop A vmagent
    sleep 100
    during=$(metric "$NODE_B" 8429 'vmagent_rows_inserted_total\{type="promremotewrite"\}')
    log "B 의 vmagent 가 remote_write 로 받은 행: 정지 전 $before → 100초 뒤 $during (A 의 vmalert 기록이 B 로 넘어왔으면 늘어난다)"
    on "$NODE_A" "grep -iE 'warn|error' $BASE/logs/gateway.log | tail -3" || true
    $F start A vmagent
    sleep 20
    $F count 5m
    ;;
  2)
    log "== 시험 2: 노드 B 의 서비스를 모두 정지 (3분)"
    $F stop B gateway vmalert alertmanager vmagent vlagent vtagent victoria-metrics victoria-logs victoria-traces >/dev/null
    sleep 180
    log "A 조회 (조회 토큰): $(on "$NODE_A" "curl -sk -o /dev/null -w '%{http_code}' -H \"Authorization: Bearer \$(sudo cat $BASE/conf/tokens/query.token)\" https://$NODE_A:8443/metrics/api/v1/query?query=up")"
    log "A 의 B 몫 큐: $(queue_sum "$NODE_A") 바이트"
    $F start B victoria-metrics victoria-logs victoria-traces vmagent vlagent vtagent alertmanager gateway vmalert >/dev/null
    wait_queue_zero "$NODE_A" 300 || true
    sleep 60
    $F count 8m
    ;;
  3)
    log "== 시험 3: 노드 A 에서 B 와의 통신 차단 (3분, 저절로 풀림)"
    $F cut A 3
    sleep 100
    log "차단 중 A 큐 $(queue_sum "$NODE_A") / B 큐 $(queue_sum "$NODE_B") 바이트"
    sleep 100
    on "$NODE_A" "sudo nft list tables" || true
    wait_queue_zero "$NODE_A" 300 || true
    wait_queue_zero "$NODE_B" 300 || true
    sleep 60
    $F count 8m
    ;;
  esac
done
log "끝"

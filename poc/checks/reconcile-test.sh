#!/usr/bin/env bash
# 주기 대조 시험. 노드 B 의 VictoriaMetrics 를 kill -9 해서 B 에만 빈틈을 만들고(디스크에 아직 안 쓴 몇 초),
# 대조(cmp-mon-reconcile 컨테이너 안의 reconcile-metrics.sh)를 한 번 돌린 뒤 두 노드의 샘플 수가 같아지는지 본다.
# 데이터는 이미 돌고 있는 것(노드 자체 감시, 에이전트, vmalert)을 쓴다. 에이전트를 띄워 두면 더 많다.
set -euo pipefail
cd "$(dirname "$0")/.."
. ./inventory.env
NAME=${NAME:-cmp-mon}
ssh_opts=(-n -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -J "$JUMP")
on() { local h=$1; shift; timeout 600 ssh "${ssh_opts[@]}" "$SSH_USER@$h" "$@"; }
log() { echo "[$(date +%T)] $*"; }
total() { # 노드 시작 끝 → 구간 전체 샘플 수
  on "$1" "curl -s http://127.0.0.1:8428/api/v1/query --data-urlencode 'query=sum(count_over_time({__name__=~\".+\"}[$(( $3 - $2 ))s]))' --data-urlencode time=$3" |
    grep -oE '"[0-9]+"\]' | tr -d '"]'
}

t0=$(( $(date +%s) / 60 * 60 + 60 ))
sleep $(( t0 - $(date +%s) + 60 ))
log "노드 B VictoriaMetrics kill -9 (컨테이너가 죽으면 podman 이 restart: always 로 다시 띄운다)"
on "$NODE_B" "sudo kill -9 \$(sudo podman inspect -f '{{.State.Pid}}' $NAME-victoria-metrics)"
sleep 120
t1=$(( $(date +%s) / 60 * 60 ))
sleep 45   # 저장소가 최근 30초를 숨긴다
ca=$(total "$NODE_A" "$t0" "$t1"); cb=$(total "$NODE_B" "$t0" "$t1")
log "대조 전 $(date -d @$t0 +%T)~$(date -d @$t1 +%T): A $ca · B $cb (차 $(( ca - cb )))"
log "노드 B 에서 대조 실행"
on "$NODE_B" "sudo podman exec $NAME-reconcile sh /reconcile-metrics.sh | tail -4"
sleep 40
ca=$(total "$NODE_A" "$t0" "$t1"); cb=$(total "$NODE_B" "$t0" "$t1")
log "대조 후: A $ca · B $cb (차 $(( ca - cb )))"

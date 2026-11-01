#!/usr/bin/env bash
# 장애 시험 도구. 서비스 정지 · 노드 사이 차단을 걸고, 두 노드 저장소의 샘플 수와 중계 큐를 비교한다.
#
#   checks/failover.sh count [구간] [끝시각]  두 노드 VictoriaMetrics 에서 같은 구간의 샘플 수 (기본 30m, 끝시각 = 60초 전)
#   checks/failover.sh queue                 두 노드 중계(vmagent · vlagent · vtagent)의 목적지별 미전송 바이트
#   checks/failover.sh stop  A|B 서비스...   서비스 정지 (예: stop B victoria-metrics haproxy)
#   checks/failover.sh start A|B 서비스...   서비스 시작
#   checks/failover.sh cut   A 분            A 에서 상대 노드와의 통신을 막는다. 분이 지나면 저절로 풀린다
#   checks/failover.sh heal  A               막은 것을 바로 푼다
set -euo pipefail
cd "$(dirname "$0")/.."
. ./inventory.env

ssh_opts=(-n -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
[ -n "${JUMP:-}" ] && ssh_opts+=(-J "$JUMP")
ip_of()   { case "$1" in A) echo "$NODE_A" ;; B) echo "$NODE_B" ;; *) echo "노드는 A 또는 B" >&2; exit 1 ;; esac; }
peer_of() { case "$1" in A) echo "$NODE_B" ;; B) echo "$NODE_A" ;; esac; }
on()      { local n=$1; shift; timeout 120 ssh "${ssh_opts[@]}" "$SSH_USER@$(ip_of "$n")" "$@"; }

cmd=${1:-}; shift || true
case "$cmd" in
  count)
    win=${1:-30m}
    at=${2:-$(( $(date +%s) - 60 ))}
    for n in A B; do
      printf '%s  ' "$n"
      for q in "sum(count_over_time({job=\"mon2\"}[$win]))" "sum(count_over_time(ALERTS[$win]))"; do
        v=$(on "$n" "curl -s -G http://127.0.0.1:8428/api/v1/query --data-urlencode 'query=$q' --data-urlencode time=$at" \
            | grep -oE '"value":\[[^]]+\]' | grep -oE '"[0-9]+"' | tr -d '"')
        printf '%-34s %s   ' "${q:0:34}" "${v:-없음}"
      done
      echo
    done
    echo "기준 시각 $(date -d "@$at" '+%F %T') (epoch $at), 구간 $win"
    ;;
  queue)
    for n in A B; do
      echo "== $n"
      on "$n" 'for p in 8429 9429 10429; do curl -s http://127.0.0.1:$p/metrics | grep -E "^(vmagent|vlagent|vtagent)_remotewrite_pending_data_bytes" ; done' \
        | sed -E 's/\{.*url="([^"]+)".*\} /  \1 /' || true
    done
    ;;
  stop|start)
    n=$1; shift
    on "$n" "sudo systemctl $cmd $*"
    on "$n" "for s in $*; do printf '%-18s %s\n' \$s \$(systemctl is-active \$s); done"
    ;;
  cut)
    n=$1; mins=${2:-5}; peer=$(peer_of "$n")
    # 자기 nftables 표 하나만 만들고, systemd-run 으로 지정한 분 뒤에 그 표를 지운다 (잊어도 풀린다)
    on "$n" "sudo nft add table inet mon2poc && \
             sudo nft add chain inet mon2poc block '{ type filter hook input priority -10; }' && \
             sudo nft add chain inet mon2poc blockout '{ type filter hook output priority -10; }' && \
             sudo nft add rule inet mon2poc block ip saddr $peer drop && \
             sudo nft add rule inet mon2poc blockout ip daddr $peer drop && \
             sudo systemd-run --on-active=${mins}m --unit=mon2poc-heal nft delete table inet mon2poc && \
             echo '차단 시작: $peer, ${mins}분 뒤 자동 해제'"
    ;;
  heal)
    n=$1
    on "$n" "sudo nft delete table inet mon2poc 2>/dev/null; sudo systemctl stop mon2poc-heal.timer 2>/dev/null; sudo nft list tables"
    ;;
  *)
    sed -n '2,10p' "$0"; exit 1 ;;
esac

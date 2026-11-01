#!/usr/bin/env bash
# 단일 → 이중화로 바꾼 뒤, 새 노드에 없는 과거 메트릭을 기존 노드에서 복사한다.
# 사용: bin/backfill-metrics.sh <원본 A|B> <시작 RFC3339> [끝 RFC3339]
#   예: bin/backfill-metrics.sh A 2026-11-01T00:00:00Z
# 대상 노드에서 vmctl 이 원본 노드의 :7443/vm 으로 export 를 받아 자기 127.0.0.1:8428 에 넣는다.
# 로그 · 트레이스는 아직 이 스크립트에 없다 (설계 문서 7.1 참고).
set -euo pipefail
cd "$(dirname "$0")/.."
. ./inventory.env
NAME=${NAME:-cmp-mon}
BIN_DIR=${BASE_DIR:-/$NAME}/bin
src=${1:?원본 노드 A|B}; start=${2:?시작 시각}; end=${3:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
case "$src" in A) from=$NODE_A; to=$NODE_B ;; B) from=$NODE_B; to=$NODE_A ;; *) exit 1 ;; esac
[ -n "${NODE_B:-}" ] || { echo "이중화 상태가 아니다 (NODE_B 비어 있음)" >&2; exit 1; }
ssh_opts=(-n -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
[ -n "${JUMP:-}" ] && ssh_opts+=(-J "$JUMP")
timeout 3600 ssh "${ssh_opts[@]}" "$SSH_USER@$to" \
  "$BIN_DIR/vmctl-prod vm-native -s \
     --vm-native-src-addr=http://$from:7443/vm \
     --vm-native-dst-addr=http://127.0.0.1:8428 \
     --vm-native-filter-match='{__name__!=\"\"}' \
     --vm-native-filter-time-start=$start --vm-native-filter-time-end=$end"

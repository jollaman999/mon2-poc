#!/usr/bin/env bash
# 중앙 노드 A · B 의 서비스 상태, 포트 응답, SELinux 거부(AVC)를 본다.
# 사용: checks/central-health.sh [분]   (AVC 를 몇 분 전부터 볼지, 기본 30)
set -euo pipefail
cd "$(dirname "$0")/.."
. ./inventory.env
since=${1:-30}

ssh_opts=(-i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10)
[ -n "${JUMP:-}" ] && ssh_opts+=(-J "$JUMP")

check_one() { # self peer
  local self=$1 peer=$2
  echo "===== $self"
  timeout 120 ssh "${ssh_opts[@]}" "$SSH_USER@$self" "sudo SELF=$self PEER=$peer SINCE=$since bash -s" <<'EOF' || echo "접속 실패"
set +e
code() { timeout 5 curl -s -o /dev/null -w '%{http_code}' "$1"; }
for s in victoria-metrics victoria-logs victoria-traces vmagent vlagent vtagent vmalert alertmanager haproxy; do
  printf '%-18s %s\n' "$s" "$(systemctl is-active $s)"
done
echo "--- 로컬 부품 (127.0.0.1)"
for p in 8428 9428 10428 8429 9429 10429 8880; do printf ':%-6s %s\n' $p "$(code http://127.0.0.1:$p/health)"; done
printf ':9093   %s\n' "$(code http://127.0.0.1:9093/-/healthy)"
echo "--- HAProxy"
printf 'query  /metrics/health   %s\n' "$(code http://$SELF:8443/metrics/health)"
printf 'query  /logs/health      %s\n' "$(code http://$SELF:8443/logs/health)"
printf 'query  /traces/health    %s\n' "$(code http://$SELF:8443/traces/health)"
printf 'peer   %s:7443/vm/health %s\n' "$PEER" "$(code http://$PEER:7443/vm/health)"
printf 'peer   %s:7443/am        %s\n' "$PEER" "$(code http://$PEER:7443/am/-/healthy)"
echo "--- 바깥에서 듣는 포트"
ss -ltnH | awk '{print $4}' | grep -v -E '^(127\.0\.0\.1|\[::1\]):' | sort -u | tr '\n' ' '; echo
echo "--- SELinux"
getenforce
getsebool haproxy_connect_any
ps -eo label,comm | grep -E 'victoria|vmagent|vlagent|vtagent|vmalert|alertmanager|haproxy' | awk '{print $1, $2}' | sort -u
echo "--- 최근 ${SINCE}분 AVC"
ausearch -m avc -ts $(date -d "-${SINCE} min" '+%m/%d/%Y %H:%M:%S') 2>/dev/null \
  | grep -oE 'denied  \{[^}]+\}|comm="[^"]+"|dest=[0-9]+|tcontext=[^ ]+' | paste -sd' ' | head -c 3000
echo
EOF
}
check_one "$NODE_A" "$NODE_B"
check_one "$NODE_B" "$NODE_A"

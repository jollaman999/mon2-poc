#!/usr/bin/env bash
# 중앙 노드 A · B 의 서비스 상태, 포트 응답, SELinux 거부(AVC)를 본다.
# 사용: checks/central-health.sh [분]   (AVC 를 몇 분 전부터 볼지, 기본 30)
set -euo pipefail
cd "$(dirname "$0")/.."
. ./inventory.env
since=${1:-30}

ssh_opts=(-n -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10)
[ -n "${JUMP:-}" ] && ssh_opts+=(-J "$JUMP")

for node in "$NODE_A" "$NODE_B"; do
  echo "===== $node"
  timeout 120 ssh "${ssh_opts[@]}" "$SSH_USER@$node" "sudo bash -s" <<EOF || echo "접속 실패"
set +e
for s in victoria-metrics victoria-logs victoria-traces vmagent vlagent vtagent vmalert alertmanager haproxy; do
  printf '%-18s %s\n' "\$s" "\$(systemctl is-active \$s)"
done
echo "--- /health"
for p in 8428 9428 10428 8429 9429 10429 8880 18428 19428 20428; do
  printf ':%-6s %s\n' \$p "\$(timeout 5 curl -s -o /dev/null -w '%{http_code}' http://$node:\$p/health)"
done
printf ':9093   %s\n' "\$(timeout 5 curl -s -o /dev/null -w '%{http_code}' http://$node:9093/-/healthy)"
echo "--- SELinux"
getenforce
getsebool haproxy_connect_any
ps -eZ | grep -E 'victoria|vmagent|vlagent|vtagent|vmalert|alertmanager|haproxy' | awk '{print \$1, \$NF}' | sort -u
echo "--- 최근 ${since}분 AVC"
ausearch -m avc -ts \$(date -d "-${since} min" '+%m/%d/%Y %H:%M:%S') 2>/dev/null | grep -oE 'comm="[^"]+"|denied  \{[^}]+\}|tcontext=[^ ]+|dest=[0-9]+' | paste -sd' ' | head -c 2000 || true
echo
EOF
done

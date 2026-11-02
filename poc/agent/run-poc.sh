#!/usr/bin/env bash
# 시험용으로 에이전트를 한 호스트에 띄우고 내린다. 서비스로 설치하지 않는다 (SSH 계정의 홈 아래 ~/cmp-agent).
# 사용: agent/run-poc.sh start <호스트> [시작 주소를 고를 key]   |  stop <호스트>  |  log <호스트>
#   INTERVAL(기본 1s) · FAILBACK(기본 1m) 는 환경 변수로 바꾼다
set -euo pipefail
cd "$(dirname "$0")"
. ../inventory.env
cmd=${1:?start|stop|log}; host=${2:?호스트}; key=${3:-}
ssh_opts=(-i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -J "$JUMP")
on() { timeout 60 ssh -n "${ssh_opts[@]}" "$SSH_USER@$host" "$@"; }
eps="\"https://$NODE_A:4318\""
[ -n "${NODE_B:-}" ] && eps="$eps, \"https://$NODE_B:4318\""
case $cmd in
  start)
    sed -e "s|@QUEUE_DIR@|/home/$SSH_USER/cmp-agent/queue|" -e "s|@INTERVAL@|${INTERVAL:-1s}|" \
        -e "s|@ENDPOINTS@|$eps|" -e "s|@START_KEY@|$key|" -e "s|@FAILBACK@|${FAILBACK:-1m}|" \
        config.yaml.tmpl > _build/config.yaml
    on "pkill -f '[c]mp-agent/cmp-otelcol' || true; mkdir -p ~/cmp-agent"
    timeout 120 ssh "${ssh_opts[@]}" "$SSH_USER@$host" "cat > ~/cmp-agent/cmp-otelcol && chmod +x ~/cmp-agent/cmp-otelcol" < _build/cmp-otelcol
    timeout 60 ssh "${ssh_opts[@]}" "$SSH_USER@$host" "cat > ~/cmp-agent/config.yaml" < _build/config.yaml
    on "cd ~/cmp-agent && setsid ./cmp-otelcol --config config.yaml </dev/null >>agent.log 2>&1 & sleep 3; pgrep -a cmp-otelcol; grep -E 'cmpotlp 시작|error' agent.log | tail -4"
    ;;
  stop) on "pkill -f '[.]/cmp-otelcol --config' && echo 멈춤 || echo 안 돌고 있음" ;;
  log)  on "tail -${LINES:-20} ~/cmp-agent/agent.log" ;;
esac

#!/usr/bin/env bash
# 중앙 노드 A · B 에 central/install.sh 를 돌린다.
# 사용: bin/deploy-central.sh [A|B|all]   (기본 all)
# 미리: cp inventory.example.env inventory.env 후 값 채우기, bin/fetch.sh
set -euo pipefail
cd "$(dirname "$0")/.."
. ./inventory.env
. ./versions.env
target=${1:-all}

ssh_opts=(-i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
[ -n "${JUMP:-}" ] && ssh_opts+=(-J "$JUMP")

deploy_one() { # self peer
  local self=$1 peer=$2 bundle
  bundle=$(mktemp -d)
  cp -r central/conf central/systemd central/install.sh versions.env "$bundle/"
  mkdir -p "$bundle/dist"
  cp dist/*.tar.gz "dist/$HAPROXY_RPM" "$bundle/dist/"
  cat > "$bundle/node.env" <<EOF
SELF=$self
PEER=$peer
NODE_A=$NODE_A
NODE_B=$NODE_B
RETENTION=$RETENTION
QUEUE_MAX=$QUEUE_MAX
WEBHOOK=${WEBHOOK:-$self:5001}
EOF
  echo "=== $self: 올리는 중"
  tar czf - -C "$bundle" . | timeout 600 ssh "${ssh_opts[@]}" "$SSH_USER@$self" \
    'rm -rf /tmp/mon2-bundle && mkdir -p /tmp/mon2-bundle && tar xzf - -C /tmp/mon2-bundle'
  echo "=== $self: 설치"
  timeout 900 ssh -n "${ssh_opts[@]}" "$SSH_USER@$self" \
    "sudo HAPROXY_SELINUX=${HAPROXY_SELINUX:-boolean} bash /tmp/mon2-bundle/install.sh && rm -rf /tmp/mon2-bundle"
  rm -rf "$bundle"
}

case "$target" in
  A)   deploy_one "$NODE_A" "$NODE_B" ;;
  B)   deploy_one "$NODE_B" "$NODE_A" ;;
  all) deploy_one "$NODE_A" "$NODE_B"; deploy_one "$NODE_B" "$NODE_A" ;;
  *)   echo "사용: $0 [A|B|all]" >&2; exit 1 ;;
esac

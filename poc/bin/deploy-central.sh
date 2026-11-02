#!/usr/bin/env bash
# 중앙 노드 A · B 에 central/install.sh 를 돌린다.
# 사용: bin/deploy-central.sh [A|B|all]           설치 (기본 all)
#       bin/deploy-central.sh uninstall [A|B|all] 삭제 (데이터 포함)
# 미리: cp inventory.example.env inventory.env 후 값 채우기, bin/fetch.sh
#
# inventory.env 의 NODE_B 를 비우면 단일 노드, 채우면 이중화로 설치한다.
#   단일 → 이중화: NODE_B 를 채우고 all 로 다시 돌린 뒤 bin/backfill-metrics.sh 로 과거 메트릭을 채운다
#   이중화 → 단일: NODE_B 를 비우고 A 로 다시 돌린 뒤, 노드 B 의 서비스를 내린다
set -euo pipefail
cd "$(dirname "$0")/.."
. ./inventory.env
. ./versions.env
action=install
[ "${1:-}" = uninstall ] && { action=uninstall; shift; }
target=${1:-all}
NAME=${NAME:-cmp-mon}

ssh_opts=(-i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
[ -n "${JUMP:-}" ] && ssh_opts+=(-J "$JUMP")

# 인증서 · 토큰: 처음 한 번 secrets/ 에 만들고 두 노드에 같은 것을 넣는다 (secrets/ 는 커밋하지 않는다)
# 서버 인증서는 사설 CA 로 서명하고 SAN 에 두 노드 IP 를 넣는다. 노드 IP 가 바뀌면 지우고 다시 만든다.
need_cert() { # 서버 인증서가 없거나 SAN 에 노드 IP 가 빠졌으면 참
  [ -s secrets/server.pem ] || return 0
  local got ip
  got=$(openssl x509 -in secrets/server.pem -noout -ext subjectAltName 2>/dev/null | tr -d ' \n')
  for ip in $NODE_A ${NODE_B:-}; do echo "$got" | grep -q "IPAddress:$ip" || return 0; done
  return 1
}
make_secrets() {
  mkdir -p secrets; chmod 700 secrets
  [ -s secrets/peer.token ]  || openssl rand -hex 32 > secrets/peer.token
  [ -s secrets/query.token ] || openssl rand -hex 32 > secrets/query.token
  local san="IP:$NODE_A"; [ -n "${NODE_B:-}" ] && san="$san,IP:$NODE_B"
  if need_cert; then
    [ -s secrets/ca.pem ] || openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=$NAME-ca" \
        -keyout secrets/ca.key -out secrets/ca.pem 2>/dev/null
    openssl req -newkey rsa:2048 -nodes -subj "/CN=$NAME" -keyout secrets/server.key -out secrets/server.csr 2>/dev/null
    printf 'subjectAltName=%s\nbasicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' "$san" > secrets/server.ext
    openssl x509 -req -in secrets/server.csr -CA secrets/ca.pem -CAkey secrets/ca.key -CAcreateserial \
        -days 3650 -extfile secrets/server.ext -out secrets/server.pem 2>/dev/null
    rm -f secrets/server.csr secrets/server.ext
    echo "서버 인증서를 만들었다 (SAN $san)"
  fi
  chmod 600 secrets/*
}

deploy_one() { # self peer
  local self=$1 peer=$2 bundle
  bundle=$(mktemp -d)
  cp -r central/conf central/systemd central/install.sh central/gen-gateway.sh central/reconcile-metrics.sh versions.env "$bundle/"
  mkdir -p "$bundle/secrets"
  cp secrets/ca.pem secrets/server.pem secrets/server.key secrets/peer.token secrets/query.token "$bundle/secrets/"
  mkdir -p "$bundle/dist"
  cp dist/*.tar.gz "$bundle/dist/"
  cat > "$bundle/node.env" <<EOF
SELF=$self
PEER=$peer
NAME=$NAME
SERVICE_USER=${SERVICE_USER:-}
BASE_DIR=${BASE_DIR:-}
RETENTION=$RETENTION
QUEUE_MAX=$QUEUE_MAX
WEBHOOK=${WEBHOOK:-$self:5001}
TLS_VERIFY=${TLS_VERIFY:-skip}
EOF
  echo "=== $self: 올리는 중"
  tar czf - -C "$bundle" . | timeout 600 ssh "${ssh_opts[@]}" "$SSH_USER@$self" \
    "rm -rf /tmp/$NAME-bundle && mkdir -p /tmp/$NAME-bundle && tar xzf - -C /tmp/$NAME-bundle"
  echo "=== $self: 설치"
  timeout 900 ssh -n "${ssh_opts[@]}" "$SSH_USER@$self" \
    "sudo bash /tmp/$NAME-bundle/install.sh && rm -rf /tmp/$NAME-bundle"
  rm -rf "$bundle"
}

uninstall_one() { # node
  echo "=== $1: 삭제"
  timeout 600 ssh -n "${ssh_opts[@]}" "$SSH_USER@$1" "sudo bash -s ${BASE_DIR:-/$NAME}" < central/uninstall.sh
}

if [ "$action" = uninstall ]; then
  case "$target" in
    A)   uninstall_one "$NODE_A" ;;
    B)   uninstall_one "$NODE_B" ;;
    all) uninstall_one "$NODE_A"; [ -n "${NODE_B:-}" ] && uninstall_one "$NODE_B" ;;
  esac
  exit 0
fi

make_secrets
case "$target" in
  A)   deploy_one "$NODE_A" "${NODE_B:-}" ;;
  B)   [ -n "${NODE_B:-}" ] || { echo "NODE_B 가 비어 있다 (단일 노드)" >&2; exit 1; }
       deploy_one "$NODE_B" "$NODE_A" ;;
  all) if [ -n "${NODE_B:-}" ]; then deploy_one "$NODE_A" "$NODE_B"; deploy_one "$NODE_B" "$NODE_A"
       else deploy_one "$NODE_A" ""; fi ;;
  *)   echo "사용: $0 [A|B|all]" >&2; exit 1 ;;
esac

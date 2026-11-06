#!/usr/bin/env bash
# 중앙 노드 하나에 Victoria 3종 · 중계 3종 · vmalert · Alertmanager · vmauth 입구를 컨테이너로 띄운다 (Podman + podman-compose).
# deploy-central.sh 가 묶음(bundle)을 올린 뒤 그 안에서 root 로 실행한다. 인터넷에 나가지 않는다.
#
# node.env 로 받는 값
#   SELF PEER                 PEER 가 비어 있으면 단일 노드
#   NAME                      컨테이너 이름 앞말 · 디렉터리 이름 (기본 cmp-mon)
#   SERVICE_USER              컨테이너 안 실행 계정(uid · gid)으로 쓴다. 이미 있으면 그대로, 없으면 시스템 계정으로 만든다 (기본 NAME)
#   BASE_DIR                  모든 것을 두는 곳 (기본 /NAME). 아래에 compose · conf · data · logs 를 만든다
#   RETENTION QUEUE_MAX WEBHOOK
#   TLS_VERIFY                상대 노드 HTTPS 인증서를 skip(기본, 검증 안 함) / ca(묶음의 사설 CA 로 검증)
#   AM_LOG_LEVEL              Alertmanager 로그 수준 (기본 info, 원인 추적 때 debug)
# 묶음 안
#   podman-el10/rpms · whl    Podman RPM(EL10) 과 podman-compose wheel (bin/fetch-podman.sh)
#   images/*.tar              공식 이미지 (bin/fetch-images.sh)
#   secrets/                  deploy-central.sh 가 만든 인증서(ca.pem, server.pem, server.key)와 토큰(peer.token, query.token)
set -euo pipefail
cd "$(dirname "$0")"
. ./versions.env
. ./node.env
NAME=${NAME:-cmp-mon}
SERVICE_USER=${SERVICE_USER:-$NAME}
BASE_DIR=${BASE_DIR:-/$NAME}
COMPOSE_DIR=$BASE_DIR/compose
CONF_DIR=$BASE_DIR/conf
DATA_DIR=$BASE_DIR/data
LOG_DIR=$BASE_DIR/logs
if [ -n "${PEER:-}" ]; then MODE=ha; else MODE=single; fi
TLS_VERIFY=${TLS_VERIFY:-skip}
PC=/usr/local/bin/podman-compose

[ "$(id -u)" = 0 ] || { echo "root 로 실행해야 한다" >&2; exit 1; }
echo "모드 $MODE · 이름 $NAME · 계정 $SERVICE_USER · 위치 $BASE_DIR"

echo "[1/8] Podman"
# 저장소를 모두 끄고 묶음의 rpms/ 만 저장소로 쓴다. dnf install 은 이미 깔린 패키지를 올리지 않고,
# 새로 까는 패키지가 더 높은 판을 요구할 때만 그 의존성을 올린다. 서명은 배포판 키로 확인한다
todo=()
while read -r p; do rpm -q "$p" >/dev/null 2>&1 || todo+=("$p"); done < podman-el10/packages.txt
if [ ${#todo[@]} -eq 0 ]; then
  echo "묶음의 패키지가 모두 깔려 있다: $(rpm -q podman)"
else
  echo "설치할 패키지 ${#todo[@]}개: ${todo[*]}"
  repo=$NAME-podman
  dnf install -y -q --disablerepo='*' --repofrompath="$repo,$PWD/podman-el10/rpms" --enablerepo="$repo" \
    --setopt="$repo.gpgcheck=1" --setopt="$repo.gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-10" "${todo[@]}"
  rpm -q podman container-selinux
fi

echo "[2/8] podman-compose"
if [ -x "$PC" ]; then
  echo "있음: $("$PC" --version 2>/dev/null | tail -1)"
else
  # pip 이 없는 서버가 많아 묶음의 pip wheel 을 그대로 실행해 설치한다. PyYAML 은 RPM(python3-pyyaml)으로 들어간다
  pip_whl=$(ls podman-el10/whl/pip-*.whl)
  python3 "$pip_whl/pip" install -q --no-index --find-links podman-el10/whl --root-user-action=ignore \
    --disable-pip-version-check --prefix /usr/local podman-compose
  "$PC" --version | tail -1
fi

echo "[3/8] 계정 · 디렉터리"
if id "$SERVICE_USER" >/dev/null 2>&1; then
  echo "계정 $SERVICE_USER 이 이미 있어 그대로 쓴다"
  created_user=$( [ -f "$CONF_DIR/install.env" ] && sed -n 's/^CREATED_USER=//p' "$CONF_DIR/install.env" || true )
  # systemd 설치(이전 방식)가 계정을 만들었으면 conf/.created-user 를 남겼다
  [ -f "$CONF_DIR/.created-user" ] && created_user=1
else
  useradd --system --no-create-home --shell /sbin/nologin "$SERVICE_USER"
  created_user=1     # 지울 때(--purge) 우리가 만든 계정만 지운다
  echo "계정 $SERVICE_USER 을 만들었다"
fi
SVC_UID=$(id -u "$SERVICE_USER"); SVC_GID=$(id -g "$SERVICE_USER")
install -d -m 0755 "$BASE_DIR"
install -d -m 0750 "$COMPOSE_DIR"
install -d -m 0750 -o root -g "$SVC_GID" "$CONF_DIR" "$CONF_DIR/rules" "$CONF_DIR/tls" "$CONF_DIR/tokens"
install -d -m 0750 -o "$SVC_UID" -g "$SVC_GID" "$DATA_DIR" "$LOG_DIR"
# 컨테이너가 서비스 계정 uid 로 쓰므로 데이터 폴더 소유를 맞춘다 (앞선 설치가 남긴 것도 같은 계정이면 그대로다)
for d in victoria-metrics victoria-logs victoria-traces vmagent-queue vlagent-queue vtagent-queue alertmanager; do
  install -d -m 0750 -o "$SVC_UID" -g "$SVC_GID" "$DATA_DIR/$d"
done
chown -R "$SVC_UID:$SVC_GID" "$DATA_DIR"

echo "[4/8] 이미지"
( cd images && sha256sum -c --quiet SHA256SUMS )
while read -r _ f; do podman load -q -i "images/$f" | sed 's/^/  /'; done < images/SHA256SUMS

echo "[5/8] 설정 · 인증서 · 토큰"
# 인증서 · 토큰은 root 와 서비스 계정 그룹만 읽는다
install -m 0644 -o root -g "$SVC_GID" secrets/ca.pem secrets/server.pem "$CONF_DIR/tls/"
install -m 0640 -o root -g "$SVC_GID" secrets/server.key "$CONF_DIR/tls/"
install -m 0640 -o root -g "$SVC_GID" secrets/peer.token secrets/query.token "$CONF_DIR/tokens/"
TLSD=$CONF_DIR/tls; TOKD=$CONF_DIR/tokens
# 단일 · 이중화 · 인증서 검증 방식에 따라 달라지는 인자를 부품마다 env/<부품>.env 에 쓴다. compose.yaml 은 그대로 둔다.
# Victoria 부품은 -envflag.enable 로 읽는다: 이름의 . 을 _ 로 바꾸고, 배열 인자는 목적지 순서대로 쉼표로 잇는다
case "$TLS_VERIFY" in
  skip) PEER_TLS="tlsInsecureSkipVerify=false,true"; BACKEND_TLS="backend_tlsInsecureSkipVerify=true"
        SELF_TLS="tlsInsecureSkipVerify=true" ;;
  ca)   PEER_TLS="tlsCAFile=,$TLSD/ca.pem"; BACKEND_TLS="backend_TLSCAFile=$TLSD/ca.pem"
        SELF_TLS="tlsCAFile=$TLSD/ca.pem" ;;
  *)    echo "TLS_VERIFY 는 skip 또는 ca: $TLS_VERIFY" >&2; exit 1 ;;
esac
umask 027
install -d -m 0750 "$COMPOSE_DIR/env"
relay_env() { # 로컬 URL, 상대 노드 경로 → 중계 목적지 1 = 로컬(http), 2 = 상대 노드(https + 노드 사이 토큰)
  if [ "$MODE" = ha ]; then
    echo "remoteWrite_url=$1,https://$PEER:7443$2"
    echo "remoteWrite_bearerTokenFile=,$TOKD/peer.token"
    echo "remoteWrite_$PEER_TLS"
  else
    echo "remoteWrite_url=$1"
  fi
}
relay_env http://127.0.0.1:8428/api/v1/write /vm/api/v1/write > "$COMPOSE_DIR/env/vmagent.env"
relay_env http://127.0.0.1:9428/insert/native /vl/insert/native > "$COMPOSE_DIR/env/vlagent.env"
relay_env http://127.0.0.1:10428/insert/native /vt/insert/native > "$COMPOSE_DIR/env/vtagent.env"
{ # vmalert: 조회 · 기록은 자기 입구(HTTPS), 알림은 1 = 로컬 Alertmanager, 2 = 상대 노드 :7443/am
  echo "datasource_$SELF_TLS"; echo "remoteRead_$SELF_TLS"; echo "remoteWrite_$SELF_TLS"
  if [ "$MODE" = ha ]; then
    echo "notifier_url=http://127.0.0.1:9093,https://$PEER:7443/am"
    echo "notifier_bearerTokenFile=,$TOKD/peer.token"
    echo "notifier_$PEER_TLS"
  else
    echo "notifier_url=http://127.0.0.1:9093"
  fi
} > "$COMPOSE_DIR/env/vmalert.env"
{ # vmauth: 수집 4318 · 조회 8443 (+ 노드 사이 7443), 모두 HTTPS
  if [ "$MODE" = ha ]; then
    echo "httpListenAddr=$SELF:4318,$SELF:8443,$SELF:7443"
    echo "tls=true,true,true"
    echo "tlsCertFile=$TLSD/server.pem,$TLSD/server.pem,$TLSD/server.pem"
    echo "tlsKeyFile=$TLSD/server.key,$TLSD/server.key,$TLSD/server.key"
    echo "$BACKEND_TLS"
  else
    echo "httpListenAddr=$SELF:4318,$SELF:8443"
    echo "tls=true,true"
    echo "tlsCertFile=$TLSD/server.pem,$TLSD/server.pem"
    echo "tlsKeyFile=$TLSD/server.key,$TLSD/server.key"
  fi
} > "$COMPOSE_DIR/env/gateway.env"
# Alertmanager 는 환경 변수 인자가 없어 컨테이너 안 sh 가 이 값을 낱말로 나눠 붙인다
if [ "$MODE" = ha ]; then
  echo "AM_CLUSTER_ARGS=--cluster.listen-address=$SELF:9094 --cluster.peer=$PEER:9094"
else
  echo "AM_CLUSTER_ARGS=--cluster.listen-address="
fi > "$COMPOSE_DIR/env/alertmanager.env"
# compose.yaml 에 넣을 값과 주기 대조 컨테이너의 환경 변수
cat > "$COMPOSE_DIR/.env" <<ENV
COMPOSE_PROJECT_NAME=$NAME
NAME=$NAME
BASE_DIR=$BASE_DIR
SVC_UID=$SVC_UID
SVC_GID=$SVC_GID
VM_VERSION=$VM_VERSION
VL_VERSION=$VL_VERSION
VT_VERSION=$VT_VERSION
AM_VERSION=$AM_VERSION
AM_LOG_LEVEL=${AM_LOG_LEVEL:-info}
MODE=$MODE
SELF=$SELF
PEER=${PEER:-}
TLS_VERIFY=$TLS_VERIFY
RETENTION=$RETENTION
QUEUE_MAX=$QUEUE_MAX
RECONCILE_EVERY=300
ENV
install -m 0640 compose/compose.yaml "$COMPOSE_DIR/compose.yaml"
install -m 0644 reconcile-metrics.sh "$COMPOSE_DIR/reconcile-metrics.sh"
sed "s|@WEBHOOK@|$WEBHOOK|" conf/alertmanager.yml > "$CONF_DIR/alertmanager.yml"
install -m 0644 conf/rules/*.yml "$CONF_DIR/rules/"
sed "s|@SELF@|$SELF|" conf/scrape.yml > "$CONF_DIR/scrape.yml"
./gen-gateway.sh "$CONF_DIR/gateway.yml" "${PEER:-}" "$(cat secrets/peer.token)" "$(cat secrets/query.token)"
# 서비스 계정이 설정을 읽을 수 있게 그룹을 맞춘다 (쓰기는 root 만)
chgrp -R "$SVC_GID" "$CONF_DIR"; chmod -R g+rX,o-rwx "$CONF_DIR"
# 지울 때 쓰려고 설치 정보를 남긴다
restart_enabled=$( [ -f "$CONF_DIR/install.env" ] && sed -n 's/^ENABLED_PODMAN_RESTART=//p' "$CONF_DIR/install.env" || true )
if [ -z "$restart_enabled" ]; then
  if systemctl is-enabled --quiet podman-restart.service 2>/dev/null; then restart_enabled=0; else restart_enabled=1; fi
fi
cat > "$CONF_DIR/install.env" <<ENV
NAME=$NAME
SERVICE_USER=$SERVICE_USER
BASE_DIR=$BASE_DIR
CREATED_USER=${created_user:-0}
ENABLED_PODMAN_RESTART=$restart_enabled
VM_VERSION=$VM_VERSION
VL_VERSION=$VL_VERSION
VT_VERSION=$VT_VERSION
AM_VERSION=$AM_VERSION
ENV

echo "[6/8] 컨테이너"
cd "$COMPOSE_DIR"
# 이중화 → 단일이면 주기 대조 컨테이너도 내려야 하므로 늘 ha 프로필까지 넣어 내린다
"$PC" --profile ha down >/dev/null 2>&1 || true
# 이중화에서 단일로 바꾸면 상대 노드로 보내던 큐가 남는다. 중계의 목적지 순서는 늘 1 = 로컬, 2 = 상대 노드라서
# 큐 디렉터리 이름이 2_ 로 시작하는 것이 상대 노드 몫이다. 단일 노드에서는 보낼 곳이 없으므로 지운다
if [ "$MODE" = single ]; then
  for q in vmagent-queue vlagent-queue vtagent-queue; do
    for d in "$DATA_DIR/$q"/persistent-queue/2_*; do
      [ -d "$d" ] && { echo "상대 노드 몫 큐 삭제: $d"; rm -rf "$d"; }
    done
  done
fi
profile=(); [ "$MODE" = ha ] && profile=(--profile ha)
# podman-compose 는 돌린 podman 명령을 다 찍으므로 파일로 남기고 실패할 때만 보여 준다
if ! "$PC" "${profile[@]}" up -d > "$LOG_DIR/compose-up.log" 2>&1; then
  tail -30 "$LOG_DIR/compose-up.log" >&2; exit 1
fi
podman ps -a --filter "name=^$NAME-" --format '  {{.Names}} {{.Status}}'

echo "[7/8] 재부팅 뒤 자동 시작"
# restart: always 인 컨테이너는 podman 패키지의 podman-restart.service 가 부팅 때 다시 띄운다
systemctl enable podman-restart.service >/dev/null 2>&1
systemctl is-enabled podman-restart.service

echo "[8/8] 방화벽"
if systemctl is-active --quiet firewalld; then
  ports="4318 8443"; [ "$MODE" = ha ] && ports="$ports 7443 9094"
  for p in $ports; do firewall-cmd --permanent --add-port="$p/tcp" >/dev/null; done
  [ "$MODE" = ha ] && firewall-cmd --permanent --add-port=9094/udp >/dev/null
  firewall-cmd --reload
else
  echo "firewalld 꺼짐"
fi
echo "설치 끝: $SELF ($MODE, 상대 ${PEER:-없음})"

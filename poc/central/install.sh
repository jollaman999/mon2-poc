#!/usr/bin/env bash
# 중앙 노드 하나에 Victoria 3종 · 중계 3종 · vmalert · Alertmanager · vmauth 입구 3개를 설치한다.
# deploy-central.sh 가 묶음(bundle)을 올린 뒤 그 안에서 root 로 실행한다. 인터넷에 나가지 않는다.
#
# node.env 로 받는 값
#   SELF PEER                 PEER 가 비어 있으면 단일 노드
#   NAME                      서비스 · 디렉터리 이름 (기본 cmp-mon)
#   SERVICE_USER              실행 계정. 이미 있으면 그대로 쓰고, 없으면 시스템 계정으로 만든다 (기본 NAME)
#   BASE_DIR                  모든 것을 두는 곳 (기본 /NAME). 아래에 bin · conf · data · logs 를 만든다
#   RETENTION QUEUE_MAX WEBHOOK
#   TLS_VERIFY                상대 노드 HTTPS 인증서를 skip(기본, 검증 안 함) / ca(묶음의 사설 CA 로 검증)
# 묶음의 secrets/ 에 deploy-central.sh 가 만든 인증서(ca.pem, server.pem, server.key)와 토큰(peer.token, query.token)이 있다.
# 모든 부품이 정적 바이너리라 패키지를 설치하지 않는다.
set -euo pipefail
cd "$(dirname "$0")"
. ./versions.env
. ./node.env
NAME=${NAME:-cmp-mon}
SERVICE_USER=${SERVICE_USER:-$NAME}
BASE_DIR=${BASE_DIR:-/$NAME}
BIN_DIR=$BASE_DIR/bin
CONF_DIR=$BASE_DIR/conf
DATA_DIR=$BASE_DIR/data
LOG_DIR=$BASE_DIR/logs
if [ -n "${PEER:-}" ]; then MODE=ha; else MODE=single; fi
TLS_VERIFY=${TLS_VERIFY:-skip}
SERVICES="victoria-metrics victoria-logs victoria-traces vmagent vlagent vtagent alertmanager gateway vmalert"

[ "$(id -u)" = 0 ] || { echo "root 로 실행해야 한다" >&2; exit 1; }
echo "모드 $MODE · 이름 $NAME · 계정 $SERVICE_USER · 위치 $BASE_DIR"

echo "[1/7] 계정 · 디렉터리"
install -d -m 0755 "$BASE_DIR" "$CONF_DIR" "$CONF_DIR/rules" "$BIN_DIR"
if id "$SERVICE_USER" >/dev/null 2>&1; then
  echo "계정 $SERVICE_USER 이 이미 있어 그대로 쓴다"
else
  useradd --system --no-create-home --shell /sbin/nologin "$SERVICE_USER"
  touch "$CONF_DIR/.created-user"     # 지울 때 우리가 만든 계정만 지우려는 표시
  echo "계정 $SERVICE_USER 을 만들었다"
fi
SERVICE_GROUP=$(id -gn "$SERVICE_USER")
install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$DATA_DIR" "$LOG_DIR"

echo "[2/7] 바이너리"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for f in dist/*.tar.gz; do tar xzf "$f" -C "$tmp"; done
for b in victoria-metrics-prod vmagent-prod vmalert-prod vmauth-prod vmctl-prod vmbackup-prod \
         victoria-logs-prod vlagent-prod victoria-traces-prod vtagent-prod; do
  install -m 0755 "$tmp/$b" "$BIN_DIR/"
done
install -m 0755 "$tmp"/alertmanager-*/alertmanager "$tmp"/alertmanager-*/amtool "$BIN_DIR/"

echo "[3/7] SELinux 라벨"
# semanage(policycoreutils-python-utils)는 없는 서버가 있다. 그래서 라벨 규칙을 CIL 정책 모듈로 만들어
# 늘 있는 semodule 로 넣는다. 재라벨링해도 유지되고, 지울 때 semodule -r 로 뺀다.
#   bin  bin_t     → systemd 가 unconfined_service_t 로 띄운다
#   conf etc_t · data var_lib_t · logs var_log_t, 기준 폴더 자체는 usr_t
SELINUX_MODULE=$(echo "${NAME}_files" | tr -c 'A-Za-z0-9_\n' '_')
if command -v selinuxenabled >/dev/null && selinuxenabled; then
  ctx() { echo "(system_u object_r $1 ((s0) (s0)))"; }
  cat > "$tmp/$SELINUX_MODULE.cil" <<CIL
(filecon "$BASE_DIR" dir $(ctx usr_t))
(filecon "$BIN_DIR(/.*)?" any $(ctx bin_t))
(filecon "$CONF_DIR(/.*)?" any $(ctx etc_t))
(filecon "$DATA_DIR(/.*)?" any $(ctx var_lib_t))
(filecon "$LOG_DIR(/.*)?" any $(ctx var_log_t))
CIL
  semodule -i "$tmp/$SELINUX_MODULE.cil"
  restorecon -R "$BASE_DIR"
  ls -dZ "$BASE_DIR" "$BIN_DIR/victoria-metrics-prod" "$CONF_DIR" "$DATA_DIR" "$LOG_DIR"
else
  echo "SELinux 꺼짐 또는 없음"
fi

echo "[4/7] 설정 · 인증서 · 토큰"
# 인증서 · 토큰은 root 와 서비스 계정만 읽는다
install -d -m 0750 -o root -g "$SERVICE_GROUP" "$CONF_DIR/tls" "$CONF_DIR/tokens"
install -m 0644 -o root -g "$SERVICE_GROUP" secrets/ca.pem secrets/server.pem "$CONF_DIR/tls/"
install -m 0640 -o root -g "$SERVICE_GROUP" secrets/server.key "$CONF_DIR/tls/"
install -m 0640 -o root -g "$SERVICE_GROUP" secrets/peer.token secrets/query.token "$CONF_DIR/tokens/"
TLSD=$CONF_DIR/tls; TOKD=$CONF_DIR/tokens
case "$TLS_VERIFY" in
  skip) PEER_TLS="-remoteWrite.tlsInsecureSkipVerify=false,true"
        NOTIFIER_TLS="-notifier.tlsInsecureSkipVerify=false,true"
        BACKEND_TLS="-backend.tlsInsecureSkipVerify"
        VMALERT_TLS_ARGS="-datasource.tlsInsecureSkipVerify -remoteRead.tlsInsecureSkipVerify -remoteWrite.tlsInsecureSkipVerify" ;;
  ca)   PEER_TLS="-remoteWrite.tlsCAFile=,$TLSD/ca.pem"
        NOTIFIER_TLS="-notifier.tlsCAFile=,$TLSD/ca.pem"
        BACKEND_TLS="-backend.TLSCAFile=$TLSD/ca.pem"
        VMALERT_TLS_ARGS="-datasource.tlsCAFile=$TLSD/ca.pem -remoteRead.tlsCAFile=$TLSD/ca.pem -remoteWrite.tlsCAFile=$TLSD/ca.pem" ;;
  *)    echo "TLS_VERIFY 는 skip 또는 ca: $TLS_VERIFY" >&2; exit 1 ;;
esac
# 단일 · 이중화에 따라 달라지는 인자. 유닛 파일은 그대로 두고 env 파일만 바뀐다
if [ "$MODE" = ha ]; then
  # 중계 목적지는 1 = 로컬(http), 2 = 상대 노드(https + 노드 사이 토큰). 배열 옵션은 목적지 순서대로 값을 준다
  PEER_TOKEN_ARGS="-remoteWrite.bearerTokenFile=,$TOKD/peer.token $PEER_TLS"
  VM_PEER_ARGS="-remoteWrite.url=https://$PEER:7443/vm/api/v1/write $PEER_TOKEN_ARGS"
  VL_PEER_ARGS="-remoteWrite.url=https://$PEER:7443/vl/insert/native $PEER_TOKEN_ARGS"
  VT_PEER_ARGS="-remoteWrite.url=https://$PEER:7443/vt/insert/native $PEER_TOKEN_ARGS"
  VMALERT_PEER_ARGS="-notifier.url=https://$PEER:7443/am -notifier.bearerTokenFile=,$TOKD/peer.token $NOTIFIER_TLS"
  AM_CLUSTER_ARGS="--cluster.listen-address=$SELF:9094 --cluster.peer=$PEER:9094"
  GW_ARGS="-httpListenAddr=$SELF:4318,$SELF:8443,$SELF:7443 -tls=true,true,true -tlsCertFile=$TLSD/server.pem,$TLSD/server.pem,$TLSD/server.pem -tlsKeyFile=$TLSD/server.key,$TLSD/server.key,$TLSD/server.key $BACKEND_TLS"
else
  VM_PEER_ARGS=""; VL_PEER_ARGS=""; VT_PEER_ARGS=""; VMALERT_PEER_ARGS=""
  AM_CLUSTER_ARGS="--cluster.listen-address="
  GW_ARGS="-httpListenAddr=$SELF:4318,$SELF:8443 -tls=true,true -tlsCertFile=$TLSD/server.pem,$TLSD/server.pem -tlsKeyFile=$TLSD/server.key,$TLSD/server.key"
fi
umask 027
cat > "$CONF_DIR/env" <<ENV
MODE=$MODE
SELF=$SELF
PEER=${PEER:-}
RETENTION=$RETENTION
QUEUE_MAX=$QUEUE_MAX
VM_PEER_ARGS=$VM_PEER_ARGS
VL_PEER_ARGS=$VL_PEER_ARGS
VT_PEER_ARGS=$VT_PEER_ARGS
VMALERT_PEER_ARGS=$VMALERT_PEER_ARGS
VMALERT_TLS_ARGS=$VMALERT_TLS_ARGS
AM_CLUSTER_ARGS=$AM_CLUSTER_ARGS
GW_ARGS=$GW_ARGS
ENV
# 지울 때 쓰려고 설치 위치를 남긴다
cat > "$CONF_DIR/install.env" <<ENV
NAME=$NAME
SERVICE_USER=$SERVICE_USER
BASE_DIR=$BASE_DIR
SELINUX_MODULE=$SELINUX_MODULE
ENV
sed "s|@WEBHOOK@|$WEBHOOK|" conf/alertmanager.yml > "$CONF_DIR/alertmanager.yml"
install -m 0644 conf/rules/*.yml "$CONF_DIR/rules/"
sed "s|@SELF@|$SELF|" conf/scrape.yml > "$CONF_DIR/scrape.yml"

# 서비스 계정이 설정을 읽을 수 있게 그룹을 맞춘다 (쓰기는 root 만)
chgrp -R "$SERVICE_GROUP" "$CONF_DIR"; chmod -R g+rX,o-rwx "$CONF_DIR"

echo "[5/7] 입구 (vmauth)"
./gen-gateway.sh "$CONF_DIR/gateway.yml" "${PEER:-}" "$(cat secrets/peer.token)" "$(cat secrets/query.token)"
chown root:"$SERVICE_GROUP" "$CONF_DIR/gateway.yml"; chmod 0640 "$CONF_DIR/gateway.yml"

# 이중화에서 단일로 바꾸면 상대 노드로 보내던 큐가 남는다. 중계의 목적지 순서는 늘 1 = 로컬, 2 = 상대 노드라서
# 큐 디렉터리 이름이 2_ 로 시작하는 것이 상대 노드 몫이다. 단일 노드에서는 보낼 곳이 없으므로 지운다
if [ "$MODE" = single ]; then
  systemctl stop "$NAME-vmagent" "$NAME-vlagent" "$NAME-vtagent" 2>/dev/null || true
  for q in vmagent-queue vlagent-queue vtagent-queue; do
    for d in "$DATA_DIR/$q"/persistent-queue/2_*; do
      [ -d "$d" ] && { echo "상대 노드 몫 큐 삭제: $d"; rm -rf "$d"; }
    done
  done
fi

echo "[6/7] systemd"
for s in $SERVICES; do
  sed -e "s|@NAME@|$NAME|g" -e "s|@USER@|$SERVICE_USER|g" -e "s|@GROUP@|$SERVICE_GROUP|g" \
      -e "s|@BIN@|$BIN_DIR|g" -e "s|@CONF@|$CONF_DIR|g" -e "s|@DATA@|$DATA_DIR|g" -e "s|@LOGS@|$LOG_DIR|g" \
      "systemd/$s.service.tmpl" > "/etc/systemd/system/$NAME-$s.service"
done
# 로그 파일은 logrotate 로 돌린다 (서비스가 파일을 계속 열고 있으므로 copytruncate)
cat > "/etc/logrotate.d/$NAME" <<ROT
$LOG_DIR/*.log {
    daily
    rotate 7
    compress
    missingok
    notifempty
    copytruncate
}
ROT
systemctl daemon-reload
for s in $SERVICES; do
  systemctl enable "$NAME-$s" >/dev/null 2>&1
  systemctl restart "$NAME-$s"
done

echo "[7/7] 방화벽"
if systemctl is-active --quiet firewalld; then
  ports="4318 8443"; [ "$MODE" = ha ] && ports="$ports 7443 9094"
  for p in $ports; do firewall-cmd --permanent --add-port="$p/tcp" >/dev/null; done
  [ "$MODE" = ha ] && firewall-cmd --permanent --add-port=9094/udp >/dev/null
  firewall-cmd --reload
else
  echo "firewalld 꺼짐"
fi
echo "설치 끝: $SELF ($MODE, 상대 ${PEER:-없음})"

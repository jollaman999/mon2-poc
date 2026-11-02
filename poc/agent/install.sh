#!/usr/bin/env bash
# 에이전트(cmp-otelcol)를 이 호스트에 서비스로 설치한다. 묶음(cmp-otelcol-<버전>-linux-amd64)을 푼 폴더에서 root 로 돌린다.
# 다시 돌려도 된다: 바이너리 · 설정 · 유닛을 새것으로 바꾸고 서비스를 다시 띄운다. 큐(data)는 그대로 둔다.
# 사용: install.sh --nodes https://A:4318,https://B:4318 [--interval 10s] [--failback 1m]
#                  [--tls-verify skip|ca] [--ca <사설 CA pem>] [--start-key <값>]
#   --nodes       중앙 노드 수집 주소 목록 (쉼표로). 평소에는 한 곳에만 보내고 실패하면 다음 주소로 넘어간다
#   --interval    호스트 지표 수집 주기 (기본 10s)
#   --failback    시작 주소로 돌아가기를 시도하는 간격 (기본 1m)
#   --tls-verify  노드 HTTPS 인증서: skip(검증 안 함, 기본) 또는 ca(--ca 로 준 사설 CA 로 검증)
#   --start-key   시작 주소를 고를 때 해시할 값 (기본 호스트 이름)
# 기준 폴더 /cmp-agent 아래: bin(실행 파일) · conf(설정) · data(file_storage 디스크 큐) · logs(에이전트 로그)
set -euo pipefail
cd "$(dirname "$0")"
NAME=cmp-otelcol
BASE_DIR=/cmp-agent
BIN_DIR=$BASE_DIR/bin CONF_DIR=$BASE_DIR/conf DATA_DIR=$BASE_DIR/data LOG_DIR=$BASE_DIR/logs
SELINUX_MODULE=cmp_agent_files

nodes="" interval=10s failback=1m tls_verify=skip ca="" start_key=""
while [ $# -gt 0 ]; do
  case $1 in
    --nodes)      nodes=${2:?}; shift 2 ;;
    --interval)   interval=${2:?}; shift 2 ;;
    --failback)   failback=${2:?}; shift 2 ;;
    --tls-verify) tls_verify=${2:?}; shift 2 ;;
    --ca)         ca=${2:?}; shift 2 ;;
    --start-key)  start_key=${2:?}; shift 2 ;;
    *) echo "모르는 인자: $1" >&2; exit 1 ;;
  esac
done
[ "$(id -u)" = 0 ] || { echo "root 로 돌린다" >&2; exit 1; }
[ -n "$nodes" ] || { echo "--nodes 가 필요하다 (예: https://192.0.2.11:4318,https://192.0.2.12:4318)" >&2; exit 1; }
for f in bin/$NAME systemd/$NAME.service config.yaml.tmpl uninstall.sh; do
  [ -f "$f" ] || { echo "묶음에 $f 가 없다. 묶음을 푼 폴더에서 돌린다" >&2; exit 1; }
done
# 쉼표 목록 → YAML 목록 안의 "주소", "주소"
endpoints=""
IFS=, read -ra list <<<"$nodes"
for n in "${list[@]}"; do
  case $n in https://*|http://*) ;; *) echo "주소는 http(s):// 로 시작한다: $n" >&2; exit 1 ;; esac
  endpoints="$endpoints${endpoints:+, }\"$n\""
done
case $tls_verify in
  skip) tls_line="insecure_skip_verify: true" ;;
  ca)   [ -s "$ca" ] || { echo "--tls-verify ca 에는 --ca <pem> 이 필요하다" >&2; exit 1; }
        tls_line="ca_file: $CONF_DIR/ca.pem" ;;
  *)    echo "--tls-verify 는 skip 또는 ca: $tls_verify" >&2; exit 1 ;;
esac

echo "[1/6] 폴더"
# 기준 폴더는 다른 것과 같이 쓸 수 있으므로 우리 하위 폴더만 만든다
install -d -m 0755 "$BASE_DIR" "$BIN_DIR" "$CONF_DIR" "$LOG_DIR"
install -d -m 0700 "$DATA_DIR"

echo "[2/6] 실행 파일"
# 돌고 있는 실행 파일을 덮어쓰지 않도록 옆에 복사한 뒤 바꿔 끼운다 (서비스는 6단계에서 다시 띄운다)
install -m 0755 "bin/$NAME" "$BIN_DIR/.$NAME.new"
mv -f "$BIN_DIR/.$NAME.new" "$BIN_DIR/$NAME"
install -m 0755 uninstall.sh "$BIN_DIR/uninstall.sh"
"$BIN_DIR/$NAME" --version

echo "[3/6] 설정"
[ "$tls_verify" = ca ] && install -m 0644 "$ca" "$CONF_DIR/ca.pem"
sed -e "s|@QUEUE_DIR@|$DATA_DIR|" -e "s|@INTERVAL@|$interval|" -e "s|@ENDPOINTS@|$endpoints|" \
    -e "s|@START_KEY@|$start_key|" -e "s|@FAILBACK@|$failback|" -e "s|@TLS@|$tls_line|" \
    -e "s|@LOG_FILE@|$LOG_DIR/$NAME.log|g" config.yaml.tmpl > "$CONF_DIR/.config.yaml.new"
chmod 0640 "$CONF_DIR/.config.yaml.new"
"$BIN_DIR/$NAME" validate --config "$CONF_DIR/.config.yaml.new"
mv -f "$CONF_DIR/.config.yaml.new" "$CONF_DIR/config.yaml"
echo "노드 $nodes, 수집 주기 $interval, 돌아가기 $failback, TLS $tls_verify"

echo "[4/6] SELinux 라벨"
# semanage(policycoreutils-python-utils)는 없는 서버가 있다. 그래서 라벨 규칙을 CIL 정책 모듈로 만들어
# 늘 있는 semodule 로 넣는다. 재라벨링해도 유지되고, 지울 때 semodule -r 로 뺀다.
#   bin  bin_t     → systemd 가 unconfined_service_t 로 띄운다
#   conf etc_t · data var_lib_t · logs var_log_t, 기준 폴더 자체는 usr_t
if command -v selinuxenabled >/dev/null && selinuxenabled; then
  tmp=$(mktemp -d)
  ctx() { echo "(system_u object_r $1 ((s0) (s0)))"; }
  cat > "$tmp/$SELINUX_MODULE.cil" <<CIL
(filecon "$BASE_DIR" dir $(ctx usr_t))
(filecon "$BIN_DIR(/.*)?" any $(ctx bin_t))
(filecon "$CONF_DIR(/.*)?" any $(ctx etc_t))
(filecon "$DATA_DIR(/.*)?" any $(ctx var_lib_t))
(filecon "$LOG_DIR(/.*)?" any $(ctx var_log_t))
CIL
  semodule -i "$tmp/$SELINUX_MODULE.cil"
  rm -rf "$tmp"
  # 기준 폴더 안의 다른 것은 건드리지 않도록 우리 폴더만 다시 라벨링한다
  restorecon "$BASE_DIR"
  restorecon -R "$BIN_DIR" "$CONF_DIR" "$DATA_DIR" "$LOG_DIR"
  ls -dZ "$BASE_DIR" "$BIN_DIR/$NAME" "$CONF_DIR" "$DATA_DIR" "$LOG_DIR"
else
  echo "SELinux 꺼짐 또는 없음"
fi

echo "[5/6] logrotate"
# 에이전트가 로그 파일을 계속 열고 있으므로 copytruncate
cat > "/etc/logrotate.d/$NAME" <<ROT
$LOG_DIR/*.log {
    daily
    rotate 7
    maxsize 50M
    compress
    missingok
    notifempty
    copytruncate
}
ROT

echo "[6/6] 서비스"
install -m 0644 "systemd/$NAME.service" "/etc/systemd/system/$NAME.service"
systemctl daemon-reload
systemctl enable "$NAME" >/dev/null 2>&1
systemctl restart "$NAME"
sleep 3
if ! systemctl is-active --quiet "$NAME"; then
  systemctl status --no-pager "$NAME" || true
  tail -20 "$LOG_DIR/$NAME.log" 2>/dev/null || true
  echo "서비스가 뜨지 않았다" >&2; exit 1
fi
if command -v selinuxenabled >/dev/null && selinuxenabled; then
  ps -o label=,pid=,cmd= -C "$NAME"
fi
echo "설치 끝: $NAME $(systemctl is-active "$NAME"), 로그 $LOG_DIR/$NAME.log"

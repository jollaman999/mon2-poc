#!/usr/bin/env bash
# install.sh 가 깐 것을 지운다: 서비스 · 유닛 · SELinux 모듈 · logrotate 설정 · /cmp-agent 의 bin · conf · logs.
# 큐(data)는 --purge 일 때만 지운다 (아직 못 보낸 데이터가 남아 있을 수 있다).
# 사용: uninstall.sh [--purge]   (root)
set -euo pipefail
NAME=cmp-otelcol
BASE_DIR=/cmp-agent
SELINUX_MODULE=cmp_agent_files
purge=""
[ "${1:-}" = --purge ] && purge=1
[ "$(id -u)" = 0 ] || { echo "root 로 돌린다" >&2; exit 1; }

systemctl disable --now "$NAME" 2>/dev/null || true
rm -f "/etc/systemd/system/$NAME.service"
systemctl daemon-reload
systemctl reset-failed "$NAME" 2>/dev/null || true
rm -f "/etc/logrotate.d/$NAME"

selinux=""
if command -v selinuxenabled >/dev/null && selinuxenabled; then
  selinux=1
  semodule -r "$SELINUX_MODULE" 2>/dev/null || true
fi

# 기준 폴더를 다른 것과 같이 쓸 수 있으므로 우리가 만든 하위 폴더만 지우고, 기준 폴더는 비었을 때만 지운다
rm -rf "${BASE_DIR:?}/bin" "${BASE_DIR:?}/conf" "${BASE_DIR:?}/logs"
if [ -n "$purge" ]; then
  rm -rf "${BASE_DIR:?}/data"
  echo "큐 데이터 삭제"
elif [ -d "$BASE_DIR/data" ]; then
  echo "큐 데이터는 남김: $BASE_DIR/data (지우려면 --purge)"
fi
if rmdir "$BASE_DIR" 2>/dev/null; then
  :
elif [ -d "$BASE_DIR" ]; then
  echo "$BASE_DIR 에 다른 파일이 있어 폴더는 남김"
  # 모듈을 뺐으므로 남은 폴더는 기본 라벨로 되돌린다 (안의 다른 것은 건드리지 않는다)
  [ -n "$selinux" ] && restorecon "$BASE_DIR"
fi
echo "삭제 끝: $NAME"

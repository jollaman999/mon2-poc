#!/usr/bin/env bash
# install.sh 가 깐 것을 지운다. BASE_DIR/conf/install.env 에 남긴 값을 읽는다.
# BASE_DIR 아래 bin · conf · data · logs 와 서비스 · SELinux 모듈 · logrotate 설정을 지운다.
# 사용: uninstall.sh <BASE_DIR>   (기본 /cmp-mon)
set -euo pipefail
base=${1:-/cmp-mon}
[ -f "$base/conf/install.env" ] || { echo "$base/conf/install.env 가 없다" >&2; exit 1; }
. "$base/conf/install.env"
CONF_DIR=$BASE_DIR/conf
SERVICES="gateway vmalert alertmanager vmagent vlagent vtagent victoria-metrics victoria-logs victoria-traces"

for s in $SERVICES; do
  systemctl disable --now "$NAME-$s" 2>/dev/null || true
  rm -f "/etc/systemd/system/$NAME-$s.service"
done
systemctl daemon-reload

if command -v selinuxenabled >/dev/null && selinuxenabled; then
  semodule -r "$SELINUX_MODULE" 2>/dev/null || true
fi

rm -f "/etc/logrotate.d/$NAME"
if [ -f "$CONF_DIR/.created-user" ]; then
  userdel "$SERVICE_USER" 2>/dev/null || true
  echo "계정 $SERVICE_USER 삭제"
else
  echo "계정 $SERVICE_USER 은 원래 있던 것이라 남김"
fi
# 기준 폴더를 다른 것과 같이 쓸 수 있으므로 우리가 만든 하위 폴더만 지우고, 기준 폴더는 비었을 때만 지운다
case "$BASE_DIR" in ""|/) echo "BASE_DIR 이 이상하다: '$BASE_DIR'" >&2; exit 1 ;; esac
for d in bin conf data logs; do rm -rf "${BASE_DIR:?}/$d"; done
rmdir "$BASE_DIR" 2>/dev/null || echo "$BASE_DIR 에 다른 파일이 있어 폴더는 남김"
echo "삭제 끝: $NAME"

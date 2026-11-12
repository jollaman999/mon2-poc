#!/usr/bin/env bash
# install.sh 가 띄운 컨테이너를 내리고 설정을 지운다. BASE_DIR/conf/install.env 에 남긴 값을 읽는다.
# 기본은 데이터(BASE_DIR/data)를 남긴다. --purge 를 주면 데이터 · 로그 · 이미지 · 우리가 만든 계정까지 지운다.
# Podman · podman-compose 는 다른 것도 쓸 수 있어 지우지 않는다.
# 사용: uninstall.sh [--purge] [BASE_DIR]   (기본 /cmp-mon)
set -euo pipefail
purge=0
[ "${1:-}" = --purge ] && { purge=1; shift; }
base=${1:-/cmp-mon}
[ -f "$base/conf/install.env" ] || { echo "$base/conf/install.env 가 없다" >&2; exit 1; }
. "$base/conf/install.env"
COMPOSE_DIR=$BASE_DIR/compose
PC=/usr/local/bin/podman-compose
case "$BASE_DIR" in ""|/) echo "BASE_DIR 이 이상하다: '$BASE_DIR'" >&2; exit 1 ;; esac

if [ -f "$COMPOSE_DIR/compose.yaml" ]; then
  ( cd "$COMPOSE_DIR" && "$PC" --profile ha down >/dev/null 2>&1 ) || true
fi
# compose 파일이 없거나 down 이 실패해도 이름으로 남은 컨테이너를 내린다
podman ps -a --filter "name=^$NAME-" --format '{{.Names}}' | while read -r c; do podman rm -f "$c" >/dev/null; echo "컨테이너 삭제: $c"; done
if podman ps -a --filter "name=^$NAME-" --format '{{.Names}}' | grep -q .; then echo "컨테이너가 남았다" >&2; exit 1; fi

# podman-restart.service 는 우리가 켠 경우에만 끈다
if [ "${ENABLED_PODMAN_RESTART:-0}" = 1 ]; then
  systemctl disable podman-restart.service >/dev/null 2>&1 || true
  echo "podman-restart.service 끔"
fi

# 앞선 설치가 연 vmstorage 8401 rich rule (상대 노드 출발지만)을 지운다. 포트 4318 등은 다른 것도 쓸 수 있어 남긴다
if [ -n "${FW_8401_PEER:-}" ] && systemctl is-active --quiet firewalld; then
  fam=ipv4; case "$FW_8401_PEER" in *:*) fam=ipv6 ;; esac
  firewall-cmd --permanent --remove-rich-rule="rule family=\"$fam\" source address=\"$FW_8401_PEER\" port port=\"8401\" protocol=\"tcp\" accept" >/dev/null 2>&1 || true
  firewall-cmd --reload >/dev/null
  echo "방화벽 8401 (출발지 $FW_8401_PEER) 규칙 삭제"
fi

images=("victoriametrics/vmstorage:$VM_VERSION-cluster" "victoriametrics/vminsert:$VM_VERSION-cluster"
        "victoriametrics/vmselect:$VM_VERSION-cluster" "victoriametrics/vmagent:$VM_VERSION"
        "victoriametrics/vmalert:$VM_VERSION" "victoriametrics/vmauth:$VM_VERSION" "victoriametrics/vmctl:$VM_VERSION"
        "victoriametrics/victoria-logs:$VL_VERSION" "victoriametrics/vlagent:$VL_VERSION"
        "victoriametrics/victoria-traces:$VT_VERSION" "victoriametrics/vtagent:$VT_VERSION"
        "prom/alertmanager:v$AM_VERSION")

rm -rf "${BASE_DIR:?}/compose"
if [ "$purge" = 1 ]; then
  rm -rf "${BASE_DIR:?}/conf" "${BASE_DIR:?}/data" "${BASE_DIR:?}/logs"
  for i in "${images[@]}"; do
    if podman rmi "docker.io/$i" >/dev/null 2>&1; then echo "이미지 삭제: $i"; fi
  done
  if [ "${CREATED_USER:-0}" = 1 ]; then
    userdel "$SERVICE_USER" 2>/dev/null || true
    echo "계정 $SERVICE_USER 삭제"
  else
    echo "계정 $SERVICE_USER 은 원래 있던 것이라 남김"
  fi
  # 기준 폴더를 다른 것과 같이 쓸 수 있으므로 비었을 때만 지운다
  rmdir "$BASE_DIR" 2>/dev/null || echo "$BASE_DIR 에 다른 파일이 있어 폴더는 남김"
else
  # 나중에 --purge 로 다시 지울 수 있게 install.env 만 남긴다
  find "$BASE_DIR/conf" -mindepth 1 -maxdepth 1 ! -name install.env -exec rm -rf {} +
  echo "데이터 · 로그 · 이미지 · 계정 $SERVICE_USER 은 남김: $BASE_DIR/data, $BASE_DIR/logs (모두 지우려면 --purge)"
fi
echo "삭제 끝: $NAME"

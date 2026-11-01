#!/usr/bin/env bash
# 중앙 노드 하나에 Victoria 3종 · 중계 3종 · vmalert · Alertmanager · HAProxy 를 설치한다.
# deploy.sh 가 묶음(bundle)을 올린 뒤 그 안에서 root 로 실행한다. 인터넷에 나가지 않는다.
#
# 환경 변수
#   HAPROXY_SELINUX=boolean  haproxy_connect_any 를 켠다 (기본)
#   HAPROXY_SELINUX=off      켜지 않는다. SELinux 가 무엇을 막는지 AVC 로 보려고 둔다
set -euo pipefail
cd "$(dirname "$0")"
. ./versions.env
. ./node.env          # SELF PEER NODE_A NODE_B RETENTION QUEUE_MAX WEBHOOK
HAPROXY_SELINUX=${HAPROXY_SELINUX:-boolean}

[ "$(id -u)" = 0 ] || { echo "root 로 실행해야 한다" >&2; exit 1; }

echo "[1/7] 사용자 · 디렉터리"
id mon2 >/dev/null 2>&1 || useradd --system --no-create-home --shell /sbin/nologin mon2
install -d -m 0755 /opt/mon2/bin /etc/mon2 /etc/mon2/rules
install -d -m 0750 -o mon2 -g mon2 /var/lib/mon2

echo "[2/7] 바이너리"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for f in dist/*.tar.gz; do tar xzf "$f" -C "$tmp"; done
for b in victoria-metrics-prod vmagent-prod vmalert-prod vmctl-prod vmbackup-prod \
         victoria-logs-prod vlagent-prod victoria-traces-prod vtagent-prod; do
  install -m 0755 "$tmp/$b" /opt/mon2/bin/
done
install -m 0755 "$tmp"/alertmanager-*/alertmanager "$tmp"/alertmanager-*/amtool /opt/mon2/bin/

echo "[3/7] SELinux 라벨"
if command -v selinuxenabled >/dev/null && selinuxenabled; then
  # chcon 이 아니라 규칙으로 등록해야 전체 재라벨링 뒤에도 유지된다
  semanage fcontext -a -t bin_t '/opt/mon2/bin(/.*)?' 2>/dev/null \
    || semanage fcontext -m -t bin_t '/opt/mon2/bin(/.*)?'
  restorecon -R /opt/mon2
  ls -Z /opt/mon2/bin/victoria-metrics-prod
else
  echo "SELinux 꺼짐 또는 없음"
fi

echo "[4/7] 설정"
install -m 0644 node.env /etc/mon2/mon2.env
sed "s|@WEBHOOK@|$WEBHOOK|" conf/alertmanager.yml > /etc/mon2/alertmanager.yml
install -m 0644 conf/rules/*.yml /etc/mon2/rules/

echo "[5/7] HAProxy"
if ! rpm -q haproxy >/dev/null 2>&1; then
  rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-*
  rpm -K "dist/$HAPROXY_RPM"
  dnf install -y --disablerepo='*' "dist/$HAPROXY_RPM"
fi
sed -e "s|@SELF@|$SELF|g" -e "s|@PEER@|$PEER|g" conf/haproxy.cfg.tmpl > /etc/haproxy/haproxy.cfg
haproxy -c -f /etc/haproxy/haproxy.cfg
if command -v selinuxenabled >/dev/null && selinuxenabled; then
  case "$HAPROXY_SELINUX" in
    boolean) setsebool -P haproxy_connect_any 1 ;;
    off)     echo "haproxy_connect_any 를 켜지 않는다 (AVC 관찰용)" ;;
    *)       echo "HAPROXY_SELINUX 값이 이상하다: $HAPROXY_SELINUX" >&2; exit 1 ;;
  esac
  getsebool haproxy_connect_any
fi

echo "[6/7] systemd"
install -m 0644 systemd/*.service /etc/systemd/system/
systemctl daemon-reload
for s in victoria-metrics victoria-logs victoria-traces vmagent vlagent vtagent alertmanager vmalert; do
  systemctl enable --now "$s"
  systemctl restart "$s"
done
systemctl enable haproxy
systemctl restart haproxy

echo "[7/7] 방화벽"
if systemctl is-active --quiet firewalld; then
  # 바깥에 여는 것은 HAProxy 3개(수집 4318, 조회 8443, 노드 사이 7443)와 Alertmanager 클러스터 9094 뿐이다
  for p in 4318 8443 7443 9094; do
    firewall-cmd --permanent --add-port=$p/tcp >/dev/null
  done
  firewall-cmd --permanent --add-port=9094/udp >/dev/null
  firewall-cmd --reload
else
  echo "firewalld 꺼짐"
fi
echo "설치 끝: $SELF (상대 $PEER)"

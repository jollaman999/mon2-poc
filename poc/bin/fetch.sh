#!/usr/bin/env bash
# 중앙 노드에 깔 파일을 로컬 dist/ 로 받는다.
# 노드에서 DNS 가 안 되는 환경을 가정해 노드는 인터넷에 나가지 않는다.
set -euo pipefail
cd "$(dirname "$0")/.."
. ./versions.env
mkdir -p dist
gh_dl() { # repo tag asset
  local url="https://github.com/$1/releases/download/$2/$3"
  [ -s "dist/$3" ] || curl -fsSL --retry 3 -o "dist/$3" "$url"
}
gh_dl VictoriaMetrics/VictoriaMetrics "$VM_VERSION" "victoria-metrics-linux-amd64-$VM_VERSION.tar.gz"
gh_dl VictoriaMetrics/VictoriaMetrics "$VM_VERSION" "vmutils-linux-amd64-$VM_VERSION.tar.gz"
gh_dl VictoriaMetrics/VictoriaMetrics "$VM_VERSION" "victoria-metrics-linux-amd64-${VM_VERSION}_checksums.txt"
gh_dl VictoriaMetrics/VictoriaMetrics "$VM_VERSION" "vmutils-linux-amd64-${VM_VERSION}_checksums.txt"
gh_dl VictoriaMetrics/VictoriaLogs "$VL_VERSION" "victoria-logs-linux-amd64-$VL_VERSION.tar.gz"
gh_dl VictoriaMetrics/VictoriaLogs "$VL_VERSION" "vlutils-linux-amd64-$VL_VERSION.tar.gz"
gh_dl VictoriaMetrics/VictoriaLogs "$VL_VERSION" "victoria-logs-linux-amd64-${VL_VERSION}_checksums.txt"
gh_dl VictoriaMetrics/VictoriaLogs "$VL_VERSION" "vlutils-linux-amd64-${VL_VERSION}_checksums.txt"
gh_dl VictoriaMetrics/VictoriaTraces "$VT_VERSION" "victoria-traces-linux-amd64-$VT_VERSION.tar.gz"
gh_dl VictoriaMetrics/VictoriaTraces "$VT_VERSION" "vtutils-linux-amd64-$VT_VERSION.tar.gz"
gh_dl VictoriaMetrics/VictoriaTraces "$VT_VERSION" "victoria-traces-linux-amd64-${VT_VERSION}_checksums.txt"
gh_dl VictoriaMetrics/VictoriaTraces "$VT_VERSION" "vtutils-linux-amd64-${VT_VERSION}_checksums.txt"
gh_dl prometheus/alertmanager "v$AM_VERSION" "alertmanager-$AM_VERSION.linux-amd64.tar.gz"
gh_dl prometheus/alertmanager "v$AM_VERSION" "sha256sums.txt"
mv -f dist/sha256sums.txt dist/alertmanager-sha256sums.txt 2>/dev/null || true

# 체크섬 확인 (Victoria 는 *_checksums.txt, Alertmanager 는 sha256sums.txt)
( cd dist
  for f in *_checksums.txt; do grep -E 'tar\.gz$' "$f" | sha256sum -c --ignore-missing; done
  grep "alertmanager-$AM_VERSION.linux-amd64.tar.gz" alertmanager-sha256sums.txt | sha256sum -c
)
echo "dist 준비 끝"

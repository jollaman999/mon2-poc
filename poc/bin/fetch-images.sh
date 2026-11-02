#!/usr/bin/env bash
# 중앙 노드에 띄울 공식 이미지를 받아 dist/images/*.tar 로 낸다 (노드에서 podman load).
# 버전은 versions.env 를 따른다. 로컬에 docker 가 있어야 한다. 노드는 인터넷에 나가지 않는다.
# 사용: bin/fetch-images.sh
set -euo pipefail
cd "$(dirname "$0")/.."
. ./versions.env
out=dist/images
mkdir -p "$out"

images=(
  "victoriametrics/victoria-metrics:$VM_VERSION"
  "victoriametrics/vmagent:$VM_VERSION"
  "victoriametrics/vmalert:$VM_VERSION"
  "victoriametrics/vmauth:$VM_VERSION"
  "victoriametrics/vmctl:$VM_VERSION"
  "victoriametrics/victoria-logs:$VL_VERSION"
  "victoriametrics/vlagent:$VL_VERSION"
  "victoriametrics/victoria-traces:$VT_VERSION"
  "victoriametrics/vtagent:$VT_VERSION"
  "prom/alertmanager:v$AM_VERSION"
)

files=()
for img in "${images[@]}"; do
  # 파일 이름: 저장소 이름의 마지막 부분 + 태그 (예: vmagent-v1.155.0.tar)
  name=${img##*/}; file="$out/${name%%:*}-${name##*:}.tar"
  files+=("${file##*/}")
  if [ -s "$file" ]; then echo "있음: $file"; continue; fi
  docker pull --platform linux/amd64 -q "$img" >/dev/null
  docker save -o "$file.part" "$img"
  mv "$file.part" "$file"
  echo "저장: $file ($(du -h "$file" | cut -f1))"
done
# 노드는 이 목록에 있는 것만 올린다 (버전을 바꿔 남은 옛 파일은 올리지 않는다)
( cd "$out" && sha256sum "${files[@]}" > SHA256SUMS )
echo "images 준비 끝: $out"

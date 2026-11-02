#!/usr/bin/env bash
# 중앙 노드(EL10)에 깔 Podman · podman-compose 오프라인 묶음을 dist/podman-el10/ 로 만든다.
# 사용: bin/fetch-podman.sh <원본 묶음 폴더>
#   원본 폴더에는 rpms/podman/*.rpm (EL9) 과 whl/podman-compose/*.whl 이 있다.
#   EL9 RPM 은 노드(Rocky 10)에 맞지 않으므로 파일 이름에서 패키지 이름만 뽑아,
#   로컬 docker 의 rockylinux/rockylinux:10 컨테이너에서 같은 이름의 EL10 판을 의존성까지 받는다.
#   python3-pyyaml 은 EL10 RPM 으로 넣는다 (원본 PyYAML wheel 은 cp39 라 python 3.12 에 안 맞는다).
#   wheel 은 어느 파이썬에서나 도는 py3-none-any 인 podman_compose · python_dotenv · pip 만 복사한다.
# 노드에서는 rpms/ 를 로컬 저장소로 쓴다 (createrepo 로 repodata 를 만든다). 그래야 dnf 가 이미 깔린 패키지는
# 그대로 두고, 새 패키지가 요구할 때만 올린다. 설치할 이름은 packages.txt 에 남긴다.
set -euo pipefail
src=${1:?원본 묶음 폴더 (rpms/podman, whl/podman-compose 가 있는 곳)}
src=$(cd "$src" && pwd)
cd "$(dirname "$0")/.."
out=$PWD/dist/podman-el10
[ -d "$src/rpms/podman" ] && [ -d "$src/whl/podman-compose" ] || { echo "$src 에 rpms/podman 또는 whl/podman-compose 가 없다" >&2; exit 1; }

# 이름-버전-릴리스.아키텍처.rpm 에서 뒤 세 덩어리를 떼어 이름만 남긴다
names=$(for f in "$src"/rpms/podman/*.rpm; do basename "$f" | sed -E 's/-[^-]+-[^-]+\.[^.]+\.rpm$//'; done | sort -u)
names="$names python3-pyyaml"
echo "받을 패키지: $(echo "$names" | tr '\n' ' ')"

mkdir -p "$out/rpms" "$out/whl"
find "$out/rpms" -name '*.rpm' -delete
rm -f "$out/packages.txt"
# EL10 저장소에 있는 이름만 골라(repoquery) 의존성까지 받는다(--resolve: 컨테이너에 없는 것까지). 없는 이름은 알린다
docker run --rm -i --platform linux/amd64 -e NAMES="$names" -e OWNER="$(id -u):$(id -g)" \
  -v "$out/rpms:/out" rockylinux/rockylinux:10 bash -s <<'IN'
set -euo pipefail
dnf -q -y install 'dnf-command(download)' >/dev/null
have=()
for n in $NAMES; do
  if [ -n "$(dnf -q repoquery --qf '%{name}\n' "$n" 2>/dev/null | grep -x "$n")" ]; then have+=("$n")
  else echo "EL10 에 없어 뺐다: $n"; fi
done
dnf -q download --resolve --destdir /out "${have[@]}"
dnf -q -y install createrepo_c >/dev/null
createrepo_c -q --update /out
printf '%s\n' "${have[@]}" > /out/packages.txt
chown -R "$OWNER" /out
IN

for w in "$src"/whl/podman-compose/*-py3-none-any.whl; do
  case "$(basename "$w")" in podman_compose-*|python_dotenv-*|pip-*) cp -f "$w" "$out/whl/" ;; esac
done
mv "$out/rpms/packages.txt" "$out/packages.txt"
( cd "$out" && sha256sum rpms/*.rpm whl/*.whl > SHA256SUMS )
echo "RPM $(find "$out/rpms" -name '*.rpm' | wc -l)개, wheel $(find "$out/whl" -name '*.whl' | wc -l)개: $out"

#!/usr/bin/env bash
# 주기 대조 (메트릭). 설계 문서 7.2.
# 몇 분 전까지의 구간을 1분씩 나눠 로컬과 상대 노드의 샘플 수를 비교하고,
# 상대 쪽이 많은 분만 상대 노드(:7443/vm)에서 로컬로 복사한다.
# 겹쳐 복사한 샘플은 VictoriaMetrics 의 -dedup.minScrapeInterval 로 하나로 합쳐진다.
# 두 노드가 각자 돌리므로 어느 쪽이 빠졌든 빠진 쪽이 채운다.
# 사용: reconcile-metrics.sh <conf 폴더> [몇 분 전부터(기본 15)] [몇 분 전까지(기본 2)]
set -euo pipefail
conf=${1:?conf 폴더}; from_min=${2:-15}; to_min=${3:-2}
# conf/env 는 systemd EnvironmentFile 형식(값 안에 공백)이라 source 하지 않고 필요한 값만 읽는다
val() { sed -n "s/^$1=//p" "$conf/env" | head -1; }
MODE=$(val MODE); PEER=$(val PEER); TLS_VERIFY=$(val TLS_VERIFY)
[ "${MODE:-}" = ha ] && [ -n "${PEER:-}" ] || { echo "단일 노드라 대조할 상대가 없다"; exit 0; }
bin=$(dirname "$(readlink -f "$0")")
tok=$(cat "$conf/tokens/peer.token")
case "${TLS_VERIFY:-skip}" in
  skip) curl_tls=(-k); vmctl_tls=(--vm-native-src-insecure-skip-verify) ;;
  *)    curl_tls=(--cacert "$conf/tls/ca.pem"); vmctl_tls=(--vm-native-src-ca-file="$conf/tls/ca.pem") ;;
esac
local_q() { curl -sf --max-time 20 http://127.0.0.1:8428/api/v1/query --data-urlencode "query=$1" --data-urlencode "time=$2"; }
peer_q()  { curl -sf --max-time 20 "${curl_tls[@]}" -H "Authorization: Bearer $tok" "https://$PEER:7443/vm/api/v1/query" \
              --data-urlencode "query=$1" --data-urlencode "time=$2"; }
num() { grep -oE '"value":\[[0-9.]+,"[0-9]+"' | grep -oE '"[0-9]+"$' | tr -d '"' || true; }

q='sum(count_over_time({__name__=~".+"}[60s]))'
now=$(( $(date +%s) / 60 * 60 ))
copied=0; checked=0
for (( m = now - from_min * 60; m < now - to_min * 60; m += 60 )); do
  end=$(( m + 60 ))
  l=$(local_q "$q" "$end" | num); p=$(peer_q "$q" "$end" | num)
  [ -n "$p" ] || { echo "상대 노드 조회 실패 ($(date -d @$end +%T)), 이번 회차는 건너뛴다"; exit 0; }
  l=${l:-0}; checked=$(( checked + 1 ))
  if [ "$p" -gt "$l" ]; then
    echo "$(date -d @$m +%T)~$(date -d @$end +%T) 로컬 $l < 상대 $p: 상대에서 복사"
    "$bin/vmctl-prod" vm-native -s --vm-native-src-addr="https://$PEER:7443/vm" \
      --vm-native-src-bearer-token="$tok" "${vmctl_tls[@]}" \
      --vm-native-dst-addr=http://127.0.0.1:8428 \
      --vm-native-filter-match='{__name__=~".+"}' \
      --vm-native-filter-time-start="$(date -u -d @$m +%Y-%m-%dT%H:%M:%SZ)" \
      --vm-native-filter-time-end="$(date -u -d @$end +%Y-%m-%dT%H:%M:%SZ)" >/dev/null
    copied=$(( copied + 1 ))
  fi
done
echo "대조 끝: ${checked}분 비교, ${copied}분 복사"

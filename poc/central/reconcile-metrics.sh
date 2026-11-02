#!/bin/sh
# 주기 대조 (메트릭). 설계 문서 7.2.
# 몇 분 전까지의 구간을 1분씩 나눠 로컬과 상대 노드의 샘플 수를 비교하고,
# 상대 쪽이 많은 분만 상대 노드(:7443/vm)에서 로컬로 복사한다.
# 겹쳐 복사한 샘플은 VictoriaMetrics 의 -dedup.minScrapeInterval 로 하나로 합쳐진다.
# 두 노드가 각자 돌리므로 어느 쪽이 빠졌든 빠진 쪽이 채운다.
#
# compose 의 reconcile 컨테이너(공식 vmctl 이미지)에서 돈다. 이 이미지는 Alpine 이라 bash · curl 이 없어서
# POSIX sh 와 busybox wget 으로 쓴다. wget 의 HTTPS 는 OpenSSL(ssl_client)이라 인증서를 검증한다.
# 값은 환경 변수로 받는다 (compose 의 .env): BASE_DIR MODE PEER TLS_VERIFY RECONCILE_EVERY
# 사용: reconcile-metrics.sh [몇 분 전부터(기본 15)] [몇 분 전까지(기본 2)]   한 번 돌린다
#       reconcile-metrics.sh loop                                          RECONCILE_EVERY 초(기본 300)마다 되풀이
set -eu
if [ "${1:-}" = loop ]; then
  # 컨테이너의 PID 1 이라 처리기가 없으면 SIGTERM 을 무시한다. sleep 을 뒤로 돌려야 기다리는 중에도 바로 끝난다
  trap 'exit 0' TERM INT
  while :; do
    sleep "${RECONCILE_EVERY:-300}" & wait $!
    sh "$0" || echo "대조 실패 (다음 회차에 다시)"
  done
fi
from_min=${1:-15}; to_min=${2:-2}
conf=${BASE_DIR:?}/conf
[ "${MODE:-}" = ha ] && [ -n "${PEER:-}" ] || { echo "단일 노드라 대조할 상대가 없다"; exit 0; }
tok=$(cat "$conf/tokens/peer.token")
case "${TLS_VERIFY:-skip}" in
  skip) wget_tls=--no-check-certificate; vmctl_tls=--vm-native-src-insecure-skip-verify ;;
  *)    export SSL_CERT_FILE="$conf/tls/ca.pem"; wget_tls=-q; vmctl_tls=--vm-native-src-ca-file="$conf/tls/ca.pem" ;;
esac
# sum(count_over_time({__name__=~".+"}[60s])) 를 URL 인코딩한 것 (busybox 에 인코더가 없다)
q='sum%28count_over_time%28%7B__name__%3D~%22.%2B%22%7D%5B60s%5D%29%29'
local_q() { wget -q -T 20 -O- --post-data "query=$q&time=$1" http://127.0.0.1:8428/api/v1/query; }
peer_q()  { wget -q -T 20 -O- "$wget_tls" --header "Authorization: Bearer $tok" --post-data "query=$q&time=$1" \
              "https://$PEER:7443/vm/api/v1/query"; }
num() { grep -oE '"value":\[[0-9.]+,"[0-9]+"' | grep -oE '"[0-9]+"$' | tr -d '"' || true; }
hm() { date -u -d "@$1" +%H:%M:%SZ; }

now=$(( $(date +%s) / 60 * 60 ))
m=$(( now - from_min * 60 )); last=$(( now - to_min * 60 ))
copied=0; checked=0
while [ "$m" -lt "$last" ]; do
  end=$(( m + 60 ))
  l=$(local_q "$end" 2>/dev/null | num); p=$(peer_q "$end" 2>/dev/null | num)
  [ -n "$p" ] || { echo "상대 노드 조회 실패 ($(hm "$end")), 이번 회차는 건너뛴다"; exit 0; }
  l=${l:-0}; checked=$(( checked + 1 ))
  if [ "$p" -gt "$l" ]; then
    echo "$(hm "$m")~$(hm "$end") 로컬 $l < 상대 $p: 상대에서 복사"
    /vmctl-prod vm-native -s --vm-native-src-addr="https://$PEER:7443/vm" \
      --vm-native-src-bearer-token="$tok" "$vmctl_tls" \
      --vm-native-dst-addr=http://127.0.0.1:8428 \
      --vm-native-filter-match='{__name__=~".+"}' \
      --vm-native-filter-time-start="$(date -u -d "@$m" +%Y-%m-%dT%H:%M:%SZ)" \
      --vm-native-filter-time-end="$(date -u -d "@$end" +%Y-%m-%dT%H:%M:%SZ)" >/dev/null
    copied=$(( copied + 1 ))
  fi
  m=$end
done
echo "$(date -u +%FT%TZ) 대조 끝: ${checked}분 비교, ${copied}분 복사"

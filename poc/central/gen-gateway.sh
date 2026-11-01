#!/usr/bin/env bash
# vmauth 설정(gateway.yml)을 만든다. install.sh 가 부른다.
# 사용: gen-gateway.sh <출력 파일> <PEER 또는 빈 값> <노드 사이 토큰> <조회 토큰>
#
# vmauth 하나가 4318 · 8443 · 7443 을 모두 받는다. 설정이 하나라 경로가 세 포트에 다 열리므로,
# 경로를 누가 쓸 수 있는지는 토큰(Authorization: Bearer)으로 가른다 (설계 문서 5.2 절).
#   토큰 없음         수집     /v1/metrics · /v1/logs · /v1/traces · /api/v1/write → 로컬 중계, 죽으면 상대 :7443
#   조회 토큰         조회     /metrics · /logs · /traces 앞말을 떼고 → 로컬 저장소, 죽으면 상대 :7443
#   노드 사이 토큰    노드 사이 /vm /vl /vt /vmagent /vlagent /vtagent /am 앞말을 떼고 → 로컬 부품
# 상대 노드로 넘길 때는 노드 사이 토큰을 붙인다. 로컬 부품은 토큰을 보지 않는다.
set -euo pipefail
out=$1; peer=${2:-}; peer_token=$3; query_token=$4

# route <src_path 정규식> <앞말 떼기 수> <로컬 URL> [상대 경로]
route() {
  local src=$1 drop=$2 local_url=$3 peer_path=${4:-}
  echo "      - src_paths: [\"$src\"]"
  [ "$drop" -gt 0 ] && echo "        drop_src_path_prefix_parts: $drop"
  echo "        load_balancing_policy: first_available"
  echo "        retry_status_codes: [502, 503, 504]"
  if [ -n "$peer" ] && [ -n "$peer_path" ]; then
    echo "        headers: [\"Authorization: Bearer $peer_token\"]"
  fi
  echo "        url_prefix:"
  echo "          - \"$local_url\""
  [ -n "$peer" ] && [ -n "$peer_path" ] && echo "          - \"https://$peer:7443$peer_path\""
  return 0
}

umask 027
{
  echo "# 만든 것: install.sh (gen-gateway.sh). 토큰이 들어 있어 root 와 서비스 계정만 읽는다"
  echo "users:"
  echo "  - name: query"
  echo "    bearer_token: \"$query_token\""
  echo "    url_map:"
  # vmalert 는 데이터소스를 /metrics 하나로 쓰므로 로그 규칙(/metrics/select/logsql/...)을 먼저 VictoriaLogs 로 보낸다
  route "/metrics/select/logsql/.*" 1 "http://127.0.0.1:9428"  "/vl"
  route "/metrics/.*"               1 "http://127.0.0.1:8428"  "/vm"
  route "/logs/.*"                  1 "http://127.0.0.1:9428"  "/vl"
  route "/traces/.*"                1 "http://127.0.0.1:10428" "/vt"
  if [ -n "$peer" ]; then
    echo "  - name: peer"
    echo "    bearer_token: \"$peer_token\""
    echo "    url_map:"
    route "/vm/.*"      1 "http://127.0.0.1:8428"
    route "/vl/.*"      1 "http://127.0.0.1:9428"
    route "/vt/.*"      1 "http://127.0.0.1:10428"
    route "/vmagent/.*" 1 "http://127.0.0.1:8429"
    route "/vlagent/.*" 1 "http://127.0.0.1:9429"
    route "/vtagent/.*" 1 "http://127.0.0.1:10429"
    route "/am/.*"      1 "http://127.0.0.1:9093"
  fi
  echo "unauthorized_user:"
  echo "  url_map:"
  route "/v1/metrics"   0 "http://127.0.0.1:8429/opentelemetry"         "/vmagent/opentelemetry"
  route "/v1/logs"      0 "http://127.0.0.1:9429/insert/opentelemetry"  "/vlagent/insert/opentelemetry"
  route "/v1/traces"    0 "http://127.0.0.1:10429/insert/opentelemetry" "/vtagent/insert/opentelemetry"
  route "/api/v1/write" 0 "http://127.0.0.1:8429"                       "/vmagent"
} > "$out"

#!/usr/bin/env bash
# OCB 로 에이전트(cmp-otelcol)를 정적 바이너리로 빌드한다. 결과: agent/_build/cmp-otelcol
set -euo pipefail
cd "$(dirname "$0")"
OCB_VERSION=${OCB_VERSION:-v0.164.0}
BIN=${GOBIN_DIR:-$PWD/_tools}
mkdir -p "$BIN"
[ -x "$BIN/builder" ] || GOBIN="$BIN" go install "go.opentelemetry.io/collector/cmd/builder@$OCB_VERSION"
CGO_ENABLED=0 "$BIN/builder" --config builder-config.yaml
file _build/cmp-otelcol

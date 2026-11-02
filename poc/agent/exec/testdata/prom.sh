#!/bin/sh
# Prometheus 텍스트 형식 표본: counter, gauge, untyped, histogram, summary
cat <<'OUT'
# HELP app_requests_total 요청 수
# TYPE app_requests_total counter
app_requests_total{method="get"} 12
app_requests_total{method="post"} 3
# TYPE app_queue gauge
app_queue 4.5
app_plain 7
# TYPE app_latency_seconds histogram
app_latency_seconds_bucket{le="0.1"} 2
app_latency_seconds_bucket{le="0.5"} 5
app_latency_seconds_bucket{le="+Inf"} 6
app_latency_seconds_sum 1.7
app_latency_seconds_count 6
# TYPE app_size summary
app_size{quantile="0.5"} 10
app_size{quantile="0.9"} 30
app_size_sum 100
app_size_count 5
OUT

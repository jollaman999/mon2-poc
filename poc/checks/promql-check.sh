#!/usr/bin/env bash
# 새 에이전트(cmp-otelcol) 지표를 PromQL 로 조회해 결과가 있는지, 두 노드 값이 같은지 표로 낸다.
# 사용: checks/promql-check.sh                    노드 A · B 의 조회 입구(:8443/metrics, 조회 토큰)로 조회
#       checks/promql-check.sh --url <주소>       토큰 없이 그 VictoriaMetrics 하나에 조회 (예: http://127.0.0.1:8428)
#       ONLY=<정규식> checks/promql-check.sh ...  이름이 맞는 쿼리만 (예: ONLY=join)
#       checks/promql-check.sh seed <주소>        하이퍼바이저 조인 시험용 가짜 시계열을 그 주소의 /api/v1/import 로 넣는다
#       checks/promql-check.sh print              쿼리 목록만 낸다
#
# - 이름은 VictoriaMetrics -opentelemetry.usePrometheusNaming 을 거친 저장 이름이다 (README 9.3).
# - 두 노드에 같은 시각(time=지금-60초)으로 묻는다. 값 차이는 시계열마다 상대 차이의 최댓값이다.
# - 기대가 "값" 인 쿼리가 비거나 두 노드가 1% 넘게 다르면 종료 코드 1.
#   "XID" 는 XID 가 없으면 비는 것이 정상이고, "조인" 은 하이퍼바이저 수신기가 없으면 빈다 (--url 이면 값이 있어야 한다).
# - seed 는 시험용 저장소에만 쓴다. 중앙 노드 주소면 거절한다.
set -euo pipefail
cd "$(dirname "$0")/.."

names=(); expects=(); queries=()
add() { names+=("$1"); expects+=("$2"); queries+=("$3"); }

# 하이퍼바이저 VM 정보를 게스트 키로 바꾼다. 하이퍼바이저 자신의 host_id · host_mac 을 덮어쓰므로
# 키가 빈 VM 은 거른다 (거르지 않으면 하이퍼바이저 호스트 자신의 지표와 이어진다).
vm_by_uuid='max by (host_id, hypervisor_vm_name, hypervisor_vm_domain, hypervisor_host) (label_replace(label_replace(hypervisor_vm_info_ratio{hypervisor_vm_uuid!=""}, "host_id", "$1", "hypervisor_vm_uuid", "(.+)"), "hypervisor_host", "$1", "host_name", "(.*)"))'
vm_by_mac='max by (host_mac, hypervisor_vm_name, hypervisor_vm_domain, hypervisor_host) (label_replace(label_replace(hypervisor_vm_info_ratio{hypervisor_vm_mac!=""}, "host_mac", "$1", "hypervisor_vm_mac", "(.+)"), "hypervisor_host", "$1", "host_name", "(.*)"))'
gpu_by_guest='max by (host_id, accelerator_pci_bus_id, hypervisor_host, physical_pci_bus_id, physical_hw_id) (label_replace(label_replace(label_replace(label_replace(label_replace(hypervisor_gpu_mapping_ratio{hypervisor_vm_uuid!="", hypervisor_guest_pci_bus_id!=""}, "physical_pci_bus_id", "$1", "accelerator_pci_bus_id", "(.+)"), "physical_hw_id", "$1", "hw_id", "(.*)"), "accelerator_pci_bus_id", "$1", "hypervisor_guest_pci_bus_id", "(.+)"), "host_id", "$1", "hypervisor_vm_uuid", "(.+)"), "hypervisor_host", "$1", "host_name", "(.*)"))'
fs_skip='type!~"tmpfs|devtmpfs|overlay|squashfs|iso9660"'

add cpu_pct      값 '100 * (1 - sum by (host_name) (rate(system_cpu_time_seconds_total{state="idle"}[5m])) / sum by (host_name) (rate(system_cpu_time_seconds_total[5m])))'
add mem_pct      값 '100 * sum by (host_name) (system_memory_usage_bytes{state="used"}) / sum by (host_name) (system_memory_usage_bytes{state=~"used|free|buffered|cached"})'
add fs_pct       값 "100 * sum by (host_name, mountpoint) (system_filesystem_usage_bytes{state=\"used\", $fs_skip}) / sum by (host_name, mountpoint) (system_filesystem_usage_bytes{$fs_skip})"
add net_bps      값 '8 * sum by (host_name, direction) (rate(system_network_io_bytes_total{device!~"lo|docker.*|veth.*|virbr.*"}[5m]))'
add gpu_util     값 'accelerator_gpu_util_percent'
add gpu_mem_used 값 'accelerator_mem_used_bytes'
add nv_gpu_util  값 'nvidia_gpu_utilization_gpu_percent'
add nv_mem_used  값 'nvidia_gpu_memory_used_MiB'
add vgpu_license 값 'nvidia_gpu_vgpu_license_status_ratio'
add top_cpu      값 'topk by (host_name) (5, sum by (host_name, process_executable_name, process_pid) (rate(process_cpu_time_seconds_total{host_name!=""}[5m])))'
add top_mem      값 'topk by (host_name) (5, sum by (host_name, process_executable_name, process_pid) (process_memory_usage_bytes{host_name!=""}))'
add last_seen_s  값 'time() - max by (host_name) (max_over_time(timestamp(system_memory_usage_bytes{state="used"})[1h:1m]))'
add xid_1h       XID 'sum by (host_name, accelerator_pci_bus_id, nvidia_xid) (increase(nvidia_gpu_xid_count_total[1h]))'
add join_uuid    조인 "100 * (1 - sum by (host_id) (rate(system_cpu_time_seconds_total{state=\"idle\", host_id!=\"\"}[5m])) / sum by (host_id) (rate(system_cpu_time_seconds_total{host_id!=\"\"}[5m]))) * on (host_id) group_left (hypervisor_vm_name, hypervisor_vm_domain, hypervisor_host) $vm_by_uuid"
add join_mac     조인 "100 * (1 - sum by (host_mac) (rate(system_cpu_time_seconds_total{state=\"idle\", host_id=\"\", host_mac!=\"\"}[5m])) / sum by (host_mac) (rate(system_cpu_time_seconds_total{host_id=\"\", host_mac!=\"\"}[5m]))) * on (host_mac) group_left (hypervisor_vm_name, hypervisor_vm_domain, hypervisor_host) $vm_by_mac"
add join_gpu     조인 "accelerator_gpu_util_percent{host_id!=\"\"} * on (host_id, accelerator_pci_bus_id) group_left (hypervisor_host, physical_pci_bus_id, physical_hw_id) $gpu_by_guest"

# 조인 시험용 가짜 시계열. 하이퍼바이저 쪽 라벨은 hypervisor 수신기를 testdata(instance-0000000d · 00000004)로
# 돌려 VictoriaMetrics 에 쌓인 것과 같다. 게스트 쪽은 .73(uuid 있음) · .162(MAC 만) 와 같은 모양이다.
seed() {
  local url=$1
  python3 - "$url" <<'PY'
import json, sys, time, urllib.request
url = sys.argv[1].rstrip("/")
now = int(time.time()) * 1000
ts = [now - 600000 + i * 10000 for i in range(61)]
out = []
def s(labels, vals):
    out.append(json.dumps({"metric": labels, "values": vals, "timestamps": ts}))
hv = {"host_name": "hv-test", "host_id": "99999999-0000-4000-8000-000000000099", "host_mac": "aa:bb:cc:00:00:99", "os_type": "linux"}
lin = {"host_name": "vgpu-linux-test-03", "host_id": "10000000-0000-4000-8000-000000000007", "host_mac": "fa:16:3e:00:00:07", "os_type": "linux"}
win = {"host_name": "WIN-TEST", "host_mac": "fa:16:3e:00:00:02", "os_type": "windows"}
vm = {"__name__": "hypervisor_vm_info_ratio", "source": "libvirt", **{k: hv[k] for k in ("host_name", "host_id", "host_mac")}}
s(dict(vm, hypervisor_vm_domain="instance-0000000d", hypervisor_vm_name="vgpu-linux-test-03",
       hypervisor_vm_uuid="10000000-0000-4000-8000-000000000007", hypervisor_vm_mac="fa:16:3e:00:00:07"), [1] * 61)
s(dict(vm, hypervisor_vm_domain="instance-00000004", hypervisor_vm_name="vgpu-win-test-01",
       hypervisor_vm_uuid="10000000-0000-4000-8000-000000000002", hypervisor_vm_mac="fa:16:3e:00:00:02"), [1] * 61)
# uuid 가 없는 VM. 조인에서 빠져야 한다
s(dict(vm, hypervisor_vm_domain="instance-noid", hypervisor_vm_name="no-uuid-vm", hypervisor_vm_mac="fa:16:3e:00:00:aa"), [1] * 61)
s({"__name__": "hypervisor_gpu_mapping_ratio", "source": "nvidia-smi", **{k: hv[k] for k in ("host_name", "host_id", "host_mac")},
   "accelerator_pci_bus_id": "00000000:AB:00.0", "hw_id": "GPU-00000000-0000-4000-8000-000000000002", "hw_model": "Tesla T4",
   "hypervisor_gpu_status": "assigned", "hypervisor_gpu_type": "vgpu", "hypervisor_guest_pci_bus_id": "00000000:00:05.0",
   "hypervisor_vm_domain": "instance-0000000d", "hypervisor_vm_name": "vgpu-linux-test-03",
   "hypervisor_vm_uuid": "10000000-0000-4000-8000-000000000007", "nvidia_vgpu_profile": "GRID T4-4Q"}, [1] * 61)
# 게스트 CPU (바쁜 비율 linux 25%, windows 50%, 하이퍼바이저 자신 10%)
for h, busy in ((lin, 0.25), (win, 0.5), (hv, 0.1)):
    for st, frac in (("idle", 1 - busy), ("user", busy * 0.8), ("system", busy * 0.2)):
        s(dict(h, __name__="system_cpu_time_seconds_total", state=st), [round(4 * frac * 10 * i, 3) for i in range(61)])
s(dict(lin, __name__="accelerator_gpu_util_percent", vendor="nvidia", hw_model="GRID T4-4Q",
       accelerator_pci_bus_id="00000000:00:05.0"), [40] * 61)
req = urllib.request.Request(url + "/api/v1/import", data="\n".join(out).encode(), method="POST")
with urllib.request.urlopen(req, timeout=10) as r:
    print(f"{len(out)} 시계열을 {url}/api/v1/import 로 넣었다 (HTTP {r.status})")
PY
}

case "${1:-}" in
  print)
    for i in "${!names[@]}"; do printf '%s [%s]\n  %s\n' "${names[$i]}" "${expects[$i]}" "${queries[$i]}"; done
    exit 0 ;;
  seed)
    [ -n "${2:-}" ] || { echo "사용: $0 seed <주소>" >&2; exit 2; }
    if [ -f ./inventory.env ]; then
      . ./inventory.env
      for n in ${NODE_A:-} ${NODE_B:-}; do
        case "$2" in *"$n"*) echo "중앙 노드($n)에는 넣지 않는다" >&2; exit 2 ;; esac
      done
    fi
    seed "$2"; exit 0 ;;
esac

if [ -n "${ONLY:-}" ]; then
  keep_n=(); keep_e=(); keep_q=()
  for i in "${!names[@]}"; do
    [[ ${names[$i]} =~ $ONLY ]] || continue
    keep_n+=("${names[$i]}"); keep_e+=("${expects[$i]}"); keep_q+=("${queries[$i]}")
  done
  [ ${#keep_n[@]} -gt 0 ] || { echo "ONLY=$ONLY 에 맞는 쿼리가 없다" >&2; exit 2; }
  names=("${keep_n[@]}"); expects=("${keep_e[@]}"); queries=("${keep_q[@]}")
fi

at=$(( $(date +%s) - 60 ))
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
meta="$work/meta"
: > "$meta"
for i in "${!names[@]}"; do printf '%s\t%s\n' "${names[$i]}" "${expects[$i]}" >> "$meta"; done

# 노드에서 돌릴 조회 스크립트. 쿼리는 base64 로 넘겨 따옴표를 피하고, 토큰은 노드 밖으로 나오지 않는다
remote_script() {
  echo 'set +e'
  echo 'T=$(cat "$BASE/conf/tokens/query.token")'
  echo 'q() { echo "### $1"; timeout 30 curl -sk -H "Authorization: Bearer $T" "https://$SELF:8443/metrics/api/v1/query" --data-urlencode "query=$(echo "$2" | base64 -d)" --data-urlencode "time=$AT"; echo; }'
  for i in "${!names[@]}"; do
    printf "q %s %s\n" "${names[$i]}" "$(printf '%s' "${queries[$i]}" | base64 -w0)"
  done
}

cols=()
if [ "${1:-}" = --url ]; then
  url=${2:?사용: $0 --url <주소>}; url=${url%/}
  for i in "${!names[@]}"; do
    echo "### ${names[$i]}"
    timeout 30 curl -s "$url/api/v1/query" --data-urlencode "query=${queries[$i]}" --data-urlencode "time=$at" || true
    echo
  done > "$work/local"
  cols=(local)
  export REQUIRE_JOIN=1
else
  . ./inventory.env
  NAME=${NAME:-cmp-mon}
  BASE=${BASE_DIR:-/$NAME}
  ssh_opts=(-i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
  [ -n "${JUMP:-}" ] && ssh_opts+=(-J "$JUMP")
  run_node() { # 열 이름 노드
    # 스크립트를 표준 입력으로 넘기므로 ssh 에 -n 을 주지 않는다
    remote_script | timeout 180 ssh "${ssh_opts[@]}" "$SSH_USER@$2" "sudo BASE=$BASE SELF=$2 AT=$at bash -s" > "$work/$1" \
      || echo "$2: 조회 실패" >&2
  }
  run_node A "$NODE_A"; cols=(A)
  if [ -n "${NODE_B:-}" ]; then run_node B "$NODE_B"; cols+=(B); fi
  export REQUIRE_JOIN=0
fi

echo "조회 시각 $(date -d "@$at" '+%F %T %Z')"
python3 - "$meta" "${cols[@]/#/$work/}" <<'PY'
import json, os, sys
meta = [l.rstrip("\n").split("\t") for l in open(sys.argv[1]) if l.strip()]
files = sys.argv[2:]
require_join = os.environ.get("REQUIRE_JOIN") == "1"
HIDE = {"__name__", "host_id", "host_mac", "os_type", "scope_name", "scope_version",
        "hw_id", "hw_model", "accelerator_arch", "accelerator_index", "source", "vendor"}

def load(path):
    res, cur = {}, None
    for line in open(path, encoding="utf-8", errors="replace"):
        line = line.rstrip("\n")
        if line.startswith("### "):
            cur = line[4:]
        elif cur and line.strip():
            try:
                d = json.loads(line)
            except ValueError:
                res[cur] = ("오류", line[:80]); continue
            if d.get("status") != "success":
                res[cur] = ("오류", d.get("error", "")[:80]); continue
            res[cur] = ("ok", {tuple(sorted(r["metric"].items())): float(r["value"][1]) for r in d["data"]["result"]})
    return res

def short(v):
    a = abs(v)
    if a >= 1e9: return f"{v/1e9:.2f}G"
    if a >= 1e6: return f"{v/1e6:.2f}M"
    if a >= 1e4: return f"{v/1e3:.1f}k"
    return f"{v:.3g}"

def show(series):
    parts = []
    for k, v in sorted(series.items(), key=lambda kv: -kv[1])[:3]:
        lab = "/".join(val for name, val in k if name not in HIDE)
        parts.append(f"{lab}={short(v)}")
    more = len(series) - 3
    return ", ".join(parts) + (f" 외 {more}" if more > 0 else "")

data = [load(f) for f in files]
heads = [os.path.basename(f) for f in files]
print("| 쿼리 | 기대 | " + " | ".join(f"{h} 시계열" for h in heads) + " | 값 (첫 열, 큰 순 3개) | 최대 차이 | 판정 |")
print("|---|---|" + "---|" * len(heads) + "---|---|---|")
bad = 0
for name, expect in meta:
    got = [d.get(name, ("오류", "응답 없음")) for d in data]
    counts, verdict, diff = [], "", "-"
    errs = [g[1] for g in got if g[0] != "ok"]
    if errs:
        verdict = "조회 오류: " + errs[0]; bad += 1
        counts = ["-"] * len(got)
        print(f"| {name} | {expect} | " + " | ".join(counts) + f" | - | - | {verdict} |"); continue
    sets = [g[1] for g in got]
    counts = [str(len(s)) for s in sets]
    empty_ok = expect == "XID" or (expect == "조인" and not require_join)
    if all(len(s) == 0 for s in sets):
        if empty_ok:
            why = "XID 가 없으면 정상" if expect == "XID" else "하이퍼바이저 수신기 없음"
            verdict = f"비어 있음 ({why})"
        else:
            verdict = "비어 있음 (실패)"; bad += 1
    elif any(len(s) == 0 for s in sets):
        verdict = "한쪽만 비어 있음 (실패)"; bad += 1
    else:
        if len(sets) > 1:
            keys = set().union(*[set(s) for s in sets])
            same_keys = all(set(s) == keys for s in sets)
            m = 0.0
            for k in keys:
                vals = [s[k] for s in sets if k in s]
                if len(vals) > 1:
                    hi, lo = max(vals), min(vals)
                    m = max(m, (hi - lo) / max(abs(hi), abs(lo), 1e-12))
            diff = f"{m*100:.3g}%"
            if not same_keys:
                verdict = "시계열이 다름 (실패)"; bad += 1
            elif m < 1e-9:
                verdict = "같음"
            elif m <= 0.01:
                verdict = "거의 같음"
            else:
                verdict = "다름 (실패)"; bad += 1
        else:
            verdict = "값 있음"
    print(f"| {name} | {expect} | " + " | ".join(counts) + f" | {show(sets[0])} | {diff} | {verdict} |")
print(f"\n실패 {bad} / {len(meta)}")
sys.exit(1 if bad else 0)
PY

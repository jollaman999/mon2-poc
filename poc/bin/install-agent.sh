#!/usr/bin/env bash
# 에이전트 묶음(make agent-package)을 대상 VM 에 올려 설치하거나 지운다.
# 사용: bin/install-agent.sh --host <user@ip> [--jump <user@host>] [--key <pem> | --password]
#                            --nodes https://A:4318,https://B:4318 [--os linux|windows]
#                            [--interval 10s] [--failback 1m] [--tls-verify skip|ca] [--ca <pem>] [--start-key <값>]
#                            [--role hypervisor]   (linux 만. KVM/libvirt 하이퍼바이저 수신기를 켠다)
#       bin/install-agent.sh --host <user@ip> [--jump ...] [--key <pem> | --password] [--os ...] --uninstall [--purge]
#   linux    SSH 로 묶음을 올리고(scp) install.sh 를 root 로 돌린다 (root 가 아니면 sudo)
#   windows  WinRM(HTTPS 5986, 사설 인증서라 검증 안 함)으로 묶음을 올리고 install.ps1 을 돌린다. --password 가 필요하다.
#            --jump 가 있으면 점프 호스트로 ssh -L 터널을 열어 붙는다. 로컬에 pywinrm 이 있어야 한다
#   --password  비밀번호를 환경 변수 AGENT_PASSWORD 에서, 없으면 프롬프트로 받는다. 파일 · 명령줄 인자에 남기지 않는다
#               (SSH 는 sshpass -e 로 넘긴다). 점프 호스트는 키(ssh 기본 설정)로 들어간다
#   --purge     지울 때 큐 데이터(data)까지 지운다
set -euo pipefail
cd "$(dirname "$0")/.."

host="" jump="" key="" ask_secret="" os=linux action=install purge=""
inst_args=()
while [ $# -gt 0 ]; do
  case $1 in
    --host)       host=${2:?}; shift 2 ;;
    --jump)       jump=${2:?}; shift 2 ;;
    --key)        key=${2:?}; shift 2 ;;
    --password)   ask_secret=1; shift ;;
    --os)         os=${2:?}; shift 2 ;;
    --uninstall)  action=uninstall; shift ;;
    --purge)      purge=1; shift ;;
    --nodes|--interval|--failback|--tls-verify|--ca|--start-key|--role)
                  inst_args+=("$1" "${2:?}"); shift 2 ;;
    *) echo "모르는 인자: $1 (사용법은 이 파일 머리말)" >&2; exit 1 ;;
  esac
done
[[ $host == *@* ]] || { echo "--host 는 user@ip 로 준다" >&2; exit 1; }
user=${host%@*} ip=${host#*@}
case $os in linux|windows) ;; *) echo "--os 는 linux 또는 windows: $os" >&2; exit 1 ;; esac
[ -n "$key" ] && [ -n "$ask_secret" ] && { echo "--key 와 --password 중 하나만 준다" >&2; exit 1; }
[ "$os" = windows ] && [ -z "$ask_secret" ] && { echo "windows 는 --password 가 필요하다 (WinRM)" >&2; exit 1; }
if [ "$os" = windows ] && [ ${#inst_args[@]} -gt 0 ] && printf '%s\n' "${inst_args[@]}" | grep -qx -- --role; then
  echo "--role 은 linux 만 된다 (하이퍼바이저 수신기는 KVM/libvirt)" >&2; exit 1
fi
if [ "$action" = install ]; then
  printf '%s\n' "${inst_args[@]}" | grep -qx -- --nodes || { echo "--nodes 가 필요하다" >&2; exit 1; }
fi

if [ -n "$ask_secret" ] && [ -z "${AGENT_PASSWORD:-}" ]; then
  read -rsp "$host 비밀번호: " AGENT_PASSWORD </dev/tty; echo >&2
fi
export AGENT_PASSWORD

version=$(awk '$1 == "version:" {print $2; exit}' agent/builder-config.yaml)
if [ "$os" = linux ]; then pkg=cmp-otelcol-$version-linux-amd64; pkg_file=dist/$pkg.tar.gz
else pkg=cmp-otelcol-$version-windows-amd64; pkg_file=dist/$pkg.zip; fi
if [ "$action" = install ] && [ ! -s "$pkg_file" ]; then
  echo "$pkg_file 가 없다. make agent-package 를 먼저 돌린다" >&2; exit 1
fi

install_linux() {
  local ssh_opts=(-o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new) wrap=() sudo="" remote=/tmp/cmp-otelcol-install
  [ -n "$jump" ] && ssh_opts+=(-o "ProxyJump=$jump")
  if [ -n "$key" ]; then
    ssh_opts+=(-i "$key" -o IdentitiesOnly=yes -o BatchMode=yes)
  elif [ -n "$ask_secret" ]; then
    export SSHPASS=$AGENT_PASSWORD
    ssh_opts+=("-o" "PreferredAuthentications=password,keyboard-interactive" -o PubkeyAuthentication=no)
    wrap=(sshpass -e)
  fi
  # root 가 아니면 sudo. 비밀번호로 들어왔으면 sudo 비밀번호를 표준 입력으로 준다 (명령줄에 남기지 않는다)
  if [ "$user" != root ]; then
    if [ -n "$ask_secret" ]; then sudo="sudo -S -p ''"; else sudo="sudo -n"; fi
  fi
  local files=() cmd
  if [ "$action" = install ]; then
    files=("$pkg_file")
    cmd="tar xzf $remote/${pkg_file##*/} -C $remote && $sudo bash $remote/$pkg/install.sh $(printf '%q ' "${inst_args[@]}")"
  else
    files=(agent/uninstall.sh)
    cmd="$sudo bash $remote/uninstall.sh ${purge:+--purge}"
  fi
  echo "=== $host: 올리는 중 (${files[*]})"
  timeout 60 "${wrap[@]}" ssh -n "${ssh_opts[@]}" "$host" "rm -rf $remote && mkdir -p $remote"
  timeout 600 "${wrap[@]}" scp -q "${ssh_opts[@]}" "${files[@]}" "$host:$remote/"
  echo "=== $host: $action"
  local rc=0
  if [ -n "$ask_secret" ] && [ "$user" != root ]; then
    printf '%s\n' "$AGENT_PASSWORD" | timeout 600 "${wrap[@]}" ssh "${ssh_opts[@]}" "$host" "$cmd" || rc=$?
  else
    timeout 600 "${wrap[@]}" ssh -n "${ssh_opts[@]}" "$host" "$cmd" || rc=$?
  fi
  timeout 60 "${wrap[@]}" ssh -n "${ssh_opts[@]}" "$host" "rm -rf $remote" || true
  return "$rc"
}

install_windows() {
  local url="https://$ip:5986/wsman" tunnel_pid="" port i
  if [ -n "$jump" ]; then
    # 빈 로컬 포트를 골라 점프 호스트 너머의 5986 으로 터널을 연다
    port=$(python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
    timeout 3600 ssh -n -N -o ExitOnForwardFailure=yes -o ConnectTimeout=10 -L "127.0.0.1:$port:$ip:5986" "$jump" &
    tunnel_pid=$!
    # shellcheck disable=SC2064  # 지금의 PID 를 넣어 둔다
    trap "kill $tunnel_pid 2>/dev/null || true" EXIT
    for i in $(seq 1 20); do
      python3 -I -c "import socket; socket.create_connection(('127.0.0.1', $port), 1)" 2>/dev/null && break
      kill -0 "$tunnel_pid" 2>/dev/null || { echo "터널을 열지 못했다 ($jump → $ip:5986)" >&2; return 1; }
      [ "$i" = 20 ] && { echo "터널이 20초 안에 열리지 않았다" >&2; return 1; }
      sleep 1
    done
    url="https://127.0.0.1:$port/wsman"
    echo "=== 터널 127.0.0.1:$port → $jump → $ip:5986"
  fi
  local ps_args="" a
  # install.ps1 인자: --nodes → -Nodes 처럼 바꾸고 값은 작은따옴표로 감싼다
  for ((i = 0; i < ${#inst_args[@]}; i += 2)); do
    case ${inst_args[i]} in
      --nodes) a=-Nodes ;; --interval) a=-Interval ;; --failback) a=-Failback ;;
      --tls-verify) a=-TlsVerify ;; --ca) a=-CaFile ;; --start-key) a=-StartKey ;;
    esac
    ps_args="$ps_args $a '${inst_args[i+1]//\'/\'\'}'"
  done
  WINRM_URL=$url WINRM_USER=$user ACTION=$action PKG_FILE=$pkg_file PKG=$pkg PS_ARGS=$ps_args PURGE=$purge \
    python3 -I - <<'PY'
# WinRM 으로 묶음을 올리고(표준 입력으로 base64 줄을 보내 원격 PowerShell 이 파일로 쓴다) 설치 스크립트를 돌린다
import base64, os, sys
from winrm.protocol import Protocol

env = os.environ
p = Protocol(endpoint=env["WINRM_URL"], transport="ntlm", username=env["WINRM_USER"],
             password=env["AGENT_PASSWORD"], server_cert_validation="ignore",
             read_timeout_sec=900, operation_timeout_sec=880)
HEAD = "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; [Console]::OutputEncoding=[Text.Encoding]::UTF8\n"

def ps(script, data=None):
    enc = base64.b64encode((HEAD + script).encode("utf-16-le")).decode()
    shell = p.open_shell(codepage=65001)
    try:
        cmd = p.run_command(shell, "powershell.exe",
                            ["-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", enc])
        if data is not None:
            # 한 번에 보내는 양은 WinRM 봉투 한도(기본 500KB) 아래로 한다
            step = 192 * 1024
            for off in range(0, len(data), step):
                line = base64.b64encode(data[off:off + step]) + b"\r\n"
                p.send_command_input(shell, cmd, line, end=off + step >= len(data))
        out, err, rc = p.get_command_output(shell, cmd)
        p.cleanup_command(shell, cmd)
    finally:
        p.close_shell(shell)
    sys.stdout.write(out.decode("utf-8", "replace"))
    e = err.decode("utf-8", "replace")
    if e and not e.startswith("#< CLIXML"):
        sys.stderr.write(e)
    return rc

tmp = "C:\\Windows\\Temp\\cmp-otelcol-install"
if env["ACTION"] == "install":
    data = open(env["PKG_FILE"], "rb").read()
    print(f"=== 올리는 중 ({len(data) // 1024 // 1024}MB)", flush=True)
    rc = ps(f"""
if (Test-Path '{tmp}') {{ Remove-Item -Recurse -Force '{tmp}' }}
New-Item -ItemType Directory -Force '{tmp}' | Out-Null
$f = [IO.File]::Create('{tmp}\\pkg.zip')
try {{ while ($null -ne ($l = [Console]::In.ReadLine())) {{ if ($l) {{ $b = [Convert]::FromBase64String($l); $f.Write($b, 0, $b.Length) }} }} }}
finally {{ $f.Close() }}
Write-Output ("받음 " + (Get-Item '{tmp}\\pkg.zip').Length + " 바이트")
""", data)
    if rc != 0:
        sys.exit(rc)
    print("=== install", flush=True)
    rc = ps(f"""
$rc = 0
try {{
  Expand-Archive -Force -Path '{tmp}\\pkg.zip' -DestinationPath '{tmp}'
  & '{tmp}\\{env["PKG"]}\\install.ps1' {env["PS_ARGS"]}
}} catch {{ Write-Output ("실패: " + $_); $rc = 1 }}
finally {{ Remove-Item -Recurse -Force '{tmp}' -ErrorAction SilentlyContinue }}
exit $rc
""")
else:
    # 지우기 스크립트는 작아서 명령에 실어 보낸다
    body = open("agent/windows/uninstall.ps1", encoding="utf-8-sig").read()
    b64 = base64.b64encode(body.encode("utf-8")).decode()
    purge = "-Purge" if env["PURGE"] else ""
    print("=== uninstall", flush=True)
    rc = ps(f"""
$rc = 0
try {{
  $s = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('{b64}'))
  & ([scriptblock]::Create($s)) {purge}
}} catch {{ Write-Output ("실패: " + $_); $rc = 1 }}
exit $rc
""")
sys.exit(rc)
PY
}

if [ "$os" = linux ]; then install_linux; else install_windows; fi

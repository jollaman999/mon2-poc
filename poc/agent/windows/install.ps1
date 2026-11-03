# 에이전트(cmp-otelcol)를 이 Windows 호스트에 서비스로 설치한다. 묶음(cmp-otelcol-<버전>-windows-amd64)을 푼 폴더에서 관리자로 돌린다.
# 다시 돌려도 된다: 실행 파일 · 설정을 새것으로 바꾸고 서비스를 다시 띄운다. 큐(data)는 그대로 둔다.
# 사용: .\install.ps1 -Nodes https://A:4318,https://B:4318 [-Interval 10s] [-Failback 1m]
#                     [-TlsVerify skip|ca] [-CaFile <사설 CA pem>] [-StartKey <값>]
# 기준 폴더 C:\cmp-agent 아래: bin(실행 파일) · conf(설정) · data(file_storage 디스크 큐) · logs(에이전트 로그)
# host.id 는 Win32_ComputerSystemProduct.UUID(소문자)로 덮어쓴다. 하이퍼바이저가 보는 VM uuid 와 같은 값이다. 못 읽으면 넣지 않는다.
# host.mac 은 물리 NIC MAC 을 소문자 콜론 꼴로 정렬해 쉼표로 이은 것이다. 하이퍼바이저가 보는 VM 의 NIC MAC 목록과 같은 꼴이다.
# exec · vsphere 수신기와 winpackettrace 의 포트는 설정 파일(conf\config.yaml)에 예시만 있다. 고쳐서 켠다.
param(
  [Parameter(Mandatory = $true)][string[]]$Nodes,
  [string]$Interval = "10s",
  [string]$Failback = "1m",
  [ValidateSet("skip", "ca")][string]$TlsVerify = "skip",
  [string]$CaFile = "",
  [string]$StartKey = ""
)
$ErrorActionPreference = "Stop"
$Name = "cmp-otelcol"
$Base = "C:\cmp-agent"
$BinDir = "$Base\bin"; $ConfDir = "$Base\conf"; $DataDir = "$Base\data"; $LogDir = "$Base\logs"
$Src = $PSScriptRoot
# 원격(WinRM)에서 돌면 PowerShell 5.1 은 외부 프로그램의 stderr 를 오류로 바꿔 Stop 에 걸린다. 외부 프로그램은 이것으로 부른다
function Invoke-Native([scriptblock]$Cmd) {
  $old = $ErrorActionPreference; $ErrorActionPreference = "Continue"
  try { & $Cmd 2>&1 | ForEach-Object { "$_" } } finally { $ErrorActionPreference = $old }
}

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw "관리자로 돌린다" }
foreach ($f in "$Name.exe", "config.yaml.tmpl", "uninstall.ps1") {
  if (-not (Test-Path "$Src\$f")) { throw "묶음에 $f 가 없다. 묶음을 푼 폴더에서 돌린다" }
}
# 쉼표 하나로 받은 목록도 나눈다 (원격에서 문자열 하나로 넘어오는 경우)
$list = @($Nodes | ForEach-Object { $_ -split "," } | Where-Object { $_ -ne "" })
foreach ($n in $list) { if ($n -notmatch "^https?://") { throw "주소는 http(s):// 로 시작한다: $n" } }
$endpoints = ($list | ForEach-Object { '"' + $_ + '"' }) -join ", "
# YAML 안에서 역슬래시가 이스케이프로 읽히지 않도록 경로는 / 로 쓴다 (Windows 도 / 경로를 받는다)
$y = { param($p) $p -replace '\\', '/' }
if ($TlsVerify -eq "ca") {
  if (-not $CaFile -or -not (Test-Path $CaFile)) { throw "-TlsVerify ca 에는 -CaFile <pem> 이 필요하다" }
  $tlsLine = "ca_file: " + (& $y "$ConfDir\ca.pem")
} else {
  $tlsLine = "insecure_skip_verify: true"
}
# VM 이면 하이퍼바이저가 붙인 uuid 다. 형식이 uuid 가 아니면(빈 값, 제조사가 채우지 않은 값) 넣지 않는다
$hostId = ""
try { $hostId = "$((Get-CimInstance -ClassName Win32_ComputerSystemProduct).UUID)".ToLowerInvariant() } catch { $hostId = "" }
if ($hostId -notmatch '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') { $hostId = "" }
# 물리 NIC 만 (VirtIO · 실제 카드). 가상 어댑터와 루프백은 빠진다. Windows 는 FA-16-3E-.. 꼴로 준다
$hostMac = ""
try {
  $hostMac = (@(Get-NetAdapter -Physical | ForEach-Object { "$($_.MacAddress)".ToLowerInvariant() -replace '-', ':' } |
      Where-Object { $_ -match '^([0-9a-f]{2}:){5}[0-9a-f]{2}$' -and $_ -ne '00:00:00:00:00:00' }) |
    Sort-Object -Unique) -join ","
} catch { $hostMac = "" }
$resourceProc = if ($hostId -or $hostMac) { ", resource" } else { "" }

Write-Output "[1/4] 폴더"
foreach ($d in $Base, $BinDir, $ConfDir, $DataDir, $LogDir) { New-Item -ItemType Directory -Force -Path $d | Out-Null }

Write-Output "[2/4] 실행 파일"
# 돌고 있는 실행 파일은 덮어쓸 수 없으므로 서비스를 먼저 멈춘다
$svc = Get-Service -Name $Name -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -ne "Stopped") { Stop-Service -Name $Name -Force; $svc.WaitForStatus("Stopped", "00:00:30") }
Copy-Item -Force "$Src\$Name.exe" "$BinDir\$Name.exe"
Copy-Item -Force "$Src\uninstall.ps1" "$BinDir\uninstall.ps1"
Invoke-Native { & "$BinDir\$Name.exe" --version }

Write-Output "[3/4] 설정"
if ($TlsVerify -eq "ca") { Copy-Item -Force $CaFile "$ConfDir\ca.pem" }
$cfg = Get-Content -Raw -Encoding UTF8 "$Src\config.yaml.tmpl"
# 못 읽은 값의 resource 줄을 지운다
foreach ($drop in @(@{ k = "@HOST_ID@"; v = $hostId }, @{ k = "@HOST_MAC@"; v = $hostMac })) {
  if (-not $drop.v) { $cfg = (($cfg -split "`n") | Where-Object { -not $_.Contains($drop.k) }) -join "`n" }
}
$vals = [ordered]@{
  "@QUEUE_DIR@" = (& $y $DataDir); "@INTERVAL@" = $Interval; "@ENDPOINTS@" = $endpoints; "@START_KEY@" = $StartKey
  "@FAILBACK@" = $Failback; "@TLS@" = $tlsLine; "@LOG_FILE@" = (& $y "$LogDir\$Name.log")
  "@HOST_ID@" = $hostId; "@HOST_MAC@" = $hostMac; "@RESOURCE_PROC@" = $resourceProc
}
foreach ($k in $vals.Keys) { $cfg = $cfg.Replace($k, $vals[$k]) }
$new = "$ConfDir\.config.yaml.new"
# BOM 없는 UTF-8 로 쓴다
[IO.File]::WriteAllText($new, $cfg, (New-Object Text.UTF8Encoding($false)))
Invoke-Native { & "$BinDir\$Name.exe" validate --config $new }
if ($LASTEXITCODE -ne 0) { throw "설정 검사 실패: $new" }
Move-Item -Force $new "$ConfDir\config.yaml"
$hostIdText = if ($hostId) { $hostId } else { "(없음)" }
$hostMacText = if ($hostMac) { $hostMac } else { "(없음)" }
Write-Output "노드 $($list -join ','), 수집 주기 $Interval, 돌아가기 $Failback, TLS $TlsVerify, host.id $hostIdText, host.mac $hostMacText"

Write-Output "[4/4] 서비스"
$binPath = "`"$BinDir\$Name.exe`" --config `"$ConfDir\config.yaml`""
if (-not $svc) {
  New-Service -Name $Name -BinaryPathName $binPath -DisplayName "cmp-otelcol 에이전트" -StartupType Automatic `
    -Description "호스트 지표 · 로그 · 트레이스를 중앙 노드로 보낸다" | Out-Null
} else {
  Invoke-Native { & sc.exe config $Name binPath= $binPath start= auto } | Out-Null
}
# 실패하면 5초 뒤 다시 띄운다 (하루 동안 실패 횟수를 센다). 정상 종료가 아닌 멈춤에도 적용한다
Invoke-Native { & sc.exe failure $Name reset= 86400 actions= restart/5000/restart/5000/restart/5000 } | Out-Null
Invoke-Native { & sc.exe failureflag $Name 1 } | Out-Null
Start-Service -Name $Name
Start-Sleep -Seconds 3
$svc = Get-Service -Name $Name
if ($svc.Status -ne "Running") {
  Get-Content -Tail 20 "$LogDir\$Name.log" -ErrorAction SilentlyContinue
  throw "서비스가 뜨지 않았다: $($svc.Status)"
}
Write-Output "설치 끝: $Name $($svc.Status), 로그 $LogDir\$Name.log"

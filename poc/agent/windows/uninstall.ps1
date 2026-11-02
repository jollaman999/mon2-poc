# install.ps1 이 깐 것을 지운다: 서비스 · C:\cmp-agent 의 bin · conf · logs.
# 큐(data)는 -Purge 일 때만 지운다 (아직 못 보낸 데이터가 남아 있을 수 있다).
# 사용: .\uninstall.ps1 [-Purge]   (관리자)
param([switch]$Purge)
$ErrorActionPreference = "Stop"
$Name = "cmp-otelcol"
$Base = "C:\cmp-agent"

$svc = Get-Service -Name $Name -ErrorAction SilentlyContinue
if ($svc) {
  if ($svc.Status -ne "Stopped") { Stop-Service -Name $Name -Force; $svc.WaitForStatus("Stopped", "00:00:30") }
  $old = $ErrorActionPreference; $ErrorActionPreference = "Continue"
  & sc.exe delete $Name 2>&1 | ForEach-Object { "$_" }
  $ErrorActionPreference = $old
}
# 기준 폴더를 다른 것과 같이 쓸 수 있으므로 우리가 만든 하위 폴더만 지우고, 기준 폴더는 비었을 때만 지운다.
# 이 스크립트가 bin 안에 있어도 PowerShell 은 읽어 둔 뒤 돌기 때문에 지울 수 있다
$dirs = @("bin", "conf", "logs")
if ($Purge) { $dirs += "data" }
foreach ($d in $dirs) { if (Test-Path "$Base\$d") { Remove-Item -Recurse -Force "$Base\$d" } }
if ($Purge) { Write-Output "큐 데이터 삭제" }
elseif (Test-Path "$Base\data") { Write-Output "큐 데이터는 남김: $Base\data (지우려면 -Purge)" }
if ((Test-Path $Base) -and -not (Get-ChildItem -Force $Base)) { Remove-Item -Force $Base }
elseif (Test-Path $Base) { Write-Output "$Base 에 다른 파일이 있어 폴더는 남김" }
Write-Output "삭제 끝: $Name"

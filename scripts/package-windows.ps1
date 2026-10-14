# package-windows.ps1 — build the Windows zip package (installer + uninstaller).
# Output: dist/all2api-<version>-windows-amd64.zip containing
#   all2api.exe            gateway binary (version-injected)
#   config.example.json    shipped as the template for a first-run config.json
#   install.ps1/.bat       installer (per-machine or per-user, shortcuts, optional firewall+autostart)
#   uninstall.ps1/.bat     uninstaller (default keeps user data; -Purge removes it)
#   README.txt             install/uninstall/port-setting instructions (zh)
#
# Usage:  pwsh -File scripts/package-windows.ps1 [-OutDir dist] [-SkipPanel]
# The panel bundle is embedded when Node is available; without it the package
# ships the placeholder page (same semantics as the Docker build).

[CmdletBinding()]
param(
  [string]$OutDir = "dist",
  [switch]$SkipPanel
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

$version = (git describe --tags --always --dirty 2>$null)
if (-not $version) { $version = "dev" }
$commit = (git rev-parse --short HEAD 2>$null)
if (-not $commit) { $commit = "unknown" }
Write-Host "== packaging all2api $version ($commit)"

$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) {
  foreach ($cand in @("$env:ProgramFiles\Go\bin\go.exe", "$env:LOCALAPPDATA\Programs\Go\bin\go.exe")) {
    if (Test-Path $cand) { $go = $cand; Write-Host "== go found at $cand"; break }
  }
}
if (-not $go) { throw "go not on PATH — install Go >= 1.26 first" }
elseif ($go -is [string]) { $env:Path = (Split-Path -Parent $go) + ";" + $env:Path }

# ---- panel bundle (best effort; same truth source as the Docker web stage) ----
if (-not $SkipPanel) {
  $panelOk = $false
  if (Test-Path "web-panel/package.json") {
    $real = @(Get-ChildItem "web-panel/dist" -Recurse -File -ErrorAction SilentlyContinue | Where-Object Name -ne ".gitkeep")
    if ($real.Count -eq 0) {
      $npm = Get-Command npm -ErrorAction SilentlyContinue
      if ($npm) {
        Write-Host "== building panel bundle (vite)"
        Push-Location web-panel
        npm ci
        npm run build
        Pop-Location
      } else {
        Write-Warning "npm not found — package will embed the placeholder panel"
      }
    } else {
      Write-Host "== reusing existing web-panel/dist ($($real.Count) files)"
    }
    if (Test-Path "web-panel/dist/index.html") {
      New-Item -ItemType Directory -Force -Path "internal/web/dist" | Out-Null
      Copy-Item "web-panel/dist/*" "internal/web/dist/" -Recurse -Force
      $panelOk = $true
    }
  }
  if (-not $panelOk) { Write-Warning "no panel bundle — placeholder page will be served" }
} else {
  Write-Host "== SkipPanel: embedding whatever internal/web/dist currently holds"
}

# ---- build ----
Write-Host "== building all2api.exe"
New-Item -ItemType Directory -Force -Path "$OutDir/pkg" | Out-Null
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w -X main.version=$version -X main.commit=$commit" -o "$OutDir/pkg/all2api.exe" ./cmd/gateway
if ($LASTEXITCODE -ne 0) { throw "go build failed" }

# ---- stage ----
$stage = "$OutDir/pkg"
Copy-Item "config.example.json" "$stage/config.example.json" -Force

$ld = '$env:ProgramFiles'
Write-Host "== writing installer scripts"
@'
# install.ps1 — all2api Windows installer.
# Default install is per-machine (Program Files, needs an elevated prompt);
# pass -PerUser to install into %LOCALAPPDATA%\Programs\all2api instead.
# User data (config.json / auths\ / data\) is never overwritten by an upgrade.
[CmdletBinding()]
param(
  [string]$InstallDir = "",
  [switch]$PerUser,
  [switch]$Autostart,
  [switch]$Firewall,
  [switch]$NoShortcuts
)
$ErrorActionPreference = "Stop"
$src = Split-Path -Parent $MyInvocation.MyCommand.Path
if (-not $InstallDir) {
  $InstallDir = if ($PerUser) { Join-Path $env:LOCALAPPDATA "Programs\all2api" } else { Join-Path $env:ProgramFiles "all2api" }
}
if (-not $PerUser -and -not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  Write-Warning "Per-machine install without elevation will likely fail; re-run as administrator or pass -PerUser."
}
Write-Host "== installing all2api -> $InstallDir"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item (Join-Path $src "all2api.exe") $InstallDir -Force
if (-not (Test-Path (Join-Path $InstallDir "config.json"))) {
  Copy-Item (Join-Path $src "config.example.json") (Join-Path $InstallDir "config.json")
  Write-Host "== first install: config.json created from example (edit listen/api_key there)"
}
if (-not $NoShortcuts) {
  $ws = New-Object -ComObject WScript.Shell
  $smDir = Join-Path ([Environment]::GetFolderPath("Programs")) "all2api"
  New-Item -ItemType Directory -Force -Path $smDir | Out-Null
  $lnk = $ws.CreateShortcut((Join-Path $smDir "all2api.lnk"))
  $lnk.TargetPath = Join-Path $InstallDir "all2api.exe"
  $lnk.WorkingDirectory = $InstallDir
  $lnk.Save()
  $dl = $ws.CreateShortcut((Join-Path ([Environment]::GetFolderPath("DesktopDirectory")) "all2api.lnk"))
  $dl.TargetPath = $lnk.TargetPath
  $dl.WorkingDirectory = $InstallDir
  $dl.Save()
}
if ($Autostart) {
  $ws = New-Object -ComObject WScript.Shell
  $lnk = $ws.CreateShortcut((Join-Path ([Environment]::GetFolderPath("Startup")) "all2api.lnk"))
  $lnk.TargetPath = Join-Path $InstallDir "all2api.exe"
  $lnk.WorkingDirectory = $InstallDir
  $lnk.Save()
  Write-Host "== autostart shortcut written (runs at logon)"
}
if ($Firewall) {
  netsh advfirewall firewall delete rule name="all2api" | Out-Null
  netsh advfirewall firewall add rule name="all2api" dir=in action=allow protocol=TCP localport=7863 | Out-Null
  Write-Host "== firewall rule added (TCP 7863 inbound)"
}
Write-Host ""
Write-Host "all2api installed."
Write-Host "  config:  $InstallDir\config.json  (port = listen, default :7863)"
Write-Host "  panel:   http://localhost:7863 after starting the gateway"
Write-Host "  start:   run all2api.exe (or the Start Menu shortcut)"
'@ | Set-Content "$stage/install.ps1" -Encoding UTF8

@'
# uninstall.ps1 — all2api uninstaller.
# Default: removes the binary, shortcuts and firewall rule but KEEPS your data
# (config.json / auths\ credentials / data\ state). Pass -Purge to delete those too.
[CmdletBinding()]
param(
  [string]$InstallDir = "",
  [switch]$PerUser,
  [switch]$Purge
)
$ErrorActionPreference = "Continue"
if (-not $InstallDir) {
  $InstallDir = if ($PerUser) { Join-Path $env:LOCALAPPDATA "Programs\all2api" } else { Join-Path $env:ProgramFiles "all2api" }
}
Write-Host "== uninstalling all2api from $InstallDir"
Get-Process all2api -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Milliseconds 500
$ws = New-Object -ComObject WScript.Shell
foreach ($dir in @([Environment]::GetFolderPath("Programs"), [Environment]::GetFolderPath("DesktopDirectory"), [Environment]::GetFolderPath("Startup"))) {
  $p = Join-Path $dir "all2api.lnk"
  if (Test-Path $p) { Remove-Item $p -Force }
}
$smDir = Join-Path ([Environment]::GetFolderPath("Programs")) "all2api"
if (Test-Path $smDir) { Remove-Item $smDir -Recurse -Force }
netsh advfirewall firewall delete rule name="all2api" | Out-Null
$exe = Join-Path $InstallDir "all2api.exe"
if (Test-Path $exe) { Remove-Item $exe -Force }
if ($Purge) {
  if (Test-Path $InstallDir) { Remove-Item $InstallDir -Recurse -Force }
  Write-Host "== purge: install dir removed (credentials and state deleted)"
} else {
  Write-Host "== kept user data in $InstallDir (config.json, auths\, data\). Delete the folder manually or re-run with -Purge."
}
Write-Host "all2api uninstalled."
'@ | Set-Content "$stage/uninstall.ps1" -Encoding UTF8

@'
@echo off
rem Double-click wrapper: runs install.ps1 next to this file.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
pause
'@ | Set-Content "$stage/install.bat" -Encoding ASCII
@'
@echo off
rem Double-click wrapper: runs uninstall.ps1 next to this file.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0uninstall.ps1" %*
pause
'@ | Set-Content "$stage/uninstall.bat" -Encoding ASCII

@'
all2api — Windows 安装说明
==========================

安装
  双击 install.bat（默认装到 Program Files，需要管理员确认），
  或在 PowerShell 里执行： .\install.ps1 -PerUser        （装到当前用户目录，无需管理员）
  可选参数： -Autostart（开机自启）  -Firewall（放行 7863 端口入站）

设置端口
  方式一：安装目录下的 config.json，改 "listen": ":7863" 为如 ":9000"，重启 all2api 生效。
  方式二：浏览器打开管理面板 http://localhost:7863 → 设置屏 → gateway 分组 → listen 字段，
          保存后按提示重启生效（面板保存走 POST /api/config，落盘到同一份 config.json）。

账号凭证
  把 workbuddy-*.json 放到安装目录的 auths\ 子目录（没有就新建），
  重启 all2api 后自动加载；token 过期会自动刷新并回写该文件。

卸载
  双击 uninstall.bat。
  默认保留你的数据（config.json、auths\ 凭证、data\ 状态）；
  确认不再需要时用： powershell -File uninstall.ps1 -Purge   （连数据一起删除）
'@ | Set-Content "$stage/README.txt" -Encoding UTF8

# ---- zip ----
$zip = "$OutDir/all2api-$version-windows-amd64.zip"
if (Test-Path $zip) { Remove-Item $zip -Force }
Compress-Archive -Path "$stage/*" -DestinationPath $zip
Write-Host "== package: $zip"
Get-ChildItem $zip | Format-Table Name, Length

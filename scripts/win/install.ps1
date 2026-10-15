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
# Pre-seeded credentials (optional): a package built for one machine may carry
# an `auths-seed\` folder next to the installer. Its files are copied into
# auths\ only when that folder holds nothing yet, so re-installs and later
# refreshes never overwrite newer tokens.
$seed = Join-Path $src "auths-seed"
if (Test-Path $seed) {
  $authsDir = Join-Path $InstallDir "auths"
  $existing = @(Get-ChildItem $authsDir -File -ErrorAction SilentlyContinue)
  if ($existing.Count -eq 0) {
    New-Item -ItemType Directory -Force -Path $authsDir | Out-Null
    Copy-Item (Join-Path $seed "*") $authsDir -Force
    $n = @(Get-ChildItem $authsDir -File).Count
    Write-Host "== seeded $n credential file(s) into $authsDir"
  } else {
    Write-Host "== auths\ already holds $($existing.Count) file(s) — credential seed skipped (never overwrites newer tokens)"
  }
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
  netsh advfirewall firewall add rule name="all2api" dir=in action=allow protocol=TCP localport=7800 | Out-Null
  Write-Host "== firewall rule added (TCP 7800 inbound)"
}
Write-Host ""
Write-Host "all2api installed."
Write-Host "  config:  $InstallDir\config.json  (port = listen, default :7800)"
Write-Host "  panel:   http://localhost:7800 after starting the gateway"
Write-Host "  start:   run all2api.exe (or the Start Menu shortcut)"

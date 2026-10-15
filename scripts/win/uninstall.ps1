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

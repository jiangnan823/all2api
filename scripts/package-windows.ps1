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

# Installer payloads live as real files under scripts/win/ — the same files the
# release workflow stages into the GitHub Release zip, so local and CI packages
# cannot drift apart.
Write-Host "== staging installer scripts from scripts/win"
Copy-Item "scripts/win/install.ps1" "$stage/install.ps1" -Force
Copy-Item "scripts/win/uninstall.ps1" "$stage/uninstall.ps1" -Force
Copy-Item "scripts/win/install.bat" "$stage/install.bat" -Force
Copy-Item "scripts/win/uninstall.bat" "$stage/uninstall.bat" -Force
Copy-Item "scripts/win/README-package.txt" "$stage/README.txt" -Force

# ---- zip ----
$zip = "$OutDir/all2api-$version-windows-amd64.zip"
if (Test-Path $zip) { Remove-Item $zip -Force }
Compress-Archive -Path "$stage/*" -DestinationPath $zip
Write-Host "== package: $zip"
Get-ChildItem $zip | Format-Table Name, Length

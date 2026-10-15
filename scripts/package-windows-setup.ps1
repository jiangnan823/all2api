# package-windows-setup.ps1 — build the Windows installer(s) with Inno Setup.
#
# Produces a real setup executable: wizard with install-directory selection,
# optional tasks (desktop shortcut / autostart / firewall), Start Menu entry,
# Add-Remove Programs registration and a generated uninstaller that keeps user
# data. One standard setup, plus (when -SeedDir is given) a machine-personal
# setup whose installer seeds auths\ on first install only.
#
# Usage:
#   pwsh -File scripts/package-windows-setup.ps1                        # standard
#   pwsh -File scripts/package-windows-setup.ps1 -SeedDir <auths folder> # + personal
#
# Requires: Go (auto-probed), Node for the panel bundle (optional — reuses an
# existing web-panel/dist), Inno Setup 6 (ISCC on PATH or in the default
# install locations; install with: winget install JRSoftware.InnoSetup).

[CmdletBinding()]
param(
  [string]$OutDir = "dist",
  [string]$SeedDir = "",
  [switch]$SkipPanel
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

$version = (git describe --tags --always 2>$null)
if (-not $version) { $version = "dev" }
$commit = (git rev-parse --short HEAD 2>$null)
if (-not $commit) { $commit = "unknown" }
Write-Host "== building all2api installer $version ($commit)"

# ---- toolchain probes -------------------------------------------------------
$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) {
  foreach ($cand in @("$env:ProgramFiles\Go\bin\go.exe", "$env:LOCALAPPDATA\Programs\Go\bin\go.exe")) {
    if (Test-Path $cand) { $go = $cand; Write-Host "== go found at $cand"; break }
  }
}
if (-not $go) { throw "go not on PATH — install Go >= 1.26 first" }
elseif ($go -is [string]) { $env:Path = (Split-Path -Parent $go) + ";" + $env:Path }

$iscc = Get-Command ISCC.exe -ErrorAction SilentlyContinue
if (-not $iscc) {
  foreach ($cand in @(
      "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe",
      "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe",
      "$env:ProgramFiles\Inno Setup 6\ISCC.exe")) {
    if (Test-Path $cand) { $iscc = $cand; break }
  }
}
if (-not $iscc) { throw "ISCC.exe not found — install Inno Setup 6: winget install JRSoftware.InnoSetup" }
Write-Host "== ISCC: $iscc"

# ---- panel bundle (best effort; same truth source as the Docker web stage) --
if (-not $SkipPanel -and (Test-Path "web-panel/package.json")) {
  $real = @(Get-ChildItem "web-panel/dist" -Recurse -File -ErrorAction SilentlyContinue | Where-Object Name -ne ".gitkeep")
  if ($real.Count -eq 0) {
    if (Get-Command npm -ErrorAction SilentlyContinue) {
      Write-Host "== building panel bundle (vite)"
      Push-Location web-panel; npm ci; npm run build; Pop-Location
    } else {
      Write-Warning "npm not found — installer will embed the placeholder panel"
    }
  } else {
    Write-Host "== reusing existing web-panel/dist ($($real.Count) files)"
  }
  if (Test-Path "web-panel/dist/index.html") {
    New-Item -ItemType Directory -Force -Path "internal/web/dist" | Out-Null
    Copy-Item "web-panel/dist/*" "internal/web/dist/" -Recurse -Force
  }
}

# ---- stage & build ----------------------------------------------------------
$stage = Join-Path $OutDir "inst-stage"
New-Item -ItemType Directory -Force -Path $stage | Out-Null
Write-Host "== building all2api.exe"
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w -X main.version=$version -X main.commit=$commit" -o (Join-Path $stage "all2api.exe") ./cmd/gateway
if ($LASTEXITCODE -ne 0) { throw "go build failed" }
Copy-Item "config.example.json" $stage -Force

$common = @(
  "/DMyVersion=$version",
  "/DStageDir=$(Resolve-Path $stage)",
  "/DOutputDir=$(Resolve-Path $OutDir)"
)
$iss = "scripts/win/all2api.iss"

Write-Host "== compiling standard setup"
& $iscc @common "/DOutputBaseName=all2api-setup-$version" $iss
if ($LASTEXITCODE -ne 0) { throw "ISCC failed (standard)" }

if ($SeedDir) {
  if (-not (Test-Path $SeedDir)) { throw "SeedDir not found: $SeedDir" }
  $seed = Join-Path $OutDir "inst-seed"
  Remove-Item $seed -Recurse -Force -ErrorAction SilentlyContinue
  New-Item -ItemType Directory -Force -Path $seed | Out-Null
  Copy-Item (Join-Path $SeedDir "*.json") $seed -Force
  $n = @(Get-ChildItem $seed -File).Count
  if ($n -eq 0) { throw "SeedDir holds no .json credentials: $SeedDir" }
  Write-Host "== compiling personal setup (seeding $n credential file(s))"
  & $iscc @common "/DOutputBaseName=all2api-setup-$version-personal" "/DSeedDir=$(Resolve-Path $seed)" $iss
  if ($LASTEXITCODE -ne 0) { throw "ISCC failed (personal)" }
}

Write-Host ""
Write-Host "== installers:"
Get-ChildItem (Join-Path $OutDir "all2api-setup-*.exe") | Sort-Object LastWriteTime | Format-Table Name, @{N='MB';E={[math]::Round($_.Length/1MB,1)}} -AutoSize

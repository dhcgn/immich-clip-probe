# Generates app icons from brand/logo.svg. Needs nothing but ImageMagick.
# Run: pwsh brand/Generate-Icons.ps1 (from the repo root, or anywhere)
$ErrorActionPreference = "Stop"
$Magick = "C:\Program Files\ImageMagick-7.1.2-Q16-HDRI\magick.exe"
$Root = Split-Path $PSScriptRoot -Parent
$Logo = Join-Path $PSScriptRoot "logo.svg"

function Svg-Png([string]$Out, [int]$Size) {
  & $Magick -background none -density 384 $Logo -resize "${Size}x${Size}" $Out
}

# Web/brand sizes.
Svg-Png (Join-Path $PSScriptRoot "icon-16.png") 16
Svg-Png (Join-Path $PSScriptRoot "icon-32.png") 32
Svg-Png (Join-Path $PSScriptRoot "icon-48.png") 48
Svg-Png (Join-Path $PSScriptRoot "apple-touch-icon.png") 180
Svg-Png (Join-Path $PSScriptRoot "icon-192.png") 192
Svg-Png (Join-Path $PSScriptRoot "icon-512.png") 512
& $Magick -background none -density 384 $Logo -define icon:auto-resize=16,32,48 (Join-Path $PSScriptRoot "favicon.ico")

# Desktop (Wails v3): one set, replaced. build/appicon.png is the source that
# `wails3 generate icons` turns into windows/icon.ico + darwin/icons.icns.
Svg-Png (Join-Path $Root "tools/desktop/build/appicon.png") 1024
$Public = Join-Path $Root "tools/desktop/frontend/public"
Copy-Item $Logo (Join-Path $Public "favicon.svg") -Force
& $Magick -background none -density 384 $Logo -resize "32x32" (Join-Path $Public "favicon-32.png")

# CLI: Windows exe icon (embedded via the .syso files below).
& $Magick -background none -density 384 $Logo -define icon:auto-resize=16,32,48,64,128,256 (Join-Path $Root "tools/clip-check/icon.ico")

# Refresh the Wails .ico/.icns from the new appicon.png when wails3 is around.
if (Get-Command wails3 -ErrorAction SilentlyContinue) {
  Push-Location (Join-Path $Root "tools/desktop/build")
  try { wails3 generate icons -input appicon.png -macfilename darwin/icons.icns -windowsfilename windows/icon.ico -iconcomposerinput appicon.icon -macassetdir darwin }
  finally { Pop-Location }
  # CLI: embed icon.ico into the Windows exe via committed .syso files.
  Push-Location (Join-Path $Root "tools/clip-check")
  try {
    wails3 generate syso -arch amd64 -icon icon.ico -manifest clip-check.exe.manifest -info info.json -out clip-check_windows_amd64.syso
    wails3 generate syso -arch arm64 -icon icon.ico -manifest clip-check.exe.manifest -info info.json -out clip-check_windows_arm64.syso
  } finally { Pop-Location }
} else {
  Write-Warning "wails3 not on PATH: run 'wails3 generate icons' in tools/desktop/build yourself"
}

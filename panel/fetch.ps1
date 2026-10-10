#
# fetch.ps1 - gets the CPE Box binaries on Windows, so running CPE Box
# doesn't need Go. Dot-sourced by start_gui.ps1 and setup.ps1.
#
#   Ensure-GuiBin          -> panel\cpe-box.exe, built from source when Go is
#                             installed, otherwise the latest GitHub release
#                             (re-downloaded when a newer one is out).
#   Fetch-SmsReader $dir   -> $dir\sms-reader for the router (ARMv7).
#
# $env:CPEBOX_NO_BUILD = '1' skips building even with Go installed.

$CpeBoxRepo = if ($env:CPEBOX_REPO) { $env:CPEBOX_REPO } else { 'Kreal-exe/CPE-Box-cb0401' }
$FetchDir = $PSScriptRoot
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Test-Go {
    # Go 1.21+ is what the code targets (embed, min/max, ...). Older versions
    # fail at "go build" with unhelpful errors, so we skip building and use a
    # prebuilt release instead.
    if ($env:CPEBOX_NO_BUILD) { return $false }
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) { return $false }
    try {
        $v = (& go env GOVERSION 2>$null) -replace '^go', ''
        $parts = $v.Split('.')
        $major = [int]$parts[0]; $minor = [int]$parts[1]
        return ($major -gt 1) -or ($major -eq 1 -and $minor -ge 21)
    } catch { return $false }
}

function Get-LatestTag {
    try {
        (Invoke-RestMethod -Uri "https://api.github.com/repos/$CpeBoxRepo/releases/latest" -TimeoutSec 15 -UseBasicParsing).tag_name
    } catch { $null }
}

function Get-ReleaseAsset($Tag, $Asset, $Dest) {
    $tmp = "$Dest.download"
    try {
        Invoke-WebRequest -Uri "https://github.com/$CpeBoxRepo/releases/download/$Tag/$Asset" -OutFile $tmp -UseBasicParsing -TimeoutSec 300
        Move-Item -Force $tmp $Dest
        return $true
    } catch {
        Remove-Item -ErrorAction SilentlyContinue $tmp
        return $false
    }
}

function Ensure-GuiBin {
    $bin = Join-Path $FetchDir 'cpe-box.exe'
    $stamp = Join-Path $FetchDir '.release'
    if (Test-Go) {
        Write-Host 'Building CPE Box from source...'
        Push-Location $FetchDir
        # Stamp the version from the git tag, the same way build.sh does for
        # releases, so the panel doesn't show the placeholder version.
        $ver = (& git describe --tags --always 2>$null)
        if ($ver) { $ver = $ver.Trim() -replace '^v', '' } else { $ver = 'dev' }
        & go build -ldflags="-X main.appVersion=$ver" -o $bin . | Out-Host
        $ok = $LASTEXITCODE -eq 0
        Pop-Location
        if ($ok) { return $true }
        Write-Host 'Build failed - trying a prebuilt release instead.'
    }
    $tag = Get-LatestTag
    if (-not $tag) {
        if (Test-Path $bin) { Write-Host "Couldn't check GitHub for updates - using the CPE Box already here."; return $true }
        Write-Host "ERROR: couldn't reach GitHub to download CPE Box (and Go isn't installed to build it)."
        return $false
    }
    if ((Test-Path $bin) -and ((Get-Content $stamp -ErrorAction SilentlyContinue) -eq $tag)) { return $true }
    Write-Host "Downloading CPE Box $tag..."
    if (Get-ReleaseAsset $tag 'cpe-box-windows-amd64.exe' $bin) {
        Set-Content -Path $stamp -Value $tag -Encoding ascii
        return $true
    }
    if (Test-Path $bin) { Write-Host 'Download failed - using the CPE Box already here.'; return $true }
    Write-Host "ERROR: downloading CPE Box $tag failed."
    return $false
}

function Fetch-SmsReader($Dir) {
    # First try dumping the sms-reader out of the already-fetched cpe-box
    # (it ships embedded, see panel/embed_smsreader.go). Fall back to a
    # fresh cross-build if Go is present, then to a leftover dist/ copy.
    $out = Join-Path $Dir 'sms-reader'
    $src = Join-Path $FetchDir '..\router\sms-reader'
    $bin = Join-Path $FetchDir 'cpe-box.exe'
    if (Test-Path $bin) {
        & $bin --dump-sms-reader $out 2>$null | Out-Null
        if ($LASTEXITCODE -eq 0 -and (Test-Path $out)) { return $out }
    }
    if (Test-Go) {
        Push-Location $src
        $env:CGO_ENABLED = '0'; $env:GOOS = 'linux'; $env:GOARCH = 'arm'; $env:GOARM = '7'
        & go build -ldflags="-s -w" -o $out . | Out-Host
        $ok = $LASTEXITCODE -eq 0
        Remove-Item Env:\CGO_ENABLED, Env:\GOOS, Env:\GOARCH, Env:\GOARM
        Pop-Location
        if ($ok) { return $out }
    }
    $prebuilt = Join-Path $src 'dist\sms-reader-arm'
    if (Test-Path $prebuilt) { Copy-Item $prebuilt $out; return $out }
    return $null
}

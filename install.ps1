# Install wend on Windows.
#
#   irm https://raw.githubusercontent.com/dklymentiev/wend-router/main/install.ps1 | iex
#
# With Go 1.21 or newer it builds wend from source; with Docker it runs it in a
# container; with neither it asks whether to fetch Go just for the build or to download
# the ready binary. Everything goes into one folder (%LOCALAPPDATA%\wend; WEND_DIR to
# change it), because wend keeps its keys and settings next to the binary. Nothing needs
# administrator rights. Then it starts wend and sends one test request.
#
# WEND_BASE: where the release files come from (a mirror, or a local copy).

& {
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"   # the progress bar makes downloads many times slower
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$base = if ($env:WEND_BASE) { $env:WEND_BASE } else { "https://github.com/dklymentiev/wend-router/releases/latest/download" }
$dir = if ($env:WEND_DIR) { $env:WEND_DIR } else { Join-Path $env:LOCALAPPDATA "wend" }
$url = "http://127.0.0.1:8888"
# Windows' own tar (10 and newer) for the source tarball; a tar from Git or MSYS earlier in PATH misreads C:\ paths
$tar = Join-Path $env:SystemRoot "System32\tar.exe"
# under `irm | iex` an exit would close the user's window, so failures throw
function Fail($m) { throw "wend install: $m" }
function Have($c) { [bool](Get-Command $c -ErrorAction SilentlyContinue) }
function Fetch($from, $to) {
    try { Invoke-WebRequest -UseBasicParsing -Uri $from -OutFile $to } catch { Fail "could not download $from" }
}

$arch = switch ($env:PROCESSOR_ARCHITECTURE) { "AMD64" { "amd64" } "ARM64" { "arm64" } default { Fail "no build for a $($env:PROCESSOR_ARCHITECTURE) processor" } }
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("wend-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
New-Item -ItemType Directory -Force $tmp, $dir | Out-Null
if (-not (Test-Path $tar)) { Fail "this needs Windows 10 or newer (its tar.exe is missing)" }

# every downloaded release file is checked against SHA256SUMS of the same release
function Checked($name) {
    Fetch "$base/$name" "$tmp\$name"
    if (-not (Test-Path "$tmp\SHA256SUMS")) { Fetch "$base/SHA256SUMS" "$tmp\SHA256SUMS" }
    $want = (Get-Content "$tmp\SHA256SUMS" | Where-Object { ($_ -split "\s+\*?")[1] -eq $name } | ForEach-Object { ($_ -split "\s+")[0] })
    $got = (Get-FileHash -Algorithm SHA256 "$tmp\$name").Hash.ToLower()
    if (-not $want -or $want -ne $got) { Fail "checksum of $name does not match SHA256SUMS" }
}
function SourceTree($to) {
    Checked "wend-src.tar.gz"
    New-Item -ItemType Directory -Force $to | Out-Null
    & $tar -xzf "$tmp\wend-src.tar.gz" -C $to
    if ($LASTEXITCODE) { Fail "could not unpack the source" }
}
function Build($go) {
    Write-Host "Building wend from source with $(& $go env GOVERSION) ..."
    SourceTree "$tmp\src"
    $env:GOTOOLCHAIN = "auto"; $env:CGO_ENABLED = "0"
    Push-Location "$tmp\src"
    try { & $go build -trimpath -o "$tmp\wend.exe" ./cmd/wend } finally { Pop-Location }
    if ($LASTEXITCODE) { Fail "the build failed" }
}
function StopOld {
    $pidFile = Join-Path $dir "wend.pid"
    if (Test-Path $pidFile) {
        Stop-Process -Id ([int](Get-Content $pidFile)) -Force -ErrorAction SilentlyContinue
        Remove-Item $pidFile; Start-Sleep 1
    }
    try { Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 "$url/health" | Out-Null; $busy = $true } catch { $busy = $false }
    if ($busy) { Fail "something already answers on $url; stop it first (or run wend on another port with ROUTER_ADDR)" }
}
function StartBinary {
    StopOld
    Move-Item -Force "$tmp\wend.exe" "$dir\wend.exe"
    $p = Start-Process -FilePath "$dir\wend.exe" -WorkingDirectory $dir -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput "$dir\wend.log" -RedirectStandardError "$dir\wend.err.log"
    Set-Content -Path "$dir\wend.pid" -Value $p.Id
}

# Go 1.21 and newer fetch the toolchain go.mod asks for on their own
# In Windows PowerShell 5.1 a redirected stderr of a program throws under "Stop"
function GoOk {
    $ErrorActionPreference = "Continue"
    if (-not (Have go)) { return $false }
    $v = & go env GOVERSION 2>$null
    return ($v -match '^go1\.(\d+)' -and [int]$Matches[1] -ge 21)
}
function DockerOk {
    $ErrorActionPreference = "Continue"
    if (-not (Have docker)) { return $false }
    docker info *> $null; if ($LASTEXITCODE) { return $false }
    docker compose version *> $null; return (-not $LASTEXITCODE)
}

try {
    if (GoOk) { $how = "go" }
    elseif (DockerOk) { $how = "docker" }
    else {
        Write-Host "Neither Go nor Docker is here. wend can be"
        Write-Host "  1) built from source, with Go downloaded just for this build (about 70 MB, deleted after)"
        Write-Host "  2) downloaded ready-made: wend-windows-$arch.exe, checked against SHA256SUMS"
        $a = Read-Host "Choose 1 or 2 [1]"
        $how = switch ("$a".Trim()) { "" { "fetch-go" } "1" { "fetch-go" } "2" { "binary" } default { Fail "nothing chosen" } }
    }

    switch ($how) {
        "go" { Build "go"; StartBinary }
        "fetch-go" {
            $gv = ((Invoke-WebRequest -UseBasicParsing "https://go.dev/VERSION?m=text").Content -split "`n")[0].Trim()
            Write-Host "Downloading $gv for this build ..."
            Fetch "https://go.dev/dl/$gv.windows-$arch.zip" "$tmp\go.zip"
            # .NET's unzip: Windows' tar.exe rejects a few entries of Go's zip
            Add-Type -AssemblyName System.IO.Compression.FileSystem
            try { [IO.Compression.ZipFile]::ExtractToDirectory("$tmp\go.zip", $tmp) } catch { Fail "could not unpack Go: $_" }
            $env:GOPATH = "$tmp\gopath"; $env:GOCACHE = "$tmp\gocache"
            Build "$tmp\go\bin\go.exe"
            StartBinary
        }
        "binary" {
            Checked "wend-windows-$arch.exe"
            Move-Item -Force "$tmp\wend-windows-$arch.exe" "$tmp\wend.exe"
            StartBinary
        }
        "docker" {
            # an earlier container goes down first; the source is unpacked over the old one,
            # which keeps a wend.env put next to compose.yaml
            if (Test-Path "$dir\src\compose.yaml") { Push-Location "$dir\src"; docker compose -p wend down; Pop-Location }
            StopOld
            SourceTree "$dir\src"
            Write-Host "Building and starting the container ..."
            Push-Location "$dir\src"
            try { docker compose -p wend up -d --build } finally { Pop-Location }
            if ($LASTEXITCODE) { Fail "docker compose failed" }
        }
    }

    Write-Host "Waiting for Wend router to start ..."
    $up = $false
    for ($i = 0; $i -lt 60 -and -not $up; $i++) {
        try { Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 "$url/health" | Out-Null; $up = $true } catch { Start-Sleep 1 }
    }
    if (-not $up) { Fail "wend did not start; see $dir\wend.err.log" }

    # One real request. Right after the start the router is still reading the providers'
    # model lists, so give it a minute.
    Write-Host "Sending a test request ..."
    $body = '{"model":"no-think","max_tokens":80,"messages":[{"role":"user","content":"Say hello to a new user of wend in one short sentence."}]}'
    $reply = $null
    for ($i = 0; $i -lt 6 -and -not $reply; $i++) {
        try { $reply = Invoke-RestMethod -Method Post -TimeoutSec 60 -ContentType "application/json" -Uri "$url/v1/chat/completions" -Body $body }
        catch { Start-Sleep 10 }
    }
    if ($reply) { Write-Host "$($reply.model) answered: $("$($reply.choices[0].message.content)".Trim())" }
    else { Write-Host "No model answered yet. Open the dashboard and add a provider key (some providers need none)." }

    Write-Host ""
    Write-Host "Wend router is running: $url/dashboard"
    Write-Host "  agents connect to $url/v1 with any API key and the model `"auto`""
    if ($how -eq "docker") {
        Write-Host "  stop:  cd `"$dir\src`"; docker compose -p wend down"
        Write-Host "  start: cd `"$dir\src`"; docker compose -p wend up -d"
    } else {
        Write-Host "  stop:  Stop-Process -Id (Get-Content `"$dir\wend.pid`")"
        Write-Host "  start: Start-Process `"$dir\wend.exe`" -WorkingDirectory `"$dir`""
        Write-Host "  its keys, settings and log are in $dir"
    }
    Start-Process "$url/dashboard"
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
}

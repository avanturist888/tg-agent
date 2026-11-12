# Runs only on worker. Output contains no runtime state or configuration.
param([Parameter(Mandatory=$true)][string]$Artifacts)
$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $PSScriptRoot
$Artifacts = [IO.Path]::GetFullPath($Artifacts)
if (Test-Path -LiteralPath $Artifacts) { throw 'Use a new artifact directory.' }
$savedCGO = $env:CGO_ENABLED
Push-Location $Root
try {
    if (-not (Get-Command gcc -ErrorAction SilentlyContinue)) { throw 'gcc required for tgstt.' }
    $commit = git rev-parse HEAD
    if ($LASTEXITCODE -ne 0) { throw 'Cannot read commit.' }
    $build = "$($commit.Substring(0, 12))-$(Get-Date -Format 'yyyyMMdd.HHmmss')"
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'Tests failed.' }
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'Vet failed.' }
    New-Item -ItemType Directory -Path $Artifacts | Out-Null
    go build -ldflags "-X tgagent/internal/config.Build=$build" -o (Join-Path $Artifacts 'tg.exe') ./cmd/tg
    if ($LASTEXITCODE -ne 0) { throw 'tg build failed.' }
    go build -ldflags "-H=windowsgui -X tgagent/internal/config.Build=$build" -o (Join-Path $Artifacts 'tgw.exe') ./cmd/tg
    if ($LASTEXITCODE -ne 0) { throw 'tgw build failed.' }
    $env:CGO_ENABLED = '1'
    go build -C stt -o (Join-Path $Artifacts 'tgstt.exe') .
    if ($LASTEXITCODE -ne 0) { throw 'tgstt build failed.' }
    $module = go list -C stt -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-windows
    if ($LASTEXITCODE -ne 0) { throw 'Cannot locate STT libraries.' }
    foreach ($dll in @('sherpa-onnx-c-api.dll', 'sherpa-onnx-cxx-api.dll', 'onnxruntime.dll')) {
        Copy-Item -LiteralPath (Join-Path $module "lib/x86_64-pc-windows-gnu/$dll") -Destination $Artifacts
    }
    $files = @(Get-ChildItem -LiteralPath $Artifacts -File | ForEach-Object {
        @{ name = $_.Name; sha256 = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash }
    })
    @{ schema = 1; commit = $commit; build = $build; files = $files } |
        ConvertTo-Json -Depth 4 | Set-Content -Encoding UTF8 -LiteralPath (Join-Path $Artifacts 'manifest.json')
} finally {
    $env:CGO_ENABLED = $savedCGO
    Pop-Location
}

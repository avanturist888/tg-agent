# Runs only on worker. Output contains no runtime state or configuration.
param([Parameter(Mandatory=$true)][string]$Artifacts)
$ErrorActionPreference = 'Stop'
if ($env:WK_BOOTSTRAP_ISOLATION -ne 'clean-v1' -or -not $env:TG_AGENT_HOME -or -not $env:T3CODE_HOME) {
    throw 'Run through installed wk with the project before hook and private environment.'
}
if (-not $env:WK_RUN_TMP -or -not $env:PSModuleAnalysisCachePath) {
    throw 'The before hook must set the PowerShell cache before process startup.'
}
$runTempRoot = [IO.Path]::GetFullPath($env:WK_RUN_TMP).TrimEnd([char[]]'\/') + [IO.Path]::DirectorySeparatorChar
$powerShellCache = [IO.Path]::GetFullPath($env:PSModuleAnalysisCachePath)
if (-not $powerShellCache.StartsWith($runTempRoot, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'PowerShell module cache must be inside the private run directory.'
}
$Root = Split-Path -Parent $PSScriptRoot
$Artifacts = [IO.Path]::GetFullPath($Artifacts)
if (Test-Path -LiteralPath $Artifacts) { throw 'Use a new artifact directory.' }
$savedCGO = $env:CGO_ENABLED
Push-Location $Root
try {
    if (-not (Get-Command gcc -ErrorAction SilentlyContinue)) { throw 'gcc required for tgstt.' }
    $commit = git rev-parse HEAD
    if ($LASTEXITCODE -ne 0) { throw 'Cannot read commit.' }
    $dirty = @(git status --porcelain --untracked-files=normal)
    if ($LASTEXITCODE -ne 0 -or $dirty.Count) { throw 'Commit the build source first.' }
    # bin is ignored: artifacts must not change VCS metadata of later binaries.
    $artifactRoot = [IO.Path]::GetFullPath((Join-Path $Root 'bin')) + [IO.Path]::DirectorySeparatorChar
    if (-not $Artifacts.StartsWith($artifactRoot, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Use a new artifact directory inside the mirror bin directory.'
    }
    $build = "$($commit.Substring(0, 12))-$(Get-Date -Format 'yyyyMMdd.HHmmss')"
    go test -mod=readonly -count=1 ./...
    if ($LASTEXITCODE -ne 0) { throw 'Tests failed.' }
    go vet -mod=readonly ./...
    if ($LASTEXITCODE -ne 0) { throw 'Vet failed.' }
    New-Item -ItemType Directory -Path $Artifacts | Out-Null
    go build -mod=readonly -ldflags "-X tgagent/internal/config.Build=$build" -o (Join-Path $Artifacts 'tg.exe') ./cmd/tg
    if ($LASTEXITCODE -ne 0) { throw 'tg build failed.' }
    go build -mod=readonly -ldflags "-H=windowsgui -X tgagent/internal/config.Build=$build" -o (Join-Path $Artifacts 'tgw.exe') ./cmd/tg
    if ($LASTEXITCODE -ne 0) { throw 'tgw build failed.' }
    $env:CGO_ENABLED = '1'
    go build -C stt -mod=readonly -o (Join-Path $Artifacts 'tgstt.exe') .
    if ($LASTEXITCODE -ne 0) { throw 'tgstt build failed.' }
    $module = go list -C stt -mod=readonly -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-windows
    if ($LASTEXITCODE -ne 0) { throw 'Cannot locate STT libraries.' }
    foreach ($dll in @('sherpa-onnx-c-api.dll', 'sherpa-onnx-cxx-api.dll', 'onnxruntime.dll')) {
        Copy-Item -LiteralPath (Join-Path $module "lib/x86_64-pc-windows-gnu/$dll") -Destination $Artifacts
    }
    $dirty = @(git status --porcelain --untracked-files=normal)
    if ($LASTEXITCODE -ne 0) { throw 'Cannot check source state after build.' }
    if ($dirty.Count) { throw ("Build changed source; package is not releasable:`n" + ($dirty -join "`n")) }
    $files = @(Get-ChildItem -LiteralPath $Artifacts -File | ForEach-Object {
        @{ name = $_.Name; sha256 = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash }
    })
    @{ schema = 1; commit = $commit; build = $build; files = $files } |
        ConvertTo-Json -Depth 4 | Set-Content -Encoding UTF8 -LiteralPath (Join-Path $Artifacts 'manifest.json')
} finally {
    $env:CGO_ENABLED = $savedCGO
    Pop-Location
}

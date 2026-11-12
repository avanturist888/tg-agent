# Lightweight installer regression checks; no executable is launched or installed.
$ErrorActionPreference = 'Stop'
$fixture = Join-Path ([IO.Path]::GetTempPath()) ('tg-install-test-' + [guid]::NewGuid().ToString('N'))
[void][IO.Directory]::CreateDirectory($fixture)
$installer = Join-Path $PSScriptRoot 'install-build.ps1'
$commit = git -C (Split-Path -Parent $PSScriptRoot) rev-parse HEAD
$build = $commit.Substring(0, 12) + '-20261112.220000'
$global:tgInstallerFixtureBuild = $build
function go {
    $global:LASTEXITCODE = 0
    "build -ldflags=`"-X tgagent/internal/config.Build=$global:tgInstallerFixtureBuild`""
}
function Assert-Rejected([string]$reason) {
    try { & $installer -Artifacts $fixture -ValidateOnly } catch {
        if ($_.Exception.Message -notlike $reason) { throw }
        return
    }
    throw "Package accepted; expected $reason"
}
try {
    $names = @('tg.exe', 'tgw.exe', 'tgstt.exe', 'sherpa-onnx-c-api.dll', 'sherpa-onnx-cxx-api.dll', 'onnxruntime.dll')
    $files = @($names | ForEach-Object {
        $path = Join-Path $fixture $_
        [IO.File]::WriteAllText($path, 'fixture')
        @{ name = $_; sha256 = (Get-FileHash -LiteralPath $path).Hash }
    })
    $manifest = @{ schema = 1; commit = $commit; build = $build; files = $files }
    function Save-Manifest { $manifest | ConvertTo-Json -Depth 4 | Set-Content -Encoding UTF8 -LiteralPath (Join-Path $fixture 'manifest.json') }
    Save-Manifest
    & $installer -Artifacts $fixture -ValidateOnly
    $manifest.Remove('build')
    Save-Manifest
    Assert-Rejected 'Missing or invalid build ID.*'
    $manifest.build = 'arbitrary'
    Save-Manifest
    Assert-Rejected 'Missing or invalid build ID.*'
    $manifest.build = $build
    Save-Manifest
    $global:tgInstallerFixtureBuild = 'different'
    Assert-Rejected 'Embedded build ID does not match*'
    $global:tgInstallerFixtureBuild = $build
    [IO.File]::WriteAllText((Join-Path $fixture 'tg.exe'), 'corrupt')
    Assert-Rejected 'Hash mismatch*'

    # Execute the actual recovery catch with an injected blocked first file.
    $tokens = $null; $parseErrors = $null
    $ast = [Management.Automation.Language.Parser]::ParseFile($installer, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count) { throw $parseErrors[0] }
    $handler = $ast.FindAll({ param($node) $node -is [Management.Automation.Language.CatchClauseAst] -and $node.Body.Extent.Text.Contains('$restoreErrors = @()') }, $true)
    if ($handler.Count -ne 1) { throw 'Recovery handler not found.' }
    $body = $handler[0].Body.Extent.Text
    $recovery = [scriptblock]::Create($body.Substring(1, $body.Length - 2))
    $changed = @(@{current='good'; old='saved-good'; existed=$true}, @{current='blocked'; old='saved-blocked'; existed=$true})
    $script:moves = @()
    function Test-Path { param($LiteralPath) return $true }
    function Move-Item {
        param($LiteralPath, $Destination)
        $script:moves += $LiteralPath
        if ($LiteralPath -eq 'blocked') { throw 'Injected lock' }
    }
    try { & $recovery; throw 'Recovery did not report failure.' } catch {
        if ($_.Exception.Message -notlike '*saved-blocked*' -or $script:moves -notcontains 'saved-good') { throw }
    }
    Write-Output 'Installer checks passed: manifest, embedded build, hash, recovery continues after a locked file.'
} finally {
    Remove-Variable -Name tgInstallerFixtureBuild -Scope Global
    foreach ($path in [IO.Directory]::GetFiles($fixture)) { [IO.File]::Delete($path) }
    [IO.Directory]::Delete($fixture)
}

# Install a verified worker package; never compile on the laptop.
param([Parameter(Mandatory=$true)][string]$Artifacts, [switch]$ValidateOnly, [string]$BaselinePath)
$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $PSScriptRoot
$Artifacts = (Resolve-Path -LiteralPath $Artifacts).Path
$TaskName = 'tg-agent'
$savedTGHome, $savedTGProfile = $env:TG_AGENT_HOME, $env:TG_PROFILE
$installLock = $null
$stageDir = $null
function Assert-PlainPath([string]$Path) {
    $itemPath = [IO.Path]::GetFullPath($Path)
    while ($itemPath) {
        if (Test-Path -LiteralPath $itemPath) {
            if ((Get-Item -Force -LiteralPath $itemPath).Attributes -band [IO.FileAttributes]::ReparsePoint) {
                throw "Linked installation or artifact path: $itemPath"
            }
        }
        $itemPath = [IO.Path]::GetDirectoryName($itemPath)
    }
}
function Assert-InstallationBaseline([string]$Path) {
    $baseline = Get-Content -Raw -LiteralPath $Path | ConvertFrom-Json
    if ($baseline.root -ne $Root -or $baseline.commit -ne $commit) { throw 'Wrong installation baseline.' }
    $baselineFiles = @($baseline.files)
    if ($baselineFiles.Count -ne $expected.Count -or
        @(Compare-Object ($expected | Sort-Object) @($baselineFiles.name | Sort-Object)).Count) { throw 'Incomplete installation baseline.' }
    foreach ($f in $baselineFiles) {
        $currentPath = Join-Path $Root "bin\$($f.name)"
        Assert-PlainPath $currentPath
        $present = Test-Path -LiteralPath $currentPath -PathType Leaf
        if ($present -ne $f.present -or ($present -and (Get-FileHash -LiteralPath $currentPath -Algorithm SHA256).Hash -ne $f.sha256)) {
            throw "Local binary changed since baseline: $($f.name)"
        }
    }
}
Push-Location $Root
try {
    Assert-PlainPath $Artifacts
    Assert-PlainPath (Join-Path $Root 'bin')
    Assert-PlainPath (Join-Path $Artifacts 'manifest.json')
    $commit = git rev-parse HEAD
    if ($LASTEXITCODE -ne 0) { throw 'Cannot read local commit.' }
    $dirty = @(git status --porcelain --untracked-files=normal)
    if ($LASTEXITCODE -ne 0 -or $dirty.Count) { throw 'Local source changed; finish or preserve it before installation.' }
    $manifest = Get-Content -Raw -LiteralPath (Join-Path $Artifacts 'manifest.json') | ConvertFrom-Json
    if ($manifest.schema -ne 1 -or $manifest.commit -ne $commit) { throw 'Wrong revision or manifest schema.' }
    if ($manifest.build -cnotmatch ('^' + [regex]::Escape($manifest.commit.Substring(0, 12)) + '-[0-9]{8}\.[0-9]{6}$')) { throw 'Missing or invalid build ID.' }
    $files = @($manifest.files)
    $expected = @('tg.exe', 'tgw.exe', 'tgstt.exe', 'sherpa-onnx-c-api.dll', 'sherpa-onnx-cxx-api.dll', 'onnxruntime.dll')
    $actual = @($files | ForEach-Object { $_.name })
    if (@(Compare-Object ($expected | Sort-Object) ($actual | Sort-Object)).Count -ne 0 -or $actual.Count -ne $expected.Count) { throw 'Incomplete package.' }
    $disk = @(Get-ChildItem -Force -LiteralPath $Artifacts)
    if (@($disk | Where-Object { $_.PSIsContainer }).Count -or
        @(Compare-Object (@($expected) + 'manifest.json' | Sort-Object) @($disk.Name | Sort-Object)).Count) {
        throw 'Unexpected package contents.'
    }
    foreach ($f in $files) {
        $path = Join-Path $Artifacts $f.name
        if ((Get-Item -LiteralPath $path).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Artifact is a link.' }
        if ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ne $f.sha256) { throw "Hash mismatch: $($f.name)" }
    }
    foreach ($exe in @('tg.exe', 'tgw.exe', 'tgstt.exe')) {
        $metadata = & go version -m (Join-Path $Artifacts $exe) 2>&1
        if ($LASTEXITCODE -ne 0) { throw "Cannot read executable metadata: $exe" }
        if ($exe -ne 'tgstt.exe' -and (($metadata -join "`n") -notmatch ('tgagent/internal/config\.Build=' + [regex]::Escape($manifest.build) + '(?=\s|"|$)'))) {
            throw "Embedded build ID does not match manifest: $exe"
        }
        if (($metadata -join "`n") -notmatch ('vcs.revision=' + [regex]::Escape($commit) + '(?=\s|$)') -or
            ($metadata -join "`n") -notmatch 'vcs.modified=false(?=\s|$)') {
            throw "Embedded source revision does not match clean manifest: $exe"
        }
    }
    if ($ValidateOnly) {
        if ($BaselinePath) { Assert-InstallationBaseline $BaselinePath }
        return
    }
    if (-not $BaselinePath) { throw 'Capture the local bin baseline before the worker build and pass -BaselinePath.' }
    New-Item -ItemType Directory -Force (Join-Path $Root 'bin') | Out-Null
    $lockPath = Join-Path $Root 'bin/install-build.lock'
    Assert-PlainPath $lockPath
    $installLock = [IO.File]::Open($lockPath, [IO.FileMode]::OpenOrCreate, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
    Assert-InstallationBaseline $BaselinePath
    # Remember active owned services before any files or cutover markers change.
    $tasks = @(Get-ScheduledTask -ErrorAction Stop |
        Where-Object { $_.TaskName -eq $TaskName -or $_.TaskName -like "$TaskName-*" } |
        Where-Object { $_.TaskName -ne 'tg-agent-approvals' } |
        Where-Object { @($_.Actions | Where-Object { $_.Execute -eq (Join-Path $Root 'bin\tgw.exe') }).Count -gt 0 } |
        Where-Object { $_.State -eq 'Running' })
    $build = $manifest.build
    $names = @('tg', 'tgw', 'tgstt')
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss-ffff'
    $stageDir = Join-Path $Root ('bin/install-stage-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $stageDir | Out-Null
    foreach ($name in $names) {
        $staged = Join-Path $stageDir "$name.new.exe"
        Copy-Item -LiteralPath (Join-Path $Artifacts "$name.exe") -Destination $staged
        $wanted = @($files | Where-Object { $_.name -eq "$name.exe" })[0].sha256
        if ((Get-FileHash -LiteralPath $staged -Algorithm SHA256).Hash -ne $wanted) { throw "Staged hash mismatch: $name" }
    }
    $env:TG_AGENT_HOME = $Root
    $env:TG_PROFILE = ''
    & (Join-Path $stageDir 'tg.new.exe') cutover --revoke
    if ($LASTEXITCODE -ne 0) { throw 'Отметка обновления не снялась — сборку не ввожу.' }
    $changed = @()
    try {
        foreach ($f in $files) {
            $cur = Join-Path $Root "bin\$($f.name)"
            $old = Join-Path $Root ("bin\" + [IO.Path]::GetFileNameWithoutExtension($cur) + ".old-$stamp" + [IO.Path]::GetExtension($cur))
            $entry = @{ current = $cur; old = $old; existed = (Test-Path -LiteralPath $cur) }
            if ($entry.existed) { Move-Item -LiteralPath $cur -Destination $old }
            $changed += $entry
            Copy-Item -LiteralPath (Join-Path $Artifacts $f.name) -Destination $cur
            if ((Get-FileHash -LiteralPath $cur -Algorithm SHA256).Hash -ne $f.sha256) { throw "Installed hash mismatch: $($f.name)" }
        }
    } catch {
        # No migration has started. Restore the previous executable/library set.
        $failure = $_
        $restoreErrors = @()
        [array]::Reverse($changed)
        foreach ($entry in $changed) {
            try {
                if (Test-Path -LiteralPath $entry.current) { Move-Item -LiteralPath $entry.current -Destination "$($entry.current).failed-$stamp" }
                if ($entry.existed) { Move-Item -LiteralPath $entry.old -Destination $entry.current }
            } catch {
                $restoreErrors += "$($entry.current): $($_.Exception.Message); saved file: $($entry.old)"
            }
        }
        if ($restoreErrors.Count) { throw ("Installation failed: $failure`nRestore failures:`n" + ($restoreErrors -join "`n")) }
        throw $failure
    }
    # службы (основная и профилей, tg-agent-<имя>) — на новую сборку
    # (Stop-ScheduledTask сам процесс не убивает)
    if ($tasks.Count -gt 0) {
        foreach ($t in $tasks) { Stop-ScheduledTask -TaskName $t.TaskName -TaskPath $t.TaskPath -ErrorAction SilentlyContinue }
        Get-CimInstance Win32_Process |
            Where-Object { $_.ExecutablePath -and ([IO.Path]::GetDirectoryName($_.ExecutablePath) -eq (Join-Path $Root 'bin')) } |
            Where-Object { $_.CommandLine -match 'tgw?(\.old[-0-9]*)?\.exe"?\s+(serve|approvals)' } |
            ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    }
    # состояние сборки до 12.11.2026 (JSON-файлы в data/) переносится в
    # state.db, только когда её процессов не осталось: tg cutover дожидается
    # их (вызовы инструментов, CLI) или завершает (окно управления) и ставит
    # отметку. Состояние уже в базе — он сразу выходит.
    & bin\tg.exe cutover
    if ($LASTEXITCODE -ne 0) {
        throw "Cutover failed; services remain stopped. Previous binaries are bin\*.old-$stamp.*. Do not restore them after migration without tg rollback."
    }
    $startErrors = @()
    if ($tasks.Count -gt 0) {
        foreach ($t in $tasks) {
            try {
                Start-ScheduledTask -TaskName $t.TaskName -TaskPath $t.TaskPath
                Write-Host "Служба $($t.TaskName) перезапущена: $((Get-ScheduledTask -TaskName $t.TaskName -TaskPath $t.TaskPath).State)"
            } catch { $startErrors += "$($t.TaskName): $($_.Exception.Message)" }
        }
    } else {
        Write-Host 'Ранее работающих служб нет; остановленные службы не запускаются.'
    }
    if ($startErrors.Count) { throw ("Service restart failed:`n" + ($startErrors -join "`n")) }
    Write-Host "Файлы установлены: $(Join-Path $Root 'bin'), сборка $build; прежние файлы: *.old-$stamp.*"
} finally {
    $env:TG_AGENT_HOME = $savedTGHome
    $env:TG_PROFILE = $savedTGProfile
    if ($stageDir) {
        try {
            foreach ($name in @('tg', 'tgw', 'tgstt')) {
                $staged = Join-Path $stageDir "$name.new.exe"
                if ([IO.File]::Exists($staged)) { [IO.File]::Delete($staged) }
            }
            if ([IO.Directory]::Exists($stageDir)) { [IO.Directory]::Delete($stageDir) }
        } catch { Write-Warning "Staging cleanup failed; retained $stageDir : $($_.Exception.Message)" }
    }
    if ($installLock) { $installLock.Dispose() }
    Pop-Location
}

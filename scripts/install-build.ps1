# Install a verified worker package; never compile on the laptop.
param([Parameter(Mandatory=$true)][string]$Artifacts, [switch]$ValidateOnly)
$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $PSScriptRoot
$Artifacts = (Resolve-Path -LiteralPath $Artifacts).Path
$TaskName = 'tg-agent'
Push-Location $Root
try {
    $manifest = Get-Content -Raw -LiteralPath (Join-Path $Artifacts 'manifest.json') | ConvertFrom-Json
    if ($manifest.schema -ne 1 -or $manifest.commit -ne (git rev-parse HEAD)) { throw 'Wrong revision or manifest schema.' }
    if ($manifest.build -cnotmatch ('^' + [regex]::Escape($manifest.commit.Substring(0, 12)) + '-[0-9]{8}\.[0-9]{6}$')) { throw 'Missing or invalid build ID.' }
    $files = @($manifest.files)
    $expected = @('tg.exe', 'tgw.exe', 'tgstt.exe', 'sherpa-onnx-c-api.dll', 'sherpa-onnx-cxx-api.dll', 'onnxruntime.dll')
    $actual = @($files | ForEach-Object { $_.name })
    if (@(Compare-Object ($expected | Sort-Object) ($actual | Sort-Object)).Count -ne 0 -or $actual.Count -ne $expected.Count) { throw 'Incomplete package.' }
    foreach ($f in $files) {
        $path = Join-Path $Artifacts $f.name
        if ((Get-Item -LiteralPath $path).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Artifact is a link.' }
        if ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ne $f.sha256) { throw "Hash mismatch: $($f.name)" }
    }
    foreach ($exe in @('tg.exe', 'tgw.exe')) {
        $metadata = & go version -m (Join-Path $Artifacts $exe) 2>&1
        if ($LASTEXITCODE -ne 0 -or (($metadata -join "`n") -notmatch ('tgagent/internal/config\.Build=' + [regex]::Escape($manifest.build) + '(?=\s|"|$)'))) {
            throw "Embedded build ID does not match manifest: $exe"
        }
    }
    if ($ValidateOnly) { return }
    $build = $manifest.build
    $names = @('tg', 'tgw', 'tgstt')
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss-ffff'
    New-Item -ItemType Directory -Force bin | Out-Null
    foreach ($name in $names) { Copy-Item -LiteralPath (Join-Path $Artifacts "$name.exe") -Destination "bin\$name.new.exe" }
    & bin\tg.new.exe cutover --revoke
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
    Write-Host "Готово: $(Join-Path $Root 'bin'), сборка $build"

    # службы (основная и профилей, tg-agent-<имя>) — на новую сборку
    # (Stop-ScheduledTask сам процесс не убивает)
    $tasks = @(Get-ScheduledTask -TaskName "$TaskName*" -ErrorAction SilentlyContinue |
        Where-Object { $_.TaskName -eq $TaskName -or $_.TaskName -like "$TaskName-*" } |
        Where-Object { $_.TaskName -ne 'tg-agent-approvals' } |
        Where-Object { @($_.Actions | Where-Object { $_.Execute -eq (Join-Path $Root 'bin\tgw.exe') }).Count -gt 0 })
    if ($tasks.Count -gt 0) {
        foreach ($t in $tasks) { Stop-ScheduledTask -TaskName $t.TaskName -ErrorAction SilentlyContinue }
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
        Write-Warning 'tg cutover не закончил: старое состояние не перенесено, служба работает без него. Разберись и повтори: bin\tg.exe cutover'
    }
    if ($tasks.Count -gt 0) {
        foreach ($t in $tasks) {
            Start-ScheduledTask -TaskName $t.TaskName
            Write-Host "Служба $($t.TaskName) перезапущена: $((Get-ScheduledTask -TaskName $t.TaskName).State)"
        }
    } else {
        Write-Host 'Службы в Планировщике нет — поставить: scripts\install-task.ps1'
    }
} finally {
    Pop-Location
}

# Собирает tg-agent в bin\ и сразу вводит сборку в работу:
#   tg.exe  — консольная: CLI и MCP-прослойка агентов (stdio)
#   tgw.exe — без окна консоли: служба в Планировщике и окно управления
#
# Запуск:  powershell -ExecutionPolicy Bypass -File scripts\build.ps1
#
# После сборки:
#   - служба (задача Планировщика tg-agent) перезапускается сама;
#   - MCP-серверы агентов перезапускать не нужно: `tg mcp` — прослойка, вызовы
#     идут в службу, а новую сборку она замечает по метке и сама обновляет
#     список инструментов;
#   - открытое окно управления крутит старую сборку — открыть заново.

$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $PSScriptRoot
$TaskName = 'tg-agent'
Push-Location $Root
try {
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'Тесты не прошли — сборку не обновляю.' }
    New-Item -ItemType Directory -Force bin | Out-Null

    # метка сборки: по ней прослойки агентов замечают новую службу
    $commit = (git rev-parse --short HEAD 2>$null)
    if (-not $commit) { $commit = 'nogit' }
    if (git status --porcelain 2>$null) { $commit += '+' }
    $build = "$commit-$(Get-Date -Format 'MMdd.HHmm')"
    $ldBuild = "-X tgagent/internal/config.Build=$build"

    # собираем во временные имена: запущенный exe Windows перезаписать не даст
    go build -ldflags $ldBuild -o bin\tg.new.exe ./cmd/tg
    if ($LASTEXITCODE -ne 0) { throw 'Сборка tg.exe не удалась.' }
    go build -ldflags "-H=windowsgui $ldBuild" -o bin\tgw.new.exe ./cmd/tg
    if ($LASTEXITCODE -ne 0) { throw 'Сборка tgw.exe не удалась.' }

    # прежние сборки, которые уже никто не держит, — убрать
    Get-ChildItem bin -Filter '*.old*.exe' | ForEach-Object { Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue }
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    foreach ($name in 'tg', 'tgw') {
        $new = "bin\$name.new.exe"; $cur = "bin\$name.exe"
        # запущенный exe нельзя удалить, но можно переименовать; имя уникальное,
        # потому что прошлую «старую» сборку тоже может кто-то держать
        if (Test-Path $cur) { Move-Item $cur "bin\$name.old-$stamp.exe" -Force }
        Move-Item $new $cur -Force
    }
    Write-Host "Готово: $(Join-Path $Root 'bin'), сборка $build"

    # служба — на новую сборку (Stop-ScheduledTask сам процесс не убивает)
    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
        Get-CimInstance Win32_Process |
            Where-Object { $_.CommandLine -match 'tgw?(\.old[-0-9]*)?\.exe"?\s+(serve|approvals)' } |
            ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
        Start-ScheduledTask -TaskName $TaskName
        Write-Host "Служба перезапущена: $((Get-ScheduledTask -TaskName $TaskName).State)"
    } else {
        Write-Host 'Службы в Планировщике нет — поставить: scripts\install-task.ps1'
    }
} finally {
    Pop-Location
}

# Собирает Go-версию tg-agent в go\bin\:
#   tg.exe  — консольная: CLI и MCP-сервер (stdio)
#   tgw.exe — без окна консоли: слушатель в Планировщике и окно управления
#
# Запуск:  powershell -ExecutionPolicy Bypass -File scripts\build-go.ps1

$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $PSScriptRoot
Push-Location (Join-Path $Root 'go')
try {
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'Тесты не прошли — сборку не обновляю.' }
    # собираем во временные имена: работающий tg.exe (MCP, слушатель) Windows не даст перезаписать
    go build -o bin\tg.new.exe ./cmd/tg
    if ($LASTEXITCODE -ne 0) { throw 'Сборка tg.exe не удалась.' }
    go build -ldflags '-H=windowsgui' -o bin\tgw.new.exe ./cmd/tg
    if ($LASTEXITCODE -ne 0) { throw 'Сборка tgw.exe не удалась.' }
    foreach ($name in 'tg', 'tgw') {
        $new = "bin\$name.new.exe"; $cur = "bin\$name.exe"; $old = "bin\$name.old.exe"
        if (Test-Path $old) { Remove-Item $old -Force -ErrorAction SilentlyContinue }
        # запущенный exe нельзя удалить, но можно переименовать
        if (Test-Path $cur) { Move-Item $cur $old -Force }
        Move-Item $new $cur -Force
    }
    Write-Host "Готово: $(Join-Path $Root 'go\bin')"
    Write-Host 'Работающие процессы (MCP, слушатель) крутят старую сборку до перезапуска.'
} finally {
    Pop-Location
}

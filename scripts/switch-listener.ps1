# Переключает слушатель кнопок (задача Планировщика tg-agent-approvals)
# между Python- и Go-версией. Обе работают с общими data\outbox.json,
# data\bot-updates.lock и прочим, поэтому переключение не теряет черновики:
# новый слушатель при старте дошлёт то, что одобрили, но не успели отправить.
#
# Запуск:  powershell -ExecutionPolicy Bypass -File scripts\switch-listener.ps1 -To go
#          powershell -ExecutionPolicy Bypass -File scripts\switch-listener.ps1 -To python   # откат

param([Parameter(Mandatory)][ValidateSet('go', 'python')][string]$To)

$ErrorActionPreference = 'Stop'
$TaskName = 'tg-agent-approvals'
$Root = Split-Path -Parent $PSScriptRoot

if ($To -eq 'go') {
    $exe = Join-Path $Root 'go\bin\tgw.exe'
    if (-not (Test-Path $exe)) { throw "Нет $exe — сначала scripts\build-go.ps1" }
    $action = New-ScheduledTaskAction -Execute $exe -Argument 'approvals' -WorkingDirectory $Root
} else {
    $uvw = Join-Path $env:USERPROFILE '.local\bin\uvw.exe'   # uvw = uv без окна консоли
    if (-not (Test-Path $uvw)) { throw "Не найден $uvw" }
    $action = New-ScheduledTaskAction -Execute $uvw -Argument "run --directory `"$Root`" tg approvals" -WorkingDirectory $Root
}

# Stop-ScheduledTask не убивает дочерние процессы — добиваем всё дерево слушателя
Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
Get-CimInstance Win32_Process |
    Where-Object { $_.CommandLine -match 'approvals' -and $_.CommandLine -match 'tg(w)?\.exe|uvw?\.exe' } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }

$task = Get-ScheduledTask -TaskName $TaskName
$task.Actions = @($action)
$task.Settings.Priority = 5   # по умолчанию 7 (ниже обычного) — Windows морозила слушателя
Set-ScheduledTask -InputObject $task | Out-Null
Start-ScheduledTask -TaskName $TaskName
Start-Sleep -Seconds 4

$audit = Join-Path $Root 'data\audit.jsonl'
$last = Get-Content $audit -Tail 20 -Encoding UTF8 | Where-Object { $_ -match '"daemon_start"' } | Select-Object -Last 1
Write-Host "Слушатель переключён на: $To. Состояние задачи: $((Get-ScheduledTask -TaskName $TaskName).State)"
if ($last) { Write-Host "Последний старт: $last" } else { Write-Host 'daemon_start в журнале пока нет — проверь через пару секунд: tg doctor' }

# Ставит слушатель кнопок подтверждения в автозапуск (Планировщик задач).
#
# Без него нажатие в боте обрабатывается только пока какой-нибудь агент ждёт
# решения. С ним — в любой момент, в том числе через час после запроса.
#
# Запуск:   powershell -ExecutionPolicy Bypass -File scripts\install-approvals-task.ps1
# Удалить:  Unregister-ScheduledTask -TaskName 'tg-agent-approvals' -Confirm:$false

$ErrorActionPreference = 'Stop'

$TaskName = 'tg-agent-approvals'
$Root     = Split-Path -Parent $PSScriptRoot
$Uvw      = Join-Path $env:USERPROFILE '.local\bin\uvw.exe'   # uvw = uv без окна консоли

if (-not (Test-Path $Uvw)) { throw "Не найден $Uvw — поправь путь к uv в скрипте." }

$action = New-ScheduledTaskAction -Execute $Uvw `
    -Argument "run --directory `"$Root`" tg approvals" `
    -WorkingDirectory $Root

$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME

# Слушатель висит на long-polling часами: лимит времени снимаем, после падения
# (сеть отвалилась, бот перезапустился) перезапускаем сами.
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -MultipleInstances IgnoreNew `
    -Priority 5  # по умолчанию 7 (ниже обычного) — Windows морозила слушателя

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
    -Settings $settings -Description 'tg-agent: кнопки подтверждения отправки в Telegram' -Force | Out-Null

Start-ScheduledTask -TaskName $TaskName
Start-Sleep -Seconds 2
$state = (Get-ScheduledTask -TaskName $TaskName).State
Write-Host "Задача '$TaskName' установлена и запущена. Состояние: $state"
Write-Host "Проверить: uv run --directory $Root tg doctor"

# Ставит слушатель кнопок подтверждения в автозапуск (Планировщик задач).
#
# Слушатель разбирает нажатия кнопок в боте, досылает автоотправку и ведёт
# ленты новых сообщений (data\feeds). Без него нажатие обрабатывается, только
# пока какой-нибудь агент ждёт решения.
#
# Запуск:   powershell -ExecutionPolicy Bypass -File scripts\install-task.ps1
# Удалить:  Unregister-ScheduledTask -TaskName 'tg-agent-approvals' -Confirm:$false

$ErrorActionPreference = 'Stop'

$TaskName = 'tg-agent-approvals'
$Root     = Split-Path -Parent $PSScriptRoot
$Exe      = Join-Path $Root 'bin\tgw.exe'   # tgw — без окна консоли

if (-not (Test-Path $Exe)) { throw "Нет $Exe — сначала scripts\build.ps1" }

$action  = New-ScheduledTaskAction -Execute $Exe -Argument 'approvals' -WorkingDirectory $Root
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME

# Слушатель висит на long-polling часами: лимит времени снимаем, после падения
# перезапускаем сами. Приоритет обычный: с «ниже обычного» (по умолчанию 7)
# Windows морозила слушателя на десятки секунд.
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -MultipleInstances IgnoreNew `
    -Priority 5

# Прежний слушатель (если есть) останавливаем вместе с процессом:
# Stop-ScheduledTask сам процесс не убивает.
Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
Get-CimInstance Win32_Process |
    Where-Object { $_.CommandLine -match 'tgw?\.exe"?\s+approvals' } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
    -Settings $settings -Description 'tg-agent: кнопки подтверждения отправки в Telegram' -Force | Out-Null

Start-ScheduledTask -TaskName $TaskName
Start-Sleep -Seconds 2
$state = (Get-ScheduledTask -TaskName $TaskName).State
Write-Host "Задача '$TaskName' установлена и запущена. Состояние: $state"
Write-Host "Проверить: $(Join-Path $Root 'bin\tg.exe') doctor"

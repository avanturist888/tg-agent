# Ставит службу tg-agent в автозапуск (Планировщик задач).
#
# Служба держит постоянное соединение с Telegram, пишет ленты новых сообщений
# (data\feeds) в реальном времени, разбирает кнопки в боте и досылает
# автоотправку. Агенты, CLI и окно управления ходят в Telegram через неё.
# Без службы всё работает напрямую, но без лент в реальном времени, а нажатие
# кнопки обрабатывается, только пока какой-нибудь агент ждёт решения.
#
# Запуск:   powershell -ExecutionPolicy Bypass -File scripts\install-task.ps1
# Удалить:  Unregister-ScheduledTask -TaskName 'tg-agent' -Confirm:$false
#
# Профиль (второй аккаунт, tg --profile <имя>): -Profile <имя> ставит его
# службу отдельной задачей tg-agent-<имя>; службы других профилей не трогает.

# $PROFILE — встроенная переменная PowerShell, поэтому внутри имя другое
param([Alias('Profile')][string]$ProfileName = '')

$ErrorActionPreference = 'Stop'

$TaskName = 'tg-agent'
$Argument = 'serve'
if ($ProfileName -and $ProfileName -ne 'default') {
    if ($ProfileName -notmatch '^[A-Za-z0-9_-]+$') { throw "Имя профиля: только латиница, цифры, - и _" }
    $TaskName = "tg-agent-$ProfileName"
    $Argument = "serve --profile $ProfileName"
} else {
    $ProfileName = ''
}
$Root     = Split-Path -Parent $PSScriptRoot
$Exe      = Join-Path $Root 'bin\tgw.exe'   # tgw — без окна консоли

if (-not (Test-Path $Exe)) { throw "Нет $Exe — сначала scripts\build.ps1" }

$action  = New-ScheduledTaskAction -Execute $Exe -Argument $Argument -WorkingDirectory $Root
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME

# Служба работает всё время: лимит времени снимаем, после падения перезапускаем.
# Приоритет обычный: с «ниже обычного» (по умолчанию 7) Windows морозила
# процесс на десятки секунд.
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -MultipleInstances IgnoreNew `
    -Priority 5

# Прежнюю службу этого профиля (и задачу старого имени) останавливаем вместе
# с процессом: Stop-ScheduledTask сам процесс не убивает. Службы других
# профилей не трогаем: у них в командной строке другой --profile.
foreach ($old in $TaskName, 'tg-agent-approvals') {
    Stop-ScheduledTask -TaskName $old -ErrorAction SilentlyContinue
}
Get-CimInstance Win32_Process |
    Where-Object { $_.CommandLine -match 'tgw?(\.old[-0-9]*)?\.exe"?\s+(serve|approvals)' } |
    Where-Object {
        if ($ProfileName) { $_.CommandLine -match "--profile[ =]$ProfileName(\s|$)" }
        else { $_.CommandLine -notmatch '--profile' }
    } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
if (Get-ScheduledTask -TaskName 'tg-agent-approvals' -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName 'tg-agent-approvals' -Confirm:$false
}

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
    -Settings $settings -Description 'tg-agent: служба доступа агентов к Telegram' -Force | Out-Null

Start-ScheduledTask -TaskName $TaskName
Start-Sleep -Seconds 3
$state = (Get-ScheduledTask -TaskName $TaskName).State
Write-Host "Задача '$TaskName' установлена и запущена. Состояние: $state"
if ($ProfileName) {
    Write-Host "Проверить: $(Join-Path $Root 'bin\tg.exe') --profile $ProfileName doctor"
} else {
    Write-Host "Проверить: $(Join-Path $Root 'bin\tg.exe') doctor"
}

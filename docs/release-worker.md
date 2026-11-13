# Выпуск через worker

Изменения кода сначала закоммичены и прошли ревью. Рабочие профили, `.env`,
данные и службы не являются fixtures. Один ответственный выполняет run/get
в существующем namespace `tgagentcheck`; второй запуск не идёт параллельно.

## Изоляция и проверяемый состав

Используется установленный wk с `clean-v1` и проектным `tools/wk.d/before`.
Native helper задаёт private HOME/AppData/Go/temp до project Bash/deps;
before оставляет TG/T3 aliases в отдельном каталоге внутри WK_RUN_TMP.
Он же задаёт абсолютный `PSModuleAnalysisCachePath` внутри WK_RUN_TMP **до**
запуска PowerShell. Иначе Windows PowerShell может создать свой служебный
кэш `Microsoft/Windows/PowerShell/ModuleAnalysisCache` в рабочей папке.
Команда PowerShell вызывается с `-NoProfile -NonInteractive`.

`build-worker.ps1` проверяет чистоту исходников, выполняет `go test -count=1
./...` и `go vet ./...` без live-тега, затем собирает tg/tgw и отдельный
модуль STT с cgo. Все Go-команды используют `-mod=readonly`. Сборка STT
не запускает модель, ffmpeg или tgstt; три DLL копируются из Go-модуля.
Прежнее значение CGO_ENABLED восстанавливается в finally. Артефакты — новый
подкаталог игнорируемого bin, поэтому не меняют VCS metadata следующих exe.

Unit-данные находятся в t.TempDir/private TG_AGENT_HOME; HTTP/WebSocket —
подставные локальные серверы, IPC — каналы временных установок. Тесты procs
и lock запускают только собственные тестовые процессы и завершают их.
DPAPI обрабатывает синтетическую строку; wincred использует случайное имя
своей тестовой записи и удаляет только его: private HOME сам по себе не
изолирует Windows Credential Manager. TG_TEST_OGG и T3_TEST_PAIRING очищаются
before; живые тесты имеют build tag live и в этот выпуск не включаются.
Скачивание Go-модулей — обращения сборщика к зависимостям, не доступ к чатам.

Это статическая проверка scope. Выпуск требует фактических успешных тестов,
vet, сборки и проверки установщика на synthetic fixtures на worker.

## До сборки и возврат пакета

На ноутбуке из корня проекта создать новый каталог вне репозитория без junction
(например C:/Users/vasis/work/tgagent-builds/<уникальный-id>). Сохранить там
baseline.json **до** запуска сборки:

```powershell
$releaseRoot = (Get-Location).Path
$releaseNames = @('tg.exe','tgw.exe','tgstt.exe','sherpa-onnx-c-api.dll','sherpa-onnx-cxx-api.dll','onnxruntime.dll')
$releaseFiles = @($releaseNames | ForEach-Object {
    $releasePath = Join-Path $releaseRoot "bin/$_"
    $releasePresent = Test-Path -LiteralPath $releasePath -PathType Leaf
    @{ name = $_; present = $releasePresent; sha256 = $(if ($releasePresent) { (Get-FileHash -LiteralPath $releasePath -Algorithm SHA256).Hash } else { $null }) }
})
@{ root = $releaseRoot; commit = (git rev-parse HEAD); files = $releaseFiles } |
    ConvertTo-Json -Depth 4 | Set-Content -Encoding UTF8 -LiteralPath <новый-каталог>/baseline.json
```

На worker через текущую обёртку проекта, после штатного path-check:

```text
tools/wk run --no-pg --no-deps -- powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File scripts/test-install-build.ps1
tools/wk run --no-pg --no-deps -- powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File scripts/build-worker.ps1 -Artifacts bin/release-<уникальный-id>
```

Вторая команда выполняется после успешной проверки установщика. Её fixtures
создаются во временном каталоге; проверки baseline только читают bin зеркала,
не запускают exe и не вызывают Планировщик или cutover.

Сохранить source SHA, RID, лог и exit code; при обрыве читать тот же RID через
attach/status. При ошибке не устанавливать частичный пакет. После успеха
`tools/wk get bin/release-<id> <новый-каталог>/package` возвращает только новый
staging. До get не запускать новый run/clean этого зеркала. Исходники get
не перезаписывает; повторной сборки на ноутбуке нет.

## Локальная установка и отказ

Сначала `powershell -NoProfile -NonInteractive -File scripts/install-build.ps1
-Artifacts <staging>/package -ValidateOnly`. Проверяются точный набор шести
файлов и manifest, SHA256, clean VCS revision всех exe и Build tg/tgw, локальный
HEAD и отсутствие source edits, reparse в пути пакета/установки.
ValidateOnly не запускает exe, cutover и службы; Go используется только для
чтения metadata командой `go version -m`.

Затем `scripts/build.ps1 -Artifacts <staging>/package -BaselinePath
<staging>/baseline.json`. Несовпавший baseline останавливает ввод до cutover.
Сверяются также скопированные tg/tgw/tgstt.new.exe до первого запуска.
До замены каждого прежнего exe/DLL сохраняется его *.old-<stamp> файл.
Ошибка замены запускает восстановление всех уже изменённых файлов; ошибки
восстановления сообщаются вместе, а сохранившиеся backups не удаляются.

После замены останавливаются свои службы и выполняется штатный tg cutover.
При отказе cutover установка завершается ошибкой, службы не запускаются,
backups остаются. После успеха перезапускаются только ранее работавшие owned
задачи; disabled/остановленные задачи не включаются. Отказ одной не мешает
попытке запустить остальные, затем сообщаются все ошибки.

После начала cutover/миграции возврат одних бинарников не считается откатом
состояния: использовать штатный `tg rollback` при остановленных процессах
по README, и лишь после его успеха возвращать прежние exe/DLL. JSON-снимки
очереди нельзя просто копировать обратно. До запуска новых служб, если
cutover уже начался, также не предполагать отсутствие записей новой сборки.

После установки проверить фактическую сборку и состояние каждой своей службы,
затем собственную active-hook/idle-resume доставку. Тесты и файл манифеста
сами по себе не доказывают ввод SQLite и hook-доставки в работу.

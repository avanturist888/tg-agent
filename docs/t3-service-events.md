# Служебные события Codex: предлагаемый контракт T3

Статус: предложение интерфейса, **не подключено к рабочей доставке**.
В T3 0.1.9 подтверждённого публичного ingress для него нет. Типы и чистые
преобразования — `internal/t3/service_event.go`, mocked проверки —
`internal/t3/service_event_test.go`. Они не подключаются к shared app-server,
не меняют asar, глобальные hooks, почту Нестора или подписки владельца.

Неподходящий маршрут через обычный `nestor send` откачен в `8454e86`.
Текущая рабочая доставка остаётся прежней до появления проверенного T3 ingress.

## Результат для владельца

Событие Telegram поступает работающему агенту как результат инструмента на
следующем безопасном шаге. Текущие инструменты не прерываются, пользовательский
prompt не создаётся. Простаивающий агент начинает ход с тем же tool output.
В истории T3 видны источник, событие, статус и соответствующий native item.
Claude inbox и его запасное пробуждение остаются прежними.

`PostToolUse.additionalContext` пригоден для отдельного active-only варианта,
но сам не будит idle. `Stop decision:block` Codex создаёт continuation как
user prompt. Обычный `nestor send`, его idle watcher, `turn/steer` с текстом,
user bootstrap и прямой доступ к shared app-server не являются реализациями
этого контракта. Глобальные hooks для него не согласованы и не включаются.

## Минимальный запрос к владельцу адаптера T3

Добавить поддерживаемую операцию enqueue служебного события с capability
`serviceEvents.version=1` и собственным durable lifecycle/history.
Имя `orchestration.serviceEvent.enqueue` ниже **предложено**, а не найдено
в установленном T3; посылать такой RPC до согласования нельзя.

```json
{
  "version": 1,
  "source": "tg-agent",
  "sourceEventID": "notification-immutable-id",
  "threadID": "mcp:logical-t3-thread",
  "nativeThreadID": "expected-current-native-codex-thread",
  "text": "tg-agent: test-chat — ids=[11]; tg_read_chat(chat=\"test-chat\", ids=[11])"
}
```

- `sourceEventID` создаётся и сохраняется производителем **до первого вызова**,
  вместе с точным конвертом и назначением. Нельзя генерировать новый ID на retry.
- Ключ дедупликации: `(source, threadID, sourceEventID)`. `Fingerprint()` связывает
  version/source/id/logical-thread/text. Иное содержание под тем же ключом —
  `event_conflict`, без второй доставки. Ключ не зависит от native thread.
  Точный wire-алгоритм fingerprint: SHA256 от ASCII `tg-agent/service-event/v1`
  с нулевым байтом в конце, затем последовательно source, sourceEventID,
  threadID, text. Каждое поле — четырёхбайтовая unsigned big-endian длина
  UTF-8 в байтах и сами байты. Digest — lowercase hex. JSON escaping и порядок
  ключей не влияют на hash; нормализация Unicode/пробелов не выполняется.
- `nativeThreadID` — условие первой маршрутизации. T3 под своим lifecycle lock
  проверяет открытый logical thread, текущий provider/native mapping и отсутствие
  subagent-получателя. Устаревшее назначение отвергается как `stale_target`,
  не переносится в другой logical thread. Producer повторно разрешает mapping
  того же logical thread и повторяет **тот же ID и конверт**.
- Если событие уже сохранено, повтор возвращает прежнюю квитанцию независимо
  от смены native thread. Недоставленный pending event сопровождает logical thread;
  T3 перед отправкой вновь проверяет mapping. Уже принятое native-провайдером
  событие автоматически в новую native-сессию не копируется.
- Source задаётся авторизованным service token; producer не получает произвольный
  provider RPC и не выбирает role/model/runtime mode. Авторизация источника и
  право доступа к целевому треду проверяются T3 при первом enqueue.
- Все ID — непустой печатный ASCII без пробелов, до 128 байт. Текст — непустой
  корректный UTF-8, до 8192 байт; JSON tool payload после escaping — до 16384 байт.
  Неизвестные поля протокола на сервере отклоняются. Текст не цитируется в ошибках.
- Конверт содержит alias/id и служебный текст, не сообщения из чатов, токены,
  сессии или персональную переписку. Чужой текст остаётся untrusted data.

## Квитанция и сохранность

```json
{
  "source": "tg-agent",
  "sourceEventID": "notification-immutable-id",
  "threadID": "mcp:logical-t3-thread",
  "fingerprint": "sha256-canonical-envelope",
  "accepted": true,
  "state": "pending",
  "duplicate": false,
  "historyItemID": "t3-service-event-item"
}
```

Enqueue подтверждает только durable сохранение события **и** видимой записи
истории в одной транзакции. Он не подтверждает, что модель уже прочитала текст.
Историческая запись имеет тип `service_event`, а не `user_message`; хранит
source/sourceEventID/fingerprint/state и ссылки на logical run, актуальный native
thread и принятый native functionCallOutput item, когда они известны. Чтение
штатной истории T3 показывает эти сведения; без этого native-приём не считается
полностью отражённым в T3. На повторный enqueue возвращается текущая фаза той же
записи, а не новая история с тем же текстом.
`SubmitServiceEvent` проверяет соответствие квитанции исходному событию,
fingerprint и непустой history item; без реализованного ingress возвращает
`ErrServiceEventsUnsupported`. Автоматического fallback нет.

`accepted=true` подтверждает, что T3 принял обязательство хранения, независимо
от последующего исхода доставки. Состояния квитанции: `pending`,
`provider_uncertain`, `provider_accepted`, `not_delivered`. Последнее — сохранённый
терминальный исход с обязательным `reason`: `target_archived`, `target_deleted`
или `provider_unsupported`. Отказ до durable enqueue не выдаёт accepted-квитанцию.
`not_delivered` допускается только при доказанном отсутствии native-приёма;
закрытый тред с неопределённой попыткой остаётся `provider_uncertain`.
`duplicate=true` означает повтор уже сохранённого ключа, не новый provider send.
Producer после квитанции может отметить свой batch переданным T3: обязательство
хранить и доставлять его перешло T3. До квитанции batch и его ID остаются у
producer. Нельзя сдвигать только курсор, забыв ID неоднозначного вызова.
Потерянный enqueue ack и последующее закрытие треда не мешают подтвердить
сохранение: retry возвращает accepted=true/duplicate=true/not_delivered с причиной
и прежним historyItemID. Producer прекращает повтор enqueue и показывает итог
«не доставлено», не выдавая его за прочтение моделью.

T3 сохраняет переход в provider attempt до вызова native API: неизменяемые
attemptID, provider instance, nativeThreadID, logical run, fingerprint и границу
известной native history перед попыткой. Изменение текущего mapping не изменяет
назначение уже начатой попытки. Сбой до начала
вызова допускает повтор; заведомый отказ без приёма допускает отложенный retry.
Потерянный ответ после возможного приёма — `provider_uncertain`: повторный
enqueue от producer не вызывает ещё один provider send. T3 сверяет native
rollout именно сохранённого native target/попытки: functionCallOutput с ожидаемыми
tool name/namespace и полным конвертом source/sourceEventID/logical threadID/
fingerprint/text. Fingerprint пересчитывается по содержимому. Одного совпадения
sourceEventID недостаточно. Найденный item должен принадлежать записанной
попытке, а не оказаться унаследованным при fork/import; если происхождение нельзя
различить, исход остаётся uncertain. Доказанный item связывается с T3 history.
Отсутствие item в одном снимке не доказывает отказ.
Без доказательства приёма/неприёма автоматической повторной отправки нет.
Такое событие остаётся видимым с диагностикой, а не молча считается доставленным.

Обычный `turn/start.toolOutput` не документирует серверный idempotency key.
Поэтому ровно одну обработку моделью через произвольные обрывы обещать нельзя:
этот контракт предотвращает слепой дубль, но требует reconciliation и может
оставить uncertain до разбора. Если T3 обещает более сильную гарантию, ему нужна
дополнительная поддержка native-провайдера, а не новый sourceEventID на retry.

Архивирование/удаление/смена на неподдерживаемый provider до первой доставки
останавливает её с видимым итогом; не будит закрытый тред и не шлёт другому агенту.
Pending и uncertain нельзя очищать по обычному сроку хранения завершённых событий.
В версии 1 ключи дедупликации **не имеют автоматического срока истечения**.
После provider_accepted и not_delivered T3 также сохраняет tombstone:
source/threadID/sourceEventID/fingerprint, accepted, терминальный state/reason,
historyItemID и ссылки на доказательство native-приёма, если оно было.
Тело конверта можно убрать по политике хранения, но видимая history-заглушка
и tombstone остаются. Поздний retry получает прежний исход; изменённое содержимое
с тем же ключом — event_conflict. Очистка тела, закрытие/удаление треда,
перезапуск, backup/restore и смена native thread не превращают старый ID в новый.
Если T3 не может сохранить это правило после утраты ledger, ingress остаётся
недоступным до восстановления ledger; чистый ledger не выдаётся за продолжение
старого пространства ID. Иной TTL/epoch — отдельная версия протокола с явным
отказом на просроченные ID, а не молчаливый повтор отправки.

## Вызов внутри адаптера, не из tg-agent

После проверки mapping T3 строит `ServiceEvent.CodexInput()` и вызывает собственный
Codex adapter через поддерживаемую операцию `turn/start`:

```json
{
  "threadId": "verified-current-native-codex-thread",
  "input": [],
  "toolOutput": {
    "name": "notification",
    "namespace": "tg-agent",
    "output": "{\"source\":\"tg-agent\",\"sourceEventID\":\"notification-immutable-id\",\"threadID\":\"mcp:logical-t3-thread\",\"fingerprint\":\"sha256-canonical-envelope\",\"trust\":\"untrusted\",\"text\":\"tg-agent: test-chat — ids=[11]\"}"
  }
}
```

- Active regular turn: tool output присоединяется к нему. T3 не вызывает
  interrupt/restart/steer, не меняет run/модель/права и не создаёт user message.
- Idle: T3 создаёт свой run с причиной `service_event`, связывает историю события
  с ним и вызывает native turn/start с input=[]/toolOutput. Роль остаётся tool.
- Review/compact и другие non-steerable состояния: pending до безопасного
  продолжения; нельзя автоматически превращать в user input ради обхода отказа.
- Между определением active/idle и вызовом нет внешнего race: маршрутизацию
  и созданный native turn/result обрабатывает тот же T3 lifecycle, что обычные ходы.
- Успех native API означает `provider_accepted`, не «пользователь получил ответ».
  В T3 виден native functionCallOutput item со sourceEventID и фазой доставки.
  Raw payload не дублируется в диагностические логи и сообщения другим проектам.

## Проверка при реализации T3

Локальные Go tests здесь проверяют только сериализацию, ограничения, fingerprint,
квитанции и producer retry с mock ingress. Они **не доказывают поддержку T3**.
Перед вводом T3 необходимы его mocked lifecycle tests и собственный e2e-контекст:

| Сценарий | Проверяемый результат |
|---|---|
| Active + выполняющийся инструмент | Инструмент доходит до своего результата, тот же run, затем functionCallOutput; нет user/interrupt/steer |
| Idle | Один новый service_event run, input=[], tool role в native и T3 history |
| Active меняется на idle/обратно | Один native submit под lifecycle, корректная привязка run |
| Потеря enqueue receipt | Тот же ID даёт duplicate, один сохранённый event/history item |
| Потеря receipt, затем закрытие треда | Accepted terminal receipt с not_delivered/reason; producer прекращает retry |
| Поздний retry после очистки тела завершённого события | Tombstone возвращает прежний исход; второй native submit не возникает |
| Native ответ потерян | provider_uncertain, повтор producer не создаёт второй native submit |
| Одинаковый sourceEventID в перенесённой/разветвлённой истории | Проверяются полный fingerprint/конверт и сохранённая попытка, чужой item не засчитывается |
| Новый native thread того же T3 thread | Pending следует актуальному mapping; уже принятое не дублируется |
| Чужой thread/subagent/архив/нет capability | Явный отказ или сохранённый terminal outcome, никаких скрытых fallback |
| Подмена role/слишком большой payload/конфликт ID | Отказ до provider call, ошибка не выводит payload |

Собственные тестовые треды — только по разрешённому владельцем порядку создания;
чужие задачи для e2e не используются. Ни этот документ, ни mock не разрешают
отправлять новый RPC установленному T3 или подключаться к native app-server.

Основание: [Codex App Server: Start a turn](https://learn.chatgpt.com/docs/app-server#start-a-turn),
[Codex Hooks: background delivery](https://learn.chatgpt.com/docs/hooks#how-background-hooks-run).
Владелец общей локальной интеграции — Нестор; требуется его сверка и штатная
реализация T3 ingress. Публикация upstream issue/PR этим документом не производится.

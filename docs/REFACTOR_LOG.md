# Журнал рефакторинга качества кода

Задача: поднять качество кода без изменения поведения. Инварианты: судейство
бит-в-бит, wire-контракт v1 заморожен, БД не трогается, порядок локов
registry→room, перф не деградирует. Полный контекст — в постановке задачи;
здесь — решения, находки и отклонения.

Тестовая политика: на каждый рефактор-коммит — тесты затронутых пакетов; на
границе стейджа — полный `go test ./...` + `-race` + `go vet` + `golangci-lint`.
Флейк-протокол: упавший пакет повторяется изолированно; красный повторно =
регрессия (стоп и запись сюда), зелёный изолированно = флейк (строка сюда,
работа продолжается).

## Baseline (pre-refactor)

Состояние на коммите e5c6534 (после коммита quote-режима b81bae3 и разделённого
коммита countdown-lead e5c6534; рабочее дерево чистое):

- `go build ./...`, `go vet ./...`, `golangci-lint run` — 0 замечаний.
- `go test ./...` — зелёный (с Docker; интеграционные тесты падают, а не
  скипаются, без демона).
- Известный флейк: холодный полный прогон один раз уронил каскадом
  `internal/leaderboard` (первый тест 9.4 с, остальные 0.00 с — отказ старта
  testcontainers под параллельной нагрузкой); изолированный повтор — зелёный
  за 19.5 с.

Перф-базлайн (`go test -tags=load -run '^TestLoad' ./...`, машина разработчика,
Windows 10, Docker Desktop). Критерий перфа всей работы — построчный дифф этих
строк до/после, а НЕ зелёность сьюта: несколько бюджетов красные уже на
базлайне, и docs/PERFORMANCE.md фиксирует правило «missed budget is reported,
not widened».

Итог прогона: exit 1 (ожидаемо), 12 строк `BUDGET MISSED`. Пакеты:
auth ok, leaderboard FAIL (897с), replay FAIL (218с), runs FAIL (87с),
ws FAIL (316с) — FAIL здесь означает «есть missed-бюджеты», не «сломано».
Замеры зависят от машины и шумят от прогона к прогону (например, replay p99
здесь 62.55ms, в docs/PERFORMANCE.md — ~76ms): сравнивать нужно СОСТАВ
missed-строк и порядок кратностей, а не миллисекунды.

```
hashgate_load_test.go:512: BUDGET zone1-auth-hashing | gated(8) peak heap vs ungated, 200 concurrent | measured 463.5 MiB | limit 708.4 MiB (65%)
hashgate_load_test.go:552: BUDGET zone1-auth-hashing | saturated gate(4), 200 concurrent invalid logins: peak heap | measured 160.3 MiB | limit 292.0 MiB (55%)
projection_load_test.go:124: BUDGET zone 4 projection | ProjectRun for a typical player, production configuration | measured 25.1ms | limit 5ms (502%)
projection_load_test.go:124: BUDGET MISSED zone 4 projection | ProjectRun for a typical player, production configuration | measured 25.1ms exceeds 5ms by 5.0×
projection_load_test.go:153: BUDGET zone 4 projection | ProjectRun for a player with 100000 runs in one bucket | measured 967.49ms | limit 10ms (9675%)
projection_load_test.go:153: BUDGET MISSED zone 4 projection | ProjectRun for a player with 100000 runs in one bucket | measured 967.49ms exceeds 10ms by 96.7×
projection_load_test.go:532: BUDGET zone 4 projection | verified-email gate, one lookup (mean) | measured 2.32ms | limit 1ms (232%)
projection_load_test.go:532: BUDGET MISSED zone 4 projection | verified-email gate, one lookup (mean) | measured 2.32ms exceeds 1ms by 2.3×
projection_load_test.go:597: BUDGET zone 4 projection | rebuild of 427099 cells from 948600 runs, email gate OFF | measured 12m5.857s | limit 1m0s (1210%)
projection_load_test.go:597: BUDGET MISSED zone 4 projection | rebuild of 427099 cells from 948600 runs, email gate OFF | measured 12m5.857s exceeds 1m0s by 12.1×
projection_load_test.go:746: BUDGET zone 4 projection | projection overhead per judged run, production configuration | measured 7.11ms | limit 2ms (355%)
projection_load_test.go:746: BUDGET MISSED zone 4 projection | projection overhead per judged run, production configuration | measured 7.11ms exceeds 2ms by 3.6×
reads_load_test.go:55: BUDGET zone 3 reads | first page of a 99802-entry board, limit 50 | measured 31ms | limit 10ms (310%)
reads_load_test.go:55: BUDGET MISSED zone 3 reads | first page of a 99802-entry board, limit 50 | measured 31ms exceeds 10ms by 3.1×
reads_load_test.go:91: BUDGET zone 3 reads | keyset page 1001 of the same board | measured 7.6ms | limit 10ms (76%)
reads_load_test.go:154: BUDGET zone 3 reads | /me rank at the bottom of a 99786-entry board | measured 77.8ms | limit 30ms (259%)
reads_load_test.go:154: BUDGET MISSED zone 3 reads | /me rank at the bottom of a 99786-entry board | measured 77.8ms exceeds 30ms by 2.6×
reads_load_test.go:184: BUDGET zone 3 reads | bucket catalogue, 499 buckets over 427099 entries | measured 92.44ms | limit 200ms (46%)
reads_load_test.go:234: BUDGET zone 3 reads | first page with a banned player in the top ranks | measured 8.24ms | limit 10ms (82%)
replay_load_test.go:483: BUDGET replay-worker | max-submittable (120000 events, body ceiling) — worst sample vs the interrupt budget | measured 10.379s | limit 45s (23%)
replay_load_test.go:493: BUDGET replay-worker | max-submittable (120000 events, body ceiling) — median at 2x margin | measured 9.062s | limit 22.5s (40%)
replay_load_test.go:483: BUDGET replay-worker | max-events (120000, validator ceiling) — worst sample vs the interrupt budget | measured 8.666s | limit 45s (19%)
replay_load_test.go:493: BUDGET replay-worker | max-events (120000, validator ceiling) — median at 2x margin | measured 7.786s | limit 22.5s (35%)
replay_load_test.go:653: BUDGET replay-worker | max-words with a 200-event log | measured 97.13ms | limit 22.5s (0%)
replay_load_test.go:703: BUDGET replay-worker | realistic 60s run (p99) | measured 62.55ms | limit 50ms (125%)
replay_load_test.go:703: BUDGET MISSED replay-worker | realistic 60s run (p99) | measured 62.55ms exceeds 50ms by 1.3×
replay_load_test.go:803: BUDGET replay-worker | structural 50k-event log (rejected early by the reducer) | measured 975.61ms | limit 45s (2%)
ingest_load_test.go:104: BUDGET 5 ingest | POST /runs at the 2 MiB cap, p50 | measured 472.18ms | limit 150ms (315%)
ingest_load_test.go:104: BUDGET MISSED 5 ingest | POST /runs at the 2 MiB cap, p50 | measured 472.18ms exceeds 150ms by 3.1×
ingest_load_test.go:105: BUDGET 5 ingest | POST /runs at the 2 MiB cap, p99 | measured 745.14ms | limit 400ms (186%)
ingest_load_test.go:105: BUDGET MISSED 5 ingest | POST /runs at the 2 MiB cap, p99 | measured 745.14ms exceeds 400ms by 1.9×
ingest_load_test.go:196: BUDGET 5 ingest | peak heap, 20 concurrent capped POSTs | measured 335.4 MiB | limit 192.0 MiB (175%)
ingest_load_test.go:196: BUDGET MISSED 5 ingest | peak heap, 20 concurrent capped POSTs | measured 335.4 MiB exceeds 192.0 MiB
ingest_load_test.go:269: BUDGET 5 ingest | heap growth rejecting an 8 MiB body | measured 1.0 KiB | limit 13.0 MiB (0%)
ingest_load_test.go:278: BUDGET 5 ingest | bytes allocated rejecting an 8 MiB body | measured 16.1 MiB | limit 52.0 MiB (31%)
replay_endpoint_load_test.go:136: BUDGET 6 replay | GET /runs/{id}/replay/log, max run, p50 at 20 concurrent | measured 203.55ms | limit 250ms (81%)
replay_endpoint_load_test.go:137: BUDGET 6 replay | GET /runs/{id}/replay/log, max run, p99 at 20 concurrent | measured 312.49ms | limit 600ms (52%)
replay_endpoint_load_test.go:153: BUDGET 6 replay | peak heap, 20 concurrent max-run replay logs | measured 30.2 MiB | limit 192.0 MiB (16%)
replay_endpoint_load_test.go:284: BUDGET 6 replay | peak heap, one IP spending its full burst of 30 | measured 35.3 MiB | limit 128.0 MiB (28%)
matchend_load_test.go:444: BUDGET 8 | unrelated request p99 during a 20-match end burst | measured 36ms | limit 50ms (72%)
matchend_load_test.go:457: BUDGET 8 | 20 simultaneous match ends, capture persisted | measured 126.68ms | limit 5s (3%)
matchend_load_test.go:463: BUDGET 8 | peak live heap during the burst | measured 56.8 MiB | limit 256.0 MiB (22%)
matchend_load_test.go:496: BUDGET 8 | room round trip (chat) issued at the instant the burst starts | measured 39.55ms | limit 100ms (40%)
relay_load_test.go:669: BUDGET 7 | relay p99, 50 rooms x 5 clients | measured 17.67ms | limit 50ms (35%)
relay_load_test.go:671: BUDGET 7 | dropped peer_batches (responsive clients) | measured 0 | limit 0
relay_load_test.go:673: BUDGET 7 | duplicated peer_batches | measured 0 | limit 0
relay_load_test.go:1104: BUDGET 7 | slow consumer: healthy peers' p99 in the affected room | measured 1ms | limit 50ms (2%)
relay_load_test.go:1111: BUDGET 7 | slow consumer: frames lost between healthy peers | measured 0 | limit 0
relay_load_test.go:1272: BUDGET 7 | saturating burst: frames lost by the HEALTHY peers | measured 93 | limit 0
relay_load_test.go:1272: BUDGET MISSED 7 | saturating burst: frames lost by the HEALTHY peers | measured 93, expected 0
```

(Строки приведены без хвостовых пояснений-обоснований — они не меняются от
прогона к прогону и целиком лежат в исходниках тестов; полный сырой лог этого
прогона был одноразовым артефактом сессии.)

## Финальный перф-прогон (после всех стейджей)

Тот же `go test -tags=load -run '^TestLoad' ./...` на той же машине,
коммит f66c7f8: 44 строки BUDGET, 10 `BUDGET MISSED` — **строгое
подмножество** базлайновых 12. Построчный дифф:

- Ушли в зелёное (шум замеров в нашу пользу, не заслуга рефакторинга):
  `reads_load_test.go:55` first page (31→<10ms) и `relay_load_test.go:1272`
  saturating burst (93→0 потерянных кадров у здоровых пиров).
- Остальные 10 missed — те же строки, что на базлайне, с кратностями того же
  порядка (например, replay p99 1.3×→1.1×, projection rebuild 12.1×→8.7×).
- Отсутствуют 4 BUDGET-строки matchend: `TestLoadMatchEndBurst` упал за 2 с
  на старте testcontainer («rootless Docker is not supported on Windows,
  failed to create Docker provider») — флейк Docker-провайдера на длинном
  прогоне, на базлайне тест проходил (32.2 с). Не регрессия (ws-код менялся
  только перемещением + константой).
- `TestLoadIngestOversizedBodyRejectedEarly` красный в ОБОИХ прогонах
  одинаково (см. баг №7) — предсуществующий, вне диффа.

Вердикт: новых missed-бюджетов нет, перф-гейт пройден.

## Найденные баги

Правило задачи: баг, найденный при рефакторинге, записывается сюда и НЕ
чинится в рефактор-коммите. Поэтому список ниже — то, что было найдено, в
формулировках находки; он НЕ переписывается по мере починки. Актуальный статус
каждого пункта — в разделе «Триаж багов» ниже, и он ведётся там.

Заголовок раздела раньше гласил «(не исправлены)», и это разъехалось с
реальностью в тот же день, когда починили первый пункт: аудит (`docs/AUDIT_STATE.md`,
расхождение №4) поймал его на том, что №5 и №7 уже помечены исправленными
прямо в триаже ниже. Статус живёт в одном месте, а не в двух.

Сводка на сегодня: **из семи открыт один** — №6, и он средовой (флейк холодного
старта Docker), кода не касается.

1. **`cmd/server/main.go` — обещанный `defer pool.Close()` отсутствует.**
   Комментарии на строках ~74 («The pool is closed on the way out») и ~167
   (порядок deferred Wait против deferred pool.Close) описывают вызов,
   которого в файле нет: pgx-пул не закрывается штатно нигде в cmd/server
   (в остальных cmd/* — закрывается). Влияние: при graceful shutdown
   соединения обрываются на выходе процесса; комментарии вводят в
   заблуждение. Теста нет.

2. **`internal/ws/room.go:939,1062` — персист матча никем не отслеживается.**
   `go r.persist(snap)` — fire-and-forget горутина на
   `context.Background()`+15 с, не привязанная ни к WaitGroup, ни к
   shutdown-контексту. Влияние: при остановке сервера процесс может выйти
   посреди записи капчура матча в Postgres — запись теряется молча; число
   одновременных persist-горутин ничем не ограничено. Теста нет.

3. **`internal/ws/handler.go:210–214` + `room.go:295` — гонка регистрации
   grace-токена.** `disconnect` взводит grace-таймер до того, как handler
   вызовет `reg.addGrace(token)`. Если таймер сработает раньше (окно 15 с
   против двух соседних операторов — практически невероятно),
   `onGraceExpire → removeGrace` не найдёт запись, а `addGrace` добавит
   «сироту», которую никто никогда не удалит — медленная утечка слайса
   `reg.graces`. Именно эту гонку компенсирует retry-цикл в
   `relay_test.go:633` (документировано там же). Теста на саму гонку нет.

4. **`internal/ws/lobby.go:184–191` — `lobbyEntryOf` не знает `ModeQuote`.**
   switch по `Mode` покрывает только `ModeTime`/`ModeWords`: открытая
   quote-комната в публичном списке лобби не покажет ни `durationMs`, ни
   `wordCount`, хотя `protocol.IsCounted` во всех остальных местах трактует
   quote как words. Дыра свежей quote-фичи (коммит b81bae3). Теста нет.

5. **`internal/platform/mail/mail.go:98–99` — `LogSender` пишет секреты в
   логи.** Дефолтный sender (когда SMTP не сконфигурирован) логирует на
   info-уровне адрес получателя, тему и всё тело письма — а тело содержит
   живую ссылку верификации/сброса пароля с токеном (`?token=...`).
   Интеграционные тесты прямо полагаются на это (выгребают токен регэкспом
   из лога — `auth/integration_test.go:212`). Влияние: в любом окружении
   без SMTP живые токены и PII оседают в stdout-логах; тип документирован
   как dev-only, но ничем не enforced. Теста нет (тесты закрепляют
   противоположное).

6. **Флейк холодного старта testcontainers.** Полный `go test ./...` при
   непрогретом Docker уронил каскадом весь пакет `internal/leaderboard`
   (см. Baseline). Влияние: ложные красные на CI/локально при параллельном
   старте контейнеров. Изолированный повтор пакета — рабочий обход.
   Повторился на прогоне Стейджа 1: `internal/runs` упал каскадом (первый
   тест 22.9 с — таймаут старта контейнера, остальные 0.00 с) при
   одновременном старте пяти testcontainers-пакетов; изолированно — зелёный
   за 151 с. Флейк, не регрессия.
   Финальный полный прогон (после всех стейджей): та же природа, два
   перф-чувствительных теста под параллельной нагрузкой пакетов —
   `perf.TestSeedProducesAnEligiblePopulation` (0.00 с, отказ харнесса) и
   `replay.TestSeedingFitsTheStartupBudget` (бюджет времени сидинга на
   загруженной машине). Оба зелёные изолированно (perf 8.4 с,
   replay 333.9 с). Флейки, не регрессии.

7. **`internal/runs/ingest_load_test.go:243` — устаревшая фикстура
   «oversized»-теста.** `TestLoadIngestOversizedBodyRejectedEarly` шлёт тело
   6.1 MiB и ждёт 413, но после подъёма капа `maxBodyBytes` с 2 до 6.5 MiB
   (задокументированного в handler.go и docs/RUNS.md) такое тело легально —
   сервер честно отвечает 202. Красный на базлайне и на финальном прогоне
   одинаково; фикстуру нужно поднять выше капа (напр., 7 MiB) — но это
   правка теста, вне рефактор-коммитов. Влияние: ложный красный в load-сьюте.

### Триаж багов (post-review, после завершения рефакторинга)

Приоритеты не совпадают с нумерацией; решения владельца:

- **№5 — ИСПРАВЛЕН** (коммит 97b52ab, отдельная задача с мандатом на правку
  тестов): `LogSender` логирует только адресата, тему и длину тела. Правка
  тестов не понадобилась — харнесы уже перехватывали письма in-process
  (`recorderMailer` читает токен из тела перехваченного сообщения, не из
  stdout); формулировка аудита о «грепе из лога» была неточной.
- **№7 — ИСПРАВЛЕН** (тот же коммит, в рамках работ по log v2): это была не
  «ложная краснота», а дыра в покрытии — путь раннего 413 не проверялся
  ничем с момента подъёма капа. Ступени фикстуры теперь кап-ОТНОСИТЕЛЬНЫЕ
  (1.25×/2.5×/5× капа) и не сгниют при следующем пересчёте.
- **№1 + №2 — в бэклог одной задачей** «lifecycle горутин на shutdown»:
  persist-горутина без владельца (реальная потеря капчура матча — данных,
  из которых живут борды) + обещанный комментариями `defer pool.Close()`.
  Чинятся одним заходом и одним контекстом.
- **№4 — приклеить к ближайшей продуктовой задаче по бэку**: единственный
  пользовательски заметный (quote-комната в лобби без размеров), пять строк.
- **№3, №6 — как записаны, действий не требуют.**

Наблюдение владельца, важное для деплоя: пять из семи багов — не в логике,
а на границах жизненного цикла (закрытие пула, ожидание горутин, регистрация
токена, устаревшая фикстура, поведение дефолтного режима). Перед деплоем
нужен отдельный проход именно по shutdown-пути, а не по фичам.

### Триаж, обновление (фаза пост-аудита)

Тот самый «отдельный проход по shutdown-пути» и состоялся — вместе с задачей по
AFK-окну, потому что №3 оказался тем же узлом.

- **№1 + №2 — ИСПРАВЛЕНЫ** одной задачей, как и планировалось.
  `defer pool.Close()` регистрируется сразу после создания пула и потому
  разматывается последним — ровно то, что уже утверждал комментарий про порядок
  против `workers.Wait()`. Персист матча получил владельца (`Registry.goPersist`)
  и ограниченное ожидание на shutdown (`Handler.WaitForPersists`, `defer` в main).
  Контекст записи НЕ переехал на серверный: матч, закончившийся во время
  остановки, — это ровно тот капчур, который стоит сохранить, так что отменять
  его нельзя, не хватало именно ожидания. Не уложившееся в окно пишется в WARN,
  а не проглатывается. Тесты: `TestShutdownWaitsForTheMatchCapture` (без
  `Eventually` — ожидание и есть синхронизация), `TestShutdownReportsCapturesStillInFlight`.
- **№3 — ИСПРАВЛЕН**, вместе с разведением grace-окна и AFK-свипа: это один узел.
  `disconnect` теперь сам владеет всей последовательностью — регистрация токена,
  затем взвод таймера, — вместо неписаного контракта между `handler.go` и
  `room.go`. Два лока берутся последовательно и никогда вложенно (Registry.mu
  выше Room.mu). Retry-цикл в `relay_test.go`, компенсировавший гонку, больше
  ничего не компенсирует.
- **№4 — ИСПРАВЛЕН**: ветка выбора размерности спрашивает `protocol.IsCounted`,
  а не перечисляет режимы, — тот же вопрос, что задают finish-окно, AFK-share и
  потолок на слово. Следующий счётный режим попадёт в список правильно сам.
  Тесты: `TestLobbyEntryCarriesTheModesDimension` (обе размерности выставлены в
  разные ненулевые значения, проверяется ЗАКОДИРОВАННЫЙ набор ключей),
  `TestEveryCountedModeIsListedWithAWordCount`.
- **№6 — открыт, средовой.** Единственный оставшийся.

Наблюдение подтвердилось: все четыре пункта, дожившие до этой фазы, были на
границах жизненного цикла, и ни один — в логике.

## Отложено

- **Заморожено до формата лога v2** (следующая задача: keydown/keyup, бамп
  `EVENT_LOG_VERSION` до 2, пересчёт капов). Не рефакторится ничего из:
  - `internal/runs/validate.go` — `validateIngest`, капы `maxEvents=120_000`,
    `maxDurationMs`, `maxWordCount`, `seedMax`, `supportedLogVersion`;
  - `internal/runs/handler.go` — `maxBodyBytes = 13<<19` (6.5 МиБ) и
    ingest-путь (`handleIngest`), зависящий от капов;
  - `internal/runs/log.go` — gzip хранимого EventLog (формат-зависимый);
  - `internal/runs/ingest_load_test.go`, `internal/runs/contract_test.go` —
    тесты, закрепляющие капы и форму payload;
  - `internal/replay/worker.go:385–400` (`gunzip`, `maxLogBytes`) — читает
    тот же формат (и без того в замороженной зоне судейства).
  Примечание: из-за этого gzip-хелперы runs/replay исключены из
  дедупликации Стейджа 1 — двое из четырёх «дублей» заморожены.
- `internal/ws` — фреймы как `any` (`seat.backlog []any`, `send(msg any)`):
  маркер-интерфейс `protocol.Frame` типизировал бы буфер и send-путь, но
  трогает каждый тип фрейма — вне рамок задачи.
- `internal/protocol` — строковые wire-словари (Mode/Status/Reason/Code):
  типизация ломает много кода ради косметики при замороженном контракте.
- `internal/auth/register.go:32,177` — тайминг-защита (анти-enumeration,
  decoy-hash) в хендлерах `handleRegister`/`handleLogin` могла бы жить в
  service-слое, но перенос кода константного времени — высокий риск при
  нулевом поведенческом выигрыше. Не трогаем в этой задаче.
- `internal/replay/core.go:268` — `DictVersion` без ctx-параметра
  (жёсткий `context.Background()`): правка сигнатуры в замороженной зоне
  судейства.
- `internal/ws/matchend_test.go:195,230,355` — wall-clock `time.Sleep` до
  3.6 с против окон 200–300 мс: флейк-риск на нагруженном CI, но правка
  тестов = сигнал изменения поведения; отложено.
- `t.Parallel()` отсутствует во всех 340 тестах; чистые пакеты (protocol,
  replay/policy, leaderboard/bucket, quote/corpus, turnstile) могли бы
  параллелиться — правка тестов, отложено.

## Осознанно не трогаем

- `platform.Config` (40 полей, знает словарь всех доменов) — 12-factor
  трейдофф: весь env-парсинг в одном месте. Противоречит букве
  `platform/doc.go` («platform knows nothing about any domain») — противоречие
  зафиксировано здесь текстом, код не меняется: разбиение потянуло бы всю
  проводку main.go ради косметики.
- `panic` при сбое CSPRNG (`ws/id.go:18,30,40`, `ws/room.go:1332`,
  `ws/registry.go:115`) — идиоматичное «не может случиться»: отказавший
  crypto/rand невосстановим. См. «Расхождения со скиллом».
- Инлайн-сообщения валидации `protocol/protocol.go:586–640` — сверяются
  точной строкой в `protocol_test.go` намеренно: тексты ошибок — часть
  замороженного wire-контракта. Это фича, а не долг.
- Guard декомпрессионной бомбы `replay/worker.go:397` — выделенный sentinel
  дал бы метрикам отличать его от прочих ошибок воркера, но тянет за собой
  формат метрик; не в этой задаче.
- `internal/ws/wspg` — единственный сырой pgx-store: выбор документирован
  в doc.go пакета («no query reuse to justify sqlc»), параметризован, не
  трогаем.
- Комментарий-дифф countdown: `docs/PROTOCOL.md` в строках ~335 и ~399
  по-прежнему говорит «local 3-2-1», тогда как lead теперь 5 с
  (коммит e5c6534, там же это отмечено). Правка прозы PROTOCOL.md — решение
  владельца контракта, не рефакторинг.

## Ограничения среды

- `go test -race ./...` на этой машине невыполним: race-детектор на Windows
  требует CGO и C-компилятор (gcc/mingw), которого нет (Makefile сам
  оговаривает это у `test-race`). Race-гейт следует прогнать в CI/на машине
  с компилятором. Косвенное покрытие: ws-сьют содержит конкурентные
  churn/relay-тесты, а рефакторинг не менял ни одного примитива
  синхронизации (S3-a — чистое перемещение).

## Расхождения со скиллом (golang-pro)

- Скилл: «никогда panic для обычных ошибок» → оставляем `panic` на сбое
  CSPRNG в `ws` (см. выше): это не обычная ошибка, а невосстановимый отказ
  среды; конвертация в error расползлась бы по сигнатурам генераторов ID.
- Скилл: «context.Context на всех блокирующих операциях» → `DictVersion`
  (replay/core.go:268) и персист матча (room.go:1062) остаются как есть:
  первое — замороженная зона, второе — записанный баг №2, а баги в
  рефактор-коммитах не чинятся.
- Скилл: «горутины только с явным lifecycle» → janitor и persist-горутина
  не ожидаются на shutdown — записано багами №1–2, не чинится здесь.
- Скилл: «table-driven + t.Parallel + -race» → тесты в этой задаче не
  меняются (железный инвариант №1); отсутствие `t.Parallel()` — в
  «Отложено».
- Скилл предпочитает каналы/CSP для акторов → модель «Room = mutex-актор»
  заморожена инвариантом №6 и аудитом подтверждена как корректная (ни
  одного сетевого/БД-вызова под локом, порядок локов выдержан везде).

## Стейджи

### S1-a — internal/platform/httpx: дедупликация HTTP-хелперов
Что: `parseLimit` (×3, байт-в-байт: runs/leaderboard/quote), обвязка
base64url-курсора encode/decode (×3, различались только полями),
`clientIP` (×2: runs/auth), `etagMatches` (×2: runs/replay) → один пакет
`internal/platform/httpx` (ParseLimit, EncodeCursor/DecodeCursor, ClientIP,
ETagMatches). Доменные `encodeCursor`/`decodeCursor` остались как тонкие
типизированные обёртки — состав и парсинг полей курсора у каждого домена свой.
Почему: четыре копипаст-кластера между доменами; комментарий у runs-копии
`etagMatches` прямо называл причину дублирования — «делиться не откуда, кроме
как runs импортирует replay» — httpx (platform-уровень, ниже доменов) решает
ровно её. Токены курсоров, коды ошибок и заголовки на проводе не изменились.
Альтернатива, которую отверг: generic-кодек курсора целиком (поля и валидация
у доменов различаются — обобщение дало бы интерфейс шире проблемы);
`acceptsGzip` в httpx не переехал — вторая «копия» у runs не существует
(у runs осознанно нет Vary, задокументировано в handler.go).
Риск: low, чем прикрыт: хендлер-тесты runs/leaderboard/quote (пагинация,
битые курсоры), auth-тесты (rate-limit по IP), dictionaries_test (ETag/304/
Vary), contract-тесты.
Примечание: два удалённых комментария у `clientIP` противоречили друг другу
(auth: «single-binary assumption», runs: «expected to sit behind a proxy»);
httpx унаследовал формулировку runs — семантика (RemoteAddr, форвард-заголовкам
не верить) у обеих копий была идентична. Внутренние тексты ошибок «malformed
cursor» потеряли доменный префикс («runs:», «leaderboard:», «quote:» →
«httpx:») — до провода они не доходят (маппятся в фиксированный
apiErrBadCursor), тестами не сверяются.

### S2-a — internal/runstatus: один источник словаря статусов
Что: дубль констант `"pending"/"accepted"/"flagged"/"rejected"`
(runs/store.go и replay/queue.go, обязанных зеркалить один DB CHECK) →
пакет `internal/runstatus` (`type Status = string` + четыре константы);
оба домена ре-экспортируют их под историческими именами
(`StatusPending = runstatus.Pending`, …), поля `Run.Status`,
`Summary.Status`, `Decision.Status`, `CalibrationRun.Status` аннотированы
алиасом. Ни одного call-site не изменилось; строки в БД и на проводе — те же.
Почему: два списка, обязанных совпадать молча, — ровно тот дрейф, который
ловится только продом; ре-экспорт делает рассинхронизацию невозможной
(константа определена один раз). Отдельный микропакет, а не runs→replay
импорт: домены не импортируют друг друга (ARCHITECTURE.md), platform доменной
лексики не знает — соседство с internal/protocol по смыслу.
Альтернатива, которую отверг: **полноценный defined type** (`type Status
string`) — задание Стейджа 2 просило «именованный тип», но тесты закрепляют
plain-string семантику: `worker_test.go:33` сравнивает `string == StatusFlagged`
(не скомпилируется с defined type), `policy_test.go:255` присваивает константу
string-полю, `queue_pg_test.go:176` сверяет через `assert.Equal` со
string-полем (testify различает типы — тест упал бы в рантайме). Правка
тестов запрещена инвариантом №1, поэтому alias: единственность источника —
есть, номинальной типобезопасности — нет. Полная типизация возможна только
вместе с осознанной ревизией этих тестов (отдельной задачей).
Риск: low, чем прикрыт: worker_test/policy_test/queue_pg_test (replay),
runs-интеграционные и contract-тесты; алиас по определению не меняет типов.

### S3-a — ws/room.go: разрез god-файла (только перемещение)
Что: room.go (1338 строк, ~50 методов, шесть ответственностей) разрезан на
room.go (актор: seats, лобби-действия, grace, locked-хелперы, ~620 строк),
room_match.go (матч: тайминги, matchState, startMatch/relay/finish,
дедлайн/finish-window, endMatchLocked, newSeed), room_afk.go (AFK-правила:
afkAtLocked, runAfkSweep, sweepAfk), room_persist.go (снапшот, persist,
gzipBatches), room_chat.go (чат: rate-limit, chat, systemChatLocked).
Ни одна строка внутри перемещённых блоков не изменена; дифф читается как
«вырезал/вставил».
Почему: файл смешивал исполнение матча, AFK-судейство, персист с gzip, чат и
ник-генерацию — навигация и ревью страдали; разрез по ответственностям был
главной структурной целью Фазы 0.
Альтернатива, которую отверг: выделение под-типов (matchRunner, persister) —
это уже изменение структуры кода, а не перемещение; запрещено смешивать с
перемещением в одном коммите, и сам мьютекс-актор заморожен инвариантом №6.
Единственное отступление от «байт-в-байт»: общий const-блок
chatBurst/chatRefill/guestNickLow/guestNickCount разделён на два (chat-консты
уехали с чатом, guestNick-консты остались с ник-генерацией) — значения и
комментарий сохранены.
Риск: low (перемещение внутри пакета ничего не меняет по построению), чем
прикрыт: полный ws-сьют (relay/matchend/room/lobby/handler) + wspg.

### S3-b — ws: содержательные правки после разреза
Что: инлайновый `15*time.Second` в `persist` → именованная константа
`persistTimeout` с комментарием (единственный неименованный load-bearing
литерал ws-файлов по инвентарю Фазы 0). Значение не менялось.
Почему: все остальные тайминги ws — именованные константы или Option;
этот был единственным исключением, и matchend_load_test прямо ссылается на
«15 s context» в обосновании бюджета.
Альтернатива, которую отверг: делать таймаут Option/конфигом — ни один тест
его не варьирует, знob без потребителя запрещён постановкой («абстракции на
будущее»). Более крупные содержательные правки в ws (владелец
persist-горутины, unexport Registry/Room) не делались: первое — записанный
баг №2 (не чинится в рефактор-коммитах), второе не входит в согласованные
стейджи.
Риск: low, чем прикрыт: ws-сьют; константа подставляется компилятором.

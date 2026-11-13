# Приёмка

Это журнал доказательств, а не объявление готовности. `partial` означает,
что часть критерия проверена, но весь критерий ещё не принят. `pending`
означает отсутствие полной проверки. Учебный пример не заменяет второй
реальный проект. Cross-build не заменяет исполнение на macOS.

| PRD | Проверка или сценарий | Платформа / ревизия | Результат и ограничение |
| --- | --- | --- | --- |
| 1 | Одновременный запуск MillDrum и `/home/alex/dev/twikko` одним бинарником | Не запускалось | pending, этапы 7–8 |
| 2 | `examples/mini/scenario.py`, shared catalog между worktree | Linux / `12a2457` | passed на Linux, публичный сценарий завершился PASS и подтвердил cleanup |
| 3 | Тот же сценарий, независимый catalog и остановка выбранного worktree | Linux / `12a2457` | passed на Linux |
| 4 | `bind`, restart consumer, endpoint identity; dependency startup gate | Linux / `12a2457` | passed на Linux, explicit IDs сохраняются, unhealthy dependency блокирует consumer |
| 5 | Graph tests: cycle before execution, repeat и concurrent up | Linux / `12a2457` | passed на Linux, core graph и config tests прошли с race |
| 6 | Lifecycle failures, фактические state и JSON exit code; hook failures | Linux / `301afb9` частично | partial, single-service build/readiness/port проверены; hooks ещё отсутствуют |
| 7 | Реальный binary, SIGKILL frontend во время build, bootstrap EOF, foreign PID identity | Linux / `301afb9` | partial, Linux-проверки прошли; общий crash audit A1 впереди |
| 8 | Ошибка подготовки всего switch и отказ после stop без отката данных | Не запускалось | pending, этап 4 |
| 9 | Live/snapshot password gate, API/WebSocket, cookies, unshare | Не запускалось | pending, этап 6 и A2 |
| 10 | Snapshot immutable files и failure новой сборки | Не запускалось | pending, этап 6 |
| 11 | MillDrum backup/seed/sign-in/agent shutdown/watcher, SQLite isolation | Не запускалось | pending, этап 7 |
| 12 | Lifecycle на Linux и macOS, инструкция twikko | Linux / `301afb9` частично | partial, Darwin arm64/amd64 cross-build прошёл, runtime не проверен |
| 13 | Два hostname, сохранение URL при restart, alias conflict | Не запускалось | pending, этап 5 |
| 14 | Реальный wildcard, DNS и отрицательные cert/key/date проверки | [LOCAL_HTTPS.md](LOCAL_HTTPS.md) | partial, внешний TLS handshake проверен; runner TLS ещё отсутствует |
| 15 | TLS runner, Secure cookies, WebSocket/HMR, SSE и bindings | Не запускалось | pending, этап 5 |
| 16 | 404/503, чужой процесс на старом порту, reload/restart proxy, диагностика | Не запускалось | pending, этап 5 |
| 17 | Build/start получают одинаковые заранее назначенные dependency addresses | Linux / `12a2457` | passed на Linux, real graph tests проверяют build/start addresses и отсутствие переназначения после build |
| 18 | Discovery, IDs, dry-run, bind и graph lifecycle через публичный JSON без интерактива | Linux / `12a2457` | passed для этапов 1–3; switch/share войдут в итоговый сценарий этапа 8 |
| 19 | Switch single/list/file/all-services, независимая ветвь и partial JSON | Не запускалось | pending, этап 4 |
| 20 | TCP/UDP wildcard ownership, proxy loopback, share-only exposure, отсутствие DNS API | Linux / `301afb9` частично | partial, Linux listener checks прошли; Darwin runtime, proxy/share впереди |
| 21 | Все ежедневные сценарии MillDrum через новую обёртку | Не запускалось | pending, этап 7; пользователь разрешил отключить OAuth только в защищённом preview, см. MIGRATION_MAP |

Для окончательной приёмки каждая строка получает точный commit, выполненную
команду или путь к воспроизводимому сценарию и фактический результат.
Неустранённые ограничения остаются в строке даже при успешных других проверках.
Публикация релиза, push и merge этим документом не разрешаются.

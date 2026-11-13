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
| 6 | Lifecycle failures, фактические state и JSON exit code; hook failures | Linux / `a825cb3` | passed на Linux, hook retry и startup failures проверены; macOS отдельно |
| 7 | Реальный SIGKILL frontend, bootstrap EOF, foreign identity, Child=ZERO recovery | Linux / `a825cb3`, A1 re-check | passed на Linux в проверенном scope; unknown survivor сохраняет state и reservations |
| 8 | `examples/mini/switch_scenario.py`, hook/partial switch tests | Linux / `bacc5c9`, fixes `a825cb3` | passed на Linux, подготовка до остановки, частичный отказ без отката данных |
| 9 | Offline live lifecycle/auth/CF fake, API/WS gate, cookie и unshare tests | Linux / `640f34f`, A2 `38628fa` | partial, offline live/snapshot CLI и A2 re-check прошли на38628fa; real CF зарегистрировался вhel02, publicDNS gaierror, cleanupconfirmed |
| 10 | Confined copy helper: symlinks/specials, независимость от source, failure cleanup | Linux / `640f34f`, A2 `38628fa` | passed на Linux offline, full build/activation/immutable snapshot/failed recreate и public CLI scenario прошли на640f34f, A2 закрыта38628fa |
| 11 | MillDrum check-rdr-backend.py, check-rdr-agent-stop.py, check-rdr-handover.py | Linux / MillDrum a58eee5, f3a3cbe, d74dc85; runner014953c | partial, real watcher/data isolation/backup/active-agent shutdown/двусторонний handover прошли; seed/UI/sign-in/preview ещё проверяются |
| 12 | Lifecycle на Linux и macOS, инструкция twikko | Linux / `301afb9` частично | partial, Darwin arm64/amd64 cross-build прошёл; Mac пока недоступен, пользователь 2026-11-13 оставил runtime-приёмку открытой |
| 13 | Routing/alias tests, `examples/mini/tls_scenario.py` | Linux / `31bb721`, code `1c46f79` | passed на Linux fixtures, hostname collision сохраняет старую адресную книгу, proxy restart сохраняет app RunIDs |
| 14 | Реальный wildcard, DNS и отрицательные cert/key/date проверки | [LOCAL_HTTPS.md](LOCAL_HTTPS.md) | partial, внешний TLS handshake и runner TLS fixtures проверены; пользовательский ingress ещё не подготовлен |
| 15 | `examples/mini/tls_scenario.py`: Secure cookie, WS handshake/HMR frame, SSE first-flush, A→main route | Linux / `31bb721`, code `1c46f79` | passed на Linux synthetic fixtures, CA доверен только тестовому клиенту; реальный домен/браузер отдельно |
| 16 | Public TLS scenario, proxy tests, doctor/open public smoke | Linux / `31bb721`, code `1c46f79` | passed на Linux fixtures, stopped503, foreign listener refusal, reload preservation, read-only diagnostics |
| 17 | Build/start получают одинаковые заранее назначенные dependency addresses | Linux / `12a2457` | passed на Linux, real graph tests проверяют build/start addresses и отсутствие переназначения после build |
| 18 | Discovery, IDs, dry-run, bind и graph lifecycle через публичный JSON без интерактива | Linux / `12a2457` | passed для этапов 1–3; switch/share войдут в итоговый сценарий этапа 8 |
| 19 | `examples/mini/switch_scenario.py`, switch graph/CLI tests | Linux / `bacc5c9`, A1 fixes `a825cb3` | passed на Linux, public JSON и подтверждённый cleanup |
| 20 | TCP/UDP wildcard ownership, proxy loopback, share-only exposure, отсутствие DNS API | Linux / `301afb9` частично | partial, Linux listener checks прошли; Darwin runtime и share впереди, loopback proxy проверен Linux fixtures |
| 21 | Все ежедневные сценарии MillDrum через новую обёртку | Linux / MillDrum ab5436e, runner014953c | partial, wrapper реализован; реальный backend/agent/handover прошли, UI/preview ещё проверяются; пользователь разрешил отключить OAuth только в защищённом preview, см. MIGRATION_MAP |

Для окончательной приёмки каждая строка получает точный commit, выполненную
команду или путь к воспроизводимому сценарию и фактический результат.
Неустранённые ограничения остаются в строке даже при успешных других проверках.
Публикация релиза, push и merge этим документом не разрешаются.

# Приёмка

Это журнал доказательств, а не объявление готовности. `partial` означает,
что часть критерия проверена, но весь критерий ещё не принят. `pending`
означает отсутствие полной проверки. Учебный пример не заменяет второй
реальный проект. Cross-build не заменяет исполнение на macOS.

| PRD | Проверка или сценарий | Платформа / ревизия | Результат и ограничение |
| --- | --- | --- | --- |
| 1 | `examples/real_projects_scenario.py`: реальные MillDrum и `/home/alex/dev/twikko` одним бинарником | Linux / runner `254e2b4`, MillDrum `3f91b31` и `951c863`, twikko config `8b897c0` | passed, три ready-сервиса одновременно; edit/save/cancel и неизменные RunIDs MillDrum; public cleanup подтверждён |
| 2 | `examples/mini/scenario.py`, shared catalog между worktree | Linux / `12a2457` | passed на Linux, публичный сценарий завершился PASS и подтвердил cleanup |
| 3 | Тот же сценарий, независимый catalog и остановка выбранного worktree | Linux / `12a2457` | passed на Linux |
| 4 | `bind`, restart consumer, endpoint identity; dependency startup gate | Linux / `12a2457` | passed на Linux, explicit IDs сохраняются, unhealthy dependency блокирует consumer |
| 5 | Graph tests: cycle before execution, repeat и concurrent up | Linux / `12a2457` | passed на Linux, core graph и config tests прошли с race |
| 6 | Lifecycle failures, фактические state и JSON exit code; hook failures | Linux / `a825cb3` | passed на Linux, hook retry и startup failures проверены; macOS отдельно |
| 7 | Реальный SIGKILL frontend, bootstrap EOF, foreign identity, Child=ZERO recovery | Linux / `a825cb3`, A1 re-check | passed на Linux в проверенном scope; unknown survivor сохраняет state и reservations |
| 8 | `examples/mini/switch_scenario.py`, hook/partial switch tests | Linux / `bacc5c9`, fixes `a825cb3` | passed на Linux, подготовка до остановки, частичный отказ без отката данных |
| 9 | `examples/mini/share_scenario.py` и `cloudflare_scenario.py`: live/snapshot auth, cookie, API/WS, unshare | Linux / production code `254e2b4`, финальные fixture scripts | passed, настоящий Cloudflare HTTPS через системные DNS/TLS, пароль на всех путях, authenticated WebSocket, закрытие публикаций и cleanup; offline сценарий также PASS |
| 10 | Confined copy tests, nested chunks, реальный Vite snapshot и Cloudflare immutable artifact | Linux / `254e2b4`, свежая A2 re-check | passed, вложенные assets читаются через fd; source removal не меняет копию; symlinks/specials/overlap отклоняются; failure cleanup и private log проверены |
| 11 | MillDrum `scripts/check-rdr-{backend,agent-stop,handover,ui}.py`, process-journal guard tests | Linux / runner `014953c`, `254e2b4`; MillDrum `a58eee5`, `3f91b31`, `951c863` | passed в Linux scope: watcher, isolation, backup, active-agent shutdown, seed/UI/sign-in/preview, двусторонний handover; свежая A3 закрыта на `951c863`, agent restart и handover повторно PASS |
| 12 | Lifecycle на Linux и macOS, инструкция twikko | Linux / `254e2b4`, twikko `b8996fc` | partial, Linux и инструкции проверены; Darwin arm64/amd64 cross-build PASS. Mac недоступен, пользователь 2026-11-13 оставил runtime-приёмку открытой |
| 13 | Routing/alias tests, `examples/mini/tls_scenario.py` | Linux / `31bb721`, code `1c46f79` | passed на Linux fixtures, hostname collision сохраняет старую адресную книгу, proxy restart сохраняет app RunIDs |
| 14 | Реальный wildcard, DNS и отрицательные cert/key/date проверки | [LOCAL_HTTPS.md](LOCAL_HTTPS.md) | partial, внешний TLS handshake и runner TLS fixtures проверены; пользовательский ingress ещё не подготовлен |
| 15 | `examples/mini/tls_scenario.py`: Secure cookie, WS handshake/HMR frame, SSE first-flush, A→main route | Linux / `31bb721`, code `1c46f79` | passed на Linux synthetic fixtures, CA доверен только тестовому клиенту; реальный домен/браузер отдельно |
| 16 | Public TLS scenario, proxy tests, doctor/open public smoke | Linux / `31bb721`, code `1c46f79` | passed на Linux fixtures, stopped503, foreign listener refusal, reload preservation, read-only diagnostics |
| 17 | Build/start получают одинаковые заранее назначенные dependency addresses | Linux / `12a2457` | passed на Linux, real graph tests проверяют build/start addresses и отсутствие переназначения после build |
| 18 | Public graph/switch/share/real-project scenarios: discovery, IDs, dry-run, bind, lifecycle и ошибки | Linux / production code `254e2b4` | passed, JSON без интерактива; single/list/file/all switch, публикации, partial registration и неверный selector проверены; cleanup подтверждён |
| 19 | `examples/mini/switch_scenario.py`, switch graph/CLI tests | Linux / `bacc5c9`, A1 fixes `a825cb3` | passed на Linux, public JSON и подтверждённый cleanup |
| 20 | TCP/UDP wildcard ownership, loopback proxy, share-only exposure, отсутствие DNS API | Linux / `254e2b4` | partial, Linux listener checks и real share PASS; реальная проверка wildcard/dual-stack/socket ownership на Darwin остаётся открытой |
| 21 | Ежедневные сценарии MillDrum через новую обёртку, explicit legacy handover | Linux / MillDrum `3f91b31`, guard `951c863`; runner `254e2b4` | passed на disposable data: start/stop/switch, watcher, logs, seed, exec, sign-in, API/WS и protected preview; OAuth отключён только там по решению пользователя. Рабочие данные пользователя не переносились |

Для окончательной приёмки каждая строка получает точный commit, выполненную
команду или путь к воспроизводимому сценарию и фактический результат.
Неустранённые ограничения остаются в строке даже при успешных других проверках.
Публикация релиза, push и merge этим документом не разрешаются.

## Итог Linux и открытые условия

Production code runner `254e2b4` прошёл `go test ./... -count=1`,
`go vet ./...` и `go test -race ./... -count=1`. Последний полный race-run
занял 156.803s в runner и 26.363s в publication. Darwin arm64 и amd64
собраны успешно. Public mini, switch и offline share завершились PASS.

Воспроизводимые реальные сценарии из корня runner:

```sh
python3 examples/real_projects_scenario.py /absolute/rdr /home/alex/dev/milldrum /home/alex/dev/twikko
CLOUDFLARED_BIN=/absolute/cloudflared python3 examples/mini/cloudflare_scenario.py /absolute/rdr
```

В настоящем Cloudflare проверены live и snapshot HTTPS, password gate,
cookie, WebSocket, независимость snapshot от source и unshare. Ранние DNS/WS
ошибки fixtures исправлены, окончательный сценарий завершился PASS. DNS,
системное доверие и существующие туннели пользователя не менялись.

Команды миграционной приёмки и ограничения находятся в
[MillDrum RDR_MIGRATION.md](/home/alex/dev/milldrum/docs/RDR_MIGRATION.md),
второй проект описан в [twikko RUNDEV.md](/home/alex/dev/twikko/docs/RUNDEV.md).
Все сценарии используют synthetic data, сохраняют HOME, изолируют runtime и
подтверждают остановку своих процессов перед удалением fixtures. В неудачных
попытках диагностические каталоги сохранены после подтверждённой остановки.

Итоговая приёмка открыта по критериям 12/20 для реальной macOS и 14 для
пользовательского HTTPS-входа `home.litvinovich.dev`. Linux TLS fixtures и
настоящий Cloudflare не заменяют эту проверку. Mac пока недоступен по решению
пользователя. Подготовка DNS/PEM/route требует отдельной настройки готовой
инфраструктуры, описанной в [LOCAL_HTTPS.md](LOCAL_HTTPS.md).

В MillDrum сравнение clean archives `1184e35` и `3f91b31` подтвердило
существующий Linux failure `TestOpsServiceInstallStatusUninstall` из-за
отсутствия `/bin/launchctl`. Этот несвязанный тест не изменялся. Ранее
наблюдавшийся ingest failure не повторился на обеих ревизиях.

Локальный бинарник установлен 2026-11-13 в `/home/alex/.local/bin/rdr`
из production code `254e2b4`, Go 1.27.4. `version --json` сообщает
`0.1.0-dev`, API 1; справка проверена. SHA256:
`d62a45efbb57509153fa3c281a292a1ff6e824afe46101a44a44c82971348b17`.
Именно установленный бинарник повторно прошёл совместный сценарий с
MillDrum `951c863` и twikko `b8996fc`, включая save/cancel и cleanup.
Последующий MillDrum `0cfc7f1` добавляет только проверку restart и журнал
приёмки, production code остаётся `951c863`.

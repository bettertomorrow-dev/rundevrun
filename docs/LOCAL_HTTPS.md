# Локальная HTTPS-среда

Пользователь выбрал `home.litvinovich.dev` для последующей приёмки.
Наблюдения ниже получены read-only 2026-11-13. Настройки DNS, proxy,
сертификатов и разрешений не изменялись.

- `home.litvinovich.dev` и пробное имя `rdr-check.home.litvinovich.dev`
  разрешаются в `192.168.100.22` в текущей среде.
- TLS handshake с `home.litvinovich.dev:443` проверен системным trust store.
  Сертификат Let's Encrypt, issuer YE1, SAN `home.litvinovich.dev` и
  `*.home.litvinovich.dev`, срок до 2026-12-22 05:51:03 UTC.
- Порты 80, 81 и 443 обслуживает контейнер
  `nginx-proxy-manager-app-1`, образ `jc21/nginx-proxy-manager:latest`.
  В nginx настроены отдельные существующие host routes. Наличие wildcard
  в сертификате само по себе не создаёт route для hostname runner.
- Nginx использует `/etc/letsencrypt/live/npm-4/fullchain.pem` и
  `/etc/letsencrypt/live/npm-4/privkey.pem` внутри контейнера.
  `/etc/letsencrypt` смонтирован из
  `/media/storage/selfhost/nginx-proxy-manager/letsencrypt`.
  Оба соответствующих host path недоступны текущему пользователю на чтение.
  Содержимое приватного ключа не читалось и не копировалось.

Это подтверждает существующий HTTPS-вход, но ещё не приёмку runner.
Перед этапом 5 требуется подготовить конкретную схему подключения:
либо предоставить runner доступ к готовой PEM-паре и локальному входу,
либо настроить разрешённый terminating proxy с сохранением Host и схемы.
Режим `routing.tls.mode = "external"` использует только публичный PEM
сертификата и не требует доступа runner к приватному ключу NPM. Он требует
явных `external_https_port` и `trusted_proxies`, а backend слушает loopback HTTP.
Это отдельный явный режим, не автоматический переход после ошибки TLS.
Доступ контейнера к host loopback и доверенный адрес proxy необходимо
проверить отдельно. Существующие routes и внешний доступ не менять
автоматически. Проверка TLS runner, DNS его hostname, cookies, WebSocket,
SSE и reload пока не выполнена.

Текущий DNS указывает на LAN-адрес, тогда как PRD требует loopback для локальных
имён runner. Наличие работающих домашних routes NPM не подтверждает этот
критерий. Фактическая настройка DNS и HTTPS-входа для runner остаётся отдельным
пользовательским условием приёмки.

## Конфигурация готовой среды

Файл машины находится в `$XDG_CONFIG_HOME/rundevrun/config.toml` либо
`~/.config/rundevrun/config.toml`. Проектный `dev.toml` не хранит ключи.
Отсутствующий файл означает отключённую маршрутизацию. Ошибка включённой
TLS-конфигурации останавливает планирование запуска до сборки.

Пример прямого TLS listener с уже подготовленными PEM-файлами:

```toml
version = 1
[routing]
enabled = true
domain = "home.litvinovich.dev"
http_listen = "127.0.0.1:8080"
https_listen = "127.0.0.1:8443"
[routing.tls]
mode = "provided"
[[routing.tls.certificates]]
cert_file = "/absolute/path/fullchain.pem"
key_file = "/absolute/path/privkey.pem"
```

Этот пример даёт URL с `:8443`. Для URL без порта нужен доступный HTTPS-вход
443. Заранее настроенная пересылка 443 на listener допускает явный
`external_https_port = 443` в `[routing]`. Сам runner пересылку не создаёт.
Все адреса listener остаются loopback, а DNS всех используемых имён должен
указывать исключительно на loopback. Wildcard SAN покрывает одну метку,
например `api-feature-project.home.litvinovich.dev`.

Если TLS завершает готовый доверенный локальный proxy, конфигурация отличается:

```toml
version = 1
[routing]
enabled = true
domain = "home.litvinovich.dev"
http_listen = "127.0.0.1:8080"
external_https_port = 443
trusted_proxies = ["127.0.0.1"]
[routing.tls]
mode = "external"
[[routing.tls.certificates]]
cert_file = "/absolute/path/public-fullchain.pem"
```

В external-режиме `key_file` запрещён. TLS proxy должен сохранять Host,
отправлять ровно один `X-Forwarded-Proto: https` и подключаться с явно
доверенного loopback-адреса. Runner открывает HTTP backend и приватный
control socket. Контейнер NPM с текущим bridge-доступом нельзя считать таким
входом без отдельной проверки сети. Доверие LAN-адресу не подходит этому
контракту. Публичный сертификат для runner должен обновляться вместе с
сертификатом terminating proxy.

После подготовки конфигурации можно выполнить обычный `up`; он автоматически
запускает нужный proxy. `--no-proxy` пропускает автоматический запуск proxy,
сохраняя запланированные HTTPS-адреса. `rdr proxy status --json` показывает
состояние proxy. `rdr proxy reload --dry-run --json` проверяет активный файл
конфигурации без замены. `rdr proxy reload --json` устанавливает только полностью
валидную конфигурацию; ошибка сохраняет прежнюю. Изменение listener или режима
TLS требует остановки и следующего запуска proxy. `rdr proxy stop --json`
сохраняет приложения работающими.

Тестовый CA допустимо передавать отдельному тестовому HTTP-клиенту. Это не
подтверждает системное доверие браузера и не устанавливает CA в trust store.

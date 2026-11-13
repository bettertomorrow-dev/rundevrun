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
Доступ контейнера к host loopback и доверенный адрес proxy необходимо
проверить отдельно. Существующие routes и внешний доступ не менять
автоматически. Проверка TLS runner, DNS его hostname, cookies, WebSocket,
SSE и reload пока не выполнена.

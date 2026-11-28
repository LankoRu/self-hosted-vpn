# shvpn — VPS с 3x-ui + telemt, наружу только порт 443

Полная инструкция по развёртыванию схемы: на сервере с уже установленной
панелью **3x-ui** добавляется **telemt** (MTProto-прокси для Telegram),
а весь входящий трафик собирается на **единственном публичном порту 443**
через SNI-роутер nginx. Сканеры и браузеры видят обычный HTTPS-сайт.

## Архитектура

```
Интернет ──► :443  nginx stream (ssl_preread, чистый L4-релей, PROXY protocol v1)
              │
              ├─ SNI = TG_DOMAIN (напр. petrovich.ru)
              │     └─► 127.0.0.1:8443  telemt
              │           ├─ валидный ee-секрет ─► MTProto ─► Telegram
              │           └─ иначе (сканер/браузер) ─► mask ─► настоящий TG_DOMAIN:443
              │
              ├─ SNI ∈ realitySettings.serverNames (google.com, g.co, …)
              │     └─► 127.0.0.1:10090  Xray VLESS+Reality (acceptProxyProtocol)
              │           └─ не-VLESS клиент ─► Reality сам шлёт на свою цель (google.com)
              │
              └─ без SNI / любой другой SNI (в т.ч. https://IP/)
                    └─► 127.0.0.1:8444  nginx HTTPS (Let's Encrypt сертификат на IP)
                          ├─ /                    → сайт-заглушка (/var/www/shvpn)
                          ├─ <webBasePath>/       → панель 3x-ui   (127.0.0.1:40323, HTTP)
                          └─ <subPath>/ и др.     → подписки 3x-ui (127.0.0.1:10091, HTTP)
```

Почему так, а не «telemt на 443 + exclusive_mask»:

- mask-релей telemt **ограничивает** неаутентифицированные соединения
  (`mask_relay_max_bytes` ≤ 64 МБ, жёсткий `mask_relay_timeout_ms`) — для
  постоянного VLESS-туннеля это недопустимо (обрывы). nginx stream — чистый
  TCP-релей без лимитов.
- telemt при этом занимается только Telegram-трафиком, а его встроенный mask
  отдаёт сканерам с SNI домена-маски **настоящий** сайт (сертификат совпадает
  с SNI — максимальная правдоподобность).
- PROXY protocol v1 передаёт реальные IP клиентов всем трём бэкендам
  (telemt: `proxy_protocol=true`, Xray: `tcpSettings.acceptProxyProtocol=true`,
  nginx: `real_ip_header proxy_protocol`).
  **Внимание:** `realitySettings.xver` — это НЕ приём PROXY от клиента, а
  отправка PROXY-заголовка на целевой сайт Reality (google.com). Его нужно
  держать `0`, иначе Google получает мусор и рвёт соединения.

Что торчит наружу: **22 (SSH) и 443**. Всё остальное слушает только 127.0.0.1
и дополнительно закрыто ufw. Порт 80 открывается на несколько секунд только
во время продления сертификата (acme.sh pre/post-хуки).

## Состав проекта

```
install.sh                    — интерактивный установщик (запуск на VPS, curl|bash)
scripts/lib.sh                — хелперы (вопросы, рендер шаблонов, swap, API)
scripts/setup-base.sh         — база: 3x-ui + telemt + nginx + серты + ufw
scripts/setup-awg.sh          — AmneziaWG 3.1: kernel-модуль (+патч) + инбаунд UDP-443
scripts/setup-vps-admin.sh    — vps-admin: prebuilt из Releases + конфиг + nginx
scripts/xui-localhost.py      — правка БД 3x-ui (listen, externalProxy, пути)
scripts/awg-kernel-patch.py   — патч модуля AWG под Ubuntu-бэкпорт API ядра 7.0
config/*.tpl                  — шаблоны конфигов (telemt, nginx, vps-admin)
config/site/index.html        — сайт-заглушка
vendor/vps-admin/             — исходники vps-admin (Go; бинарь собирает CI в Releases)
patches/                      — рецепты патчей (amneziawg-ubuntu70.md)
servers/<IP>-RESULT.txt       — итоги развёртывания (ссылки, секреты; НЕ в git)
```

## Быстрый путь (новый VPS)

На чистом VPS (Ubuntu 24.04/26.04, Debian 13; от root):

```bash
curl -fsSL https://raw.githubusercontent.com/LankoRu/self-hosted-vpn/main/install.sh | bash
```

или из склонированного репозитория: `bash install.sh`.

Установщик спросит: IP, домен-маску Telegram, ставить ли AWG (и kernel mode),
ставить ли vps-admin (bot token + chat_id). Всё остальное сделает сам:
поставит 3x-ui (если нет), создаст VLESS+Reality инбаунд, telemt, nginx
SNI-роутер, сайт-заглушку, LE-сертификат на IP, AWG-инбаунд на UDP-443,
vps-admin (prebuilt-бинарь из GitHub Releases, sha256-проверка), настроит ufw (22 + 443) и выдаст итоговый отчёт
со всеми ссылками и паролями (`/root/shvpn-result.txt`).

Автоматизация (без интерактива) — всё флагами запуска:
`curl ... | bash -s -- --yes --bot-token ... --bot-chat ...`.
Остальные: `--server-ip`, `--tg-domain`, `--tg-secret`, `--with-awg/--no-awg`,
`--awg-kernel/--awg-userspace`, `--with-vpsadmin/--no-vpsadmin`, `--vpsadmin-version`,
`--repo`, `--branch`, `--github-token` (приватный репо, Contents: Read — нужен и для
тарбола, и для Release-ассетов). Полный список: `install.sh --help`.
Все вопросы имеют дефолты; без --bot-token/--bot-chat при --yes — понятная ошибка.

`--no-bot` ставит vps-admin без Telegram: веб-панель и сбор статистики работают,
уведомлений и команд бота нет. Нужно, когда токен уже занят другим сервером.

**Токен бота нельзя использовать на двух серверах одновременно**: Telegram
держит только одну long-poll сессию (`getUpdates`), второй получает
`409 Conflict: terminated by other getUpdates request`. Установщик проверяет
журнал после старта и предупреждает, если конфликт есть.
Повторный запуск использует сохранённые значения (/etc/shvpn/env.sh), флаги их переопределяют.

Повторный запуск идемпотентен: существующие компоненты не ломаются, пользователи
telemt сохраняются, конфиг vps-admin не затирается.

SSH-доступ по ключу (шпаргалка): `ssh-copy-id -i ~/.ssh/id_ed25519.pub root@IP`.

## Что делается вручную (то же самое, что в скрипте)

Ниже — полный ручной путь для понимания/отладки. Всюду считаем:
`IP` — публичный адрес, `TG_DOMAIN=petrovich.ru`, пути/порты берём из 3x-ui.

### 1. Зависимости

```bash
apt-get update && apt-get install -y nginx libnginx-mod-stream sqlite3 python3 curl openssl
```

### 2. telemt

```bash
curl -fsSL https://raw.githubusercontent.com/telemt/telemt/main/install.sh \
  | sh -s -- -p 8443 -s <32hex-секрет> -d petrovich.ru
systemctl stop telemt
```

Заменить `/etc/telemt/telemt.toml` конфигом из `config/telemt.toml.tpl`
(подставить IP, домен, секрет, порт). Ключевое:

- `port = 8443`, `[[server.listeners]] ip = "127.0.0.1"` — только localhost;
- `proxy_protocol = true` + `proxy_protocol_trusted_cidrs = ["127.0.0.1/32"]`;
- `mask_host = "petrovich.ru"`, `mask_port = 443` — сканеры уходят на настоящий сайт;
- `[general.links] public_host/public_port` — внешние IP:443 для tg-ссылок.

```bash
chown root:telemt /etc/telemt/telemt.toml && chmod 640 /etc/telemt/telemt.toml
systemctl start telemt
```

### 3. Сертификат на IP (acme.sh)

3x-ui при установке с SSL уже выпускает короткоживущий (7 дней) LE-сертификат
на IP и кладёт его в `/root/cert/ip/`. Если сертификата нет:

```bash
ufw allow 80/tcp                                   # временно, для валидации
~/.acme.sh/acme.sh --issue --standalone -d IP --ecc --server letsencrypt
ufw delete allow 80/tcp
```

Настроить продление (cron acme.sh уже есть от установщика 3x-ui):

```bash
~/.acme.sh/acme.sh --install-cert -d IP --ecc \
  --key-file /root/cert/ip/privkey.pem \
  --fullchain-file /root/cert/ip/fullchain.pem \
  --reloadcmd "systemctl reload nginx" \
  --pre-hook "ufw allow 80/tcp" \
  --post-hook "ufw delete allow 80/tcp"
```

Продление происходит автоматически каждые ~3 дня; :80 открыт только на время
валидации (секунды).

### 4. nginx

- `config/stream-router.conf.tpl` → `/etc/nginx/stream.d/shvpn-router.conf`.
  В map перечислить **все** SNI из `realitySettings.serverNames` каждого
  VLESS-инбаунда (скрипт делает это сам из БД).
- В конец `/etc/nginx/nginx.conf` добавить (если нет):
  ```nginx
  stream {
      include /etc/nginx/stream.d/*.conf;
  }
  ```
- `config/web.conf.tpl` → `/etc/nginx/sites-available/shvpn-web`,
  симлинк в `sites-enabled`, удалить `sites-enabled/default`.
  Подставить пути панели/подписок (`webBasePath`, `subPath`, `subJsonPath`,
  `subClashPath` из таблицы `settings` БД `/etc/x-ui/x-ui.db`) и порты.
- Сайт: `config/site/index.html` → `/var/www/shvpn/index.html`.

```bash
nginx -t && systemctl enable --now nginx
```

### 5. 3x-ui на localhost

**Остановить панель** и поправить БД (это делает `scripts/xui-localhost.py`):

```bash
systemctl stop x-ui
SERVER_IP=IP python3 scripts/xui-localhost.py
systemctl start x-ui
```

Изменения:

- `settings`: `webListen`/`subListen` = `127.0.0.1`; `webCertFile`/`webKeyFile`/
  `subCertFile`/`subKeyFile` = пусто (TLS терминирует nginx);
  `subURI`/`subClashURI`/`subJsonURI` = `https://IP/<path>`.
- `inbounds`: `listen` = `127.0.0.1`; `tcpSettings.acceptProxyProtocol = true`
  (приём PROXY v1 от nginx; для ws/grpc — аналогичное поле в их settings);
  `realitySettings.xver = 0` (см. предупреждение выше);
  `stream_settings.externalProxy = [{"dest":"IP","port":443,"forceTls":"same"}]` —
  благодаря этому все ссылки/QR/подписки указывают на `IP:443`, хотя Xray слушает
  локальный порт. **`forceTls:"same"` обязателен** — без него 3x-ui вырезает из
  ссылок Reality-параметры (sni/pbk/sid/fp). **Старые клиентские ссылки с прежним
  портом перестают работать — раздать новые из панели.**

### 6. Firewall

```bash
ufw allow 22/tcp
ufw allow 443/tcp
ufw enable        # если ещё не включён; default deny incoming
ufw status verbose
```

## Проверка

Снаружи (с локальной машины):

```bash
# открыты только 22 и 443
nc -zv -w2 IP 22 443 80 8443 10090 40323 2>&1 | grep -E 'succeeded|open'

# сайт-заглушка с валидным сертификатом
curl -sS https://IP/ | head -5
curl -sSI https://IP/ | head -3

# панель (ожидаем 200/302 на странице логина)
curl -sS -o /dev/null -w '%{http_code}\n' https://IP/<webBasePath>/

# SNI=google.com должен отдать сертификат Google (Reality-цель), не наш
echo | openssl s_client -connect IP:443 -servername google.com 2>/dev/null \
  | openssl x509 -noout -subject

# SNI=petrovich.ru должен отдать сертификат настоящего petrovich.ru (mask telemt)
echo | openssl s_client -connect IP:443 -servername petrovich.ru 2>/dev/null \
  | openssl x509 -noout -subject
```

Функционально:

1. **Telegram**: открыть `tg://proxy?server=IP&port=443&secret=ee<secret><hex(TG_DOMAIN)>`
   (готовая ссылка — в RESULT.txt), включить прокси, проверить отправку сообщений.
2. **VLESS**: импортировать новую ссылку из панели (порт 443), проверить доступ в интернет.
3. **Панель**: зайти по `https://IP/<webBasePath>/`, проверить статус Xray и клиентов.
4. **Подписка**: добавить `https://IP/<subPath>/<subId>` в клиент (v2rayNG и т.п.).

## Эксплуатация

### Добавить пользователя Telegram

В `/etc/telemt/telemt.toml`:

```toml
[access.users]
hello = "..."
alice = "<новые 32 hex>"   # openssl rand -hex 16
```

`systemctl reload telemt` (или restart). Ссылка:
`ee<secret><hex(TG_DOMAIN)>`, сервер `IP`, порт `443`.
Секреты и ссылки текущего сервера — в `servers/<IP>-RESULT.txt`.

### Сменить домен-маски Telegram

`TG_DOMAIN` в telemt.toml + та же строка в map stream-роутера;
`systemctl restart telemt && systemctl reload nginx`.
**Все ранее выданные tg-ссылки станут невалидными** (домен зашит в секрет).

### WEB-прокси Telegram (HTTPS, Desktop 7.1+)

Новый тип прокси Telegram **WEB**: MTProto идёт внутри обычных HTTPS/WebSocket-
запросов к **вашему** домену, снаружи неотличимо от посещения сайта (в отличие от
FakeTLS, который DPI уже фингерпринтит). Реализован в telemt ≥ 3.5.1.

**Включение** — флаг установщика `--tg-web-domain <ваш-домен>`:
```bash
bash install.sh --tg-web-domain tg.example.duckdns.org   # + остальные флаги
```
Требования к домену:
- **Настоящий FQDN, не IP** — telemt WEB отклоняет IP-литералы. Подходит и
  бесплатный поддомен DuckDNS/No-IP, и дешёвый домен (.xyz/.top/.ru).
- **A-запись на IP сервера** (установщик проверяет DNS перед выпуском серта).
- Домен **не должен совпадать** с доменом-маской FakeTLS (`TG_DOMAIN`) или IP.

Что делает установщик:
1. Выпускает LE-серт на домен (HTTP-01, обычный профиль; :80 открывается на секунды).
2. Дописывает к `/etc/telemt/telemt.toml` WEB-листенер `127.0.0.1:18080`
   (`transport="web"`, без PROXY-протокола), `[web]` и vhost с dd-профилем на
   каждого пользователя `[access.users]`. Сайт-прикрытие — снимок `/var/www/shvpn`.
3. Поднимает nginx-vhost `shvpn-telemt-web` (SNI/Host = домен → терминирует TLS →
   plain HTTP в telemt). SNI домена в stream-карте попадает в `default web`,
   поэтому **SNI-карту менять не нужно**.

Конфиг валидируется (`telemt healthcheck`) **до** остановки telemt — невалидный
конфиг не роняет работающий прокси.

**Ссылки пользователей**: `tg://webproxy?server=<домен>&secret=dd<32hex>`. Их
выдаёт vps-admin (панель/бот, пункт «WEB (HTTPS, Desktop 7.1+)») — он сам создаёт
WEB-профиль при провижининге и убирает перед удалением пользователя. WEB не
поддерживает `ee`-секреты, используется `dd` с тем же секретом, что и MTProto.

**Проверка**:
```bash
curl -sI https://<домен>/                 # сайт-прикрытие (decoy), валидный серт
telemt healthcheck /etc/telemt/telemt.toml # конфиг валиден
```
Затем подключить WEB-ссылку в Telegram Desktop 7.1+ (Settings → Advanced →
Connection Type → WEB proxy).

**Выключение**: перезапустить установщик без `--tg-web-domain` (или с пустым) —
WEB-блок и nginx-vhost убираются, MTProto/FakeTLS остаётся.

### Логи

```bash
journalctl -u telemt -f          # Telegram-прокси
journalctl -u x-ui -f            # панель + Xray
tail -f /var/log/nginx/error.log # роутер/сайт
```

### Обновление telemt

```bash
curl -fsSL https://raw.githubusercontent.com/telemt/telemt/main/install.sh | sh
```
Установщик сохраняет существующий конфиг (обновляет только бинарник/сервис).

### Откат

```bash
systemctl stop telemt nginx
# вернуть в БД прежние listen/порты (или восстановить бэкап x-ui.db)
systemctl start x-ui
ufw delete allow 443/tcp
```

## Расширение: AmneziaWG 3.1 + vps-admin

Проверенная связка (развёрнута на эталонном сервере): AWG-инбаунд в 3x-ui
(userspace или kernel) + vps-admin (бот/веб-панель, регистрация во всех
сервисах, единый трафик на пользователя).

```
:443/tcp  nginx SNI-роутер → telemt / Xray Reality / сайт+панели
:443/udp  AmneziaWG 3.1 (инбаунд 3x-ui, kernel mode)
:22/tcp   SSH
```

### 1. AWG-инбаунд в 3x-ui

- Панель → Add Inbound → **AmneziaWG**, порт **443** (UDP), обфускация 3.1
  генерируется сама (HeaderProtectionKey и т.д.).
- **Kernel mode** (опционально, быстрее): требует модуль ядра:
  `sudo /usr/local/x-ui/x-ui install-awg-kernel`.
  **На Ubuntu 26.04 (ядро 7.0) сборка падает** — нужен upstream-патч
  (backport API `setup_udp_tunnel_sock`): см. [patches/amneziawg-ubuntu70.md](patches/amneziawg-ubuntu70.md).
- Интерфейс (`xui-awg<N>`) поднимается только когда есть **хотя бы один
  клиент** — это нормально, не баг.
- `ufw allow 443/udp`.
- Чтобы ссылки/подписки не содержали `Endpoint = localhost` (панель берёт
  хост из Host-заголовка API-запроса), инбаунду нужен externalProxy:
  `stream_settings.externalProxy = [{"dest":"<IP>","port":443}]`
  (делает `scripts/xui-localhost.py` автоматически).

### 2. telemt: права для провижининга через API

vps-admin создаёт пользователей через control-API telemt, который атомарно
перезаписывает config.toml (tmp+rename+fchown на владельца оригинала).
Если файл принадлежит root — non-root процесс telemt получает EPERM.
Поэтому:

```bash
chown telemt:telemt /etc/telemt /etc/telemt/telemt.toml
chmod 770 /etc/telemt && chmod 660 /etc/telemt/telemt.toml
```

(setup-base.sh уже так делает). Повторный прогон setup-base.sh **сохраняет**
существующих пользователей telemt (секция [access.users] переносится в новый
конфиг), но вручную правленный шаблонные параметры перезапишет.

### 3. vps-admin

Исходники: `vendor/vps-admin` (github.com/LankoRu/vps-admin). AWG — через
3x-ui (единственный бэкенд, адаптер internal/services/awg3xui): клиент
создаётся/прикрепляется через API панели, конфиги забираются готовые
(vpn:// + .conf), трафик — clientStats своего инбаунда.

Деплой:

```bash
cd vendor/vps-admin
make deploy DEPLOY_HOST=root@IP        # нужен SSH-ключ (см. ~/.ssh/config)
# заполнить /etc/vps-admin/config.yaml (chmod 600):
```

Ключевые поля конфига для нашей схемы:

```yaml
services: {xray: {enabled: true}, amneziawg: {enabled: true}, telemt: {enabled: true}}
xray_3xui:
  base_url: "http://127.0.0.1:40323"      # порт панели из БД 3x-ui
  web_base_path: "<webBasePath без слэшей>"
  api_token: "<Settings → Security → API Token>"
  inbound_id: 1                            # VLESS-инбаунд
  sub_url: "https://IP/<subPath>"
  public_host: "IP"
amneziawg_3xui:
  base_url: "http://127.0.0.1:40323"
  web_base_path: "<webBasePath>"
  api_token: "<тот же токен>"
  inbound_id: 2                            # AWG-инбаунд
  public_host: "IP"
telemt: {base_url: "http://127.0.0.1:9091", admin_key: ""}
web:
  enabled: true
  listen: "127.0.0.1:8088"
  password: "<сгенерировать>"              # TLS терминирует nginx
  tls: {enabled: false}
telegram: {token: "<bot token>", chat_id: <admin id>}
```

### 4. nginx: панель vps-admin за 443

UI vps-admin использует абсолютные пути (/api/…, /p/…), base_path нет.
Схема (шаблон `config/vps-admin-locations.conf.tpl`, рендерится в
`/etc/nginx/snippets/vps-admin-locations.conf`, подключается wildcard-include
внутри server-блока shvpn-web):

- страница панели: **секретный путь** `/vps-<hex>/` → proxy на 8088 (префикс срезается);
- `/api/*` — только при наличии cookie `vps_session` (сканеры видят 404);
- открыты: `/api/login` (с limit_req zone=vpsadmin_login 5r/m), `/api/qr`
  (QR для публичных страниц), `/p/*` (клиентские ссылки с ключами по токену);
- PWA-статика за cookie; **PWA-установка не работает** (start_url="/" ведёт на
  заглушку) — осознанный компромисс, панель открывается по секретному пути.

```bash
# рендер (подставить путь и порт):
sed -e 's|__VPSADMIN_PATH__|/vps-<hex>/|g' -e 's|__VPSADMIN_PORT__|8088|g' \
  config/vps-admin-locations.conf.tpl > /etc/nginx/snippets/vps-admin-locations.conf
nginx -t && systemctl reload nginx
```

В web.conf.tpl уже есть `limit_req_zone … vpsadmin_login` и
`include /etc/nginx/snippets/vps-admin-*.conf;` (терпит отсутствие файла).

## Подключения (антиблокировка)

В панели vps-admin карточка **«Подключения»** (или `GET /api/connections`):

- **Reality-SNI/dest/fingerprint** — ротация при блокировке текущих доменов.
  Пресеты (Google/Microsoft/Apple/Amazon/Samsung) + свой список. Перед применением
  цель проверяется с сервера (TLS 1.3 + H2). Xray перезапускается; VLESS-подписка
  обновляется сама, статические ссылки перевыпускаются в карточках юзеров.
- **Домен-маска telemt** — смена tls_domain/mask_host + restart. ВСЕ tg://-ссылки
  меняются (домен зашит в secret) — раздать новые. Домен обязан отличаться от
  Reality-SNI (nginx маршрутизирует по SNI) и быть реальным HTTPS-сайтом.
- **Запасной транспорт XHTTP/gRPC** — второй инбаунд на localhost:10092 со своим
  набором SNI; все xray-клиенты прикрепляются, в их конфигах/подписке появляется
  второй вариант. Выключение — гасит инбаунд и открепляет клиентов.
- SNI-карта `/etc/nginx/stream.d/shvpn-sni-map.conf` принадлежит vps-admin
  (атомарная запись + nginx -t + reload с откатом). Вручную не править.
- Коллизии SNI запрещены: telemt-домен ∩ Reality-SNI ∩ backup-SNI = ∅.

### 5. Семантика клиентов (важно)

- **Создание — только по отображаемому имени**: веб-форма и бот `/register Вася Пупкин`
  шлют только имя; логин генерируется транслитом (vasya-pupkin), при коллизии
  добавляется -2/-3. Логин — identity в сервисах и ключ истории трафика.
- **AWG-клиент — отдельная учётка `<логин>-awg`** (не общая с xray!): в 3x-ui
  трафик хранится одной строкой на email (`client_traffics`), общая учётка на
  двух инбаундах давала одинаковые clientStats в обоих → задвоение. Разные
  email → разные строки → честный per-service учёт. Адаптер: add/del (не
  attach/detach), identity в vps-admin = `<логин>-awg`.
- Подписка xray — своя (vless); AWG отдаётся готовыми vpn://+.conf (подписка
  для AWG не нужна). `/regen amneziawg` (del+add) **полностью ротирует ключи**.
- `/regen xray` и AWG не пересекаются (учётки разные).
- **Multi-device AWG**: один ключ параллельно с двух устройств не работает
  (WireGuard roaming) → команда бота `/device <имя> <устройство>` или кнопка
  «＋ Устройство AWG» в карточке юзера создаёт доп. учётку `<логин>-awg-<устройство>`
  (транслит); трафик всех устройств суммируется на пользователя. Удаление —
  `/deldevice` или 🗑 в карточке (стирается и история трафика устройства).
  MTProto/VLESS в этом не нуждаются — один секрет/UUID на все устройства.
- Трафик: xray-коллектор пропускает amneziawg/wireguard-инбаунды; AWG-коллектор
  читает только свой inbound_id. При миграции старых общих учёткок:detach от
  AWG-инбаунда + `UPDATE client_traffics SET inbound_id=<xray>` для старых строк
  (иначе они продолжают светиться в clientStats AWG).

### 6. Проверка

```bash
curl -sS https://IP/<секретный-путь>/                     # 200, форма входа
curl -sS -o /dev/null -w '%{http_code}' https://IP/api/overview   # 404 без cookie
curl -sS -c jar -H 'Content-Type: application/json' \
  -d '{"password":"<пароль>"}' https://IP/api/login        # {"ok":true}
curl -sS -b jar https://IP/api/overview                    # JSON со статистикой
# регистрация во всех сервисах:
curl -sS -b jar -H 'Content-Type: application/json' \
  -d '{"username":"testuser"}' https://IP/api/users
# удалить: curl -X DELETE -b jar https://IP/api/users/testuser
```

Бот: /register <имя>, /today, /listusers, /digest; алерты о поломках приходят
в telegram.chat_id. Логи: `journalctl -u vps-admin -f`.

## Подводные камни / примечания

- **Скан портов может врать**: провайдер использует SYN-прокси/анти-DDoS,
  который завершает TCP-handshake на ЛЮБОМ порту. `nc -zv` покажет «open» даже
  для закрытых портов. Проверять надо реальным запросом (`curl`): на закрытых
  портах данные не проходят (HTTP 000 / reset). Реальная картина — `ss -tlnp`
  на сервере + правила ufw.
- **Reality serverNames**: если в панели добавляются новые SNI цели — их нужно
  добавить в map `/etc/nginx/stream.d/shvpn-router.conf` (или перезапустить
  `setup-base.sh` — он перечитает БД). Иначе трафик с новым SNI уйдёт на
  сайт-заглушку и VLESS не сработает.
- **Не путать xver и acceptProxyProtocol**: `realitySettings.xver>0` отправляет
  PROXY-заголовок целевому сайту (google.com) и ломает и VLESS, и маскировку.
  Приём PROXY от nginx — только через `acceptProxyProtocol` на транспорте.
- **externalProxy без forceTls="same"** порождает «голые» vless-ссылки без
  sni/pbk/sid — клиент не подключится. Скрипт это учитывает.
- **Новые инбаунды на других портах**: скрипт создаёт по апстриму на порт;
  вручную — не забыть `upstream xray_<port>` и строки map.
- **Docker** обходит ufw через свои iptables-цепочки. Если появятся контейнеры
  с published-портами — они будут доступны снаружи; не публиковать лишнее.
- **Сертификат на IP** живёт 7 дней. Если продление сломается (проверить
  `~/.acme.sh/acme.sh --cron`), сайт/панель начнут показывать просроченный
  сертификат. Мониторить: `openssl x509 -in /root/cert/ip/fullchain.pem -noout -enddate`.
- **1 ГБ RAM**: telemt (~30–60 МБ), nginx (~10 МБ), 3x-ui+Xray (~200 МБ) —
  укладывается с запасом. Сборка telemt из исходников на 1 ГБ не рекомендуется
  (fat LTO) — использовать готовые бинарники (так и делает install.sh).
- **Панель и подписки** больше не имеют собственного TLS — не открывать их
  порты (40323/10091) наружу даже временно: трафик до nginx идёт по HTTP
  только внутри localhost.
- Секреты (TG_SECRET, пароли панели) не коммитить в публичные репозитории.
  Файлы `servers/*-RESULT.txt` содержат секреты.

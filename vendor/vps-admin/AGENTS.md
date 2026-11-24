# AGENTS.md — vps-admin

Guide for working in this repo. Read before making changes.

## What this is

`vps-admin` — a Go service that runs on a VPS via systemd. It:
1. Collects traffic stats from three VPN services (xray/3x-ui, AmneziaWG-in-Docker, telemt).
2. Registers a user in **all** services at once (one canonical user ↔ per-service
   identity), so traffic sums per person.
3. Is managed via a **Telegram bot** and a **web panel**.

Reference project (original inspiration): `../vpn-stats`.

## Commands

```bash
make build          # local binary → bin/vps-admin
make build-linux    # static linux/amd64 → bin/vps-admin-linux-amd64
make test           # one-shot collection run (needs a config)
go test ./...       # unit tests
go build ./... && go vet ./...   # always run before finishing

make deploy DEPLOY_HOST=root@HOST [SSH_PORT=22]   # build-linux + ship over SSH
make logs   DEPLOY_HOST=root@HOST                 # journalctl -u vps-admin -f
```

CI и auto-release живут в КОРНЕ дистрибутив-репо self-hosted-vpn
(`.github/workflows/ci.yml`, working-directory: vendor/vps-admin): push в main →
auto-тег v0.1.x → GitHub Release с prebuilt linux amd64/arm64 + sha256sums.txt.
Установщик self-hosted-vpn качает бинарь из Releases (на VPS Go не нужен); make deploy —
путь разработки. Вложенный .github внутри vendor удалён (GitHub не запускает
вложенные workflows).

Entry point: `cmd/vps-admin/main.go`. Runs with `-config <path>` (default
`/etc/vps-admin/config.yaml`) and `-test` (one-shot collect, no bot/web).

## Architecture

```
cmd/vps-admin        entry: logging → store → registry → manager → bot + web + scheduler
internal/config      YAML + flags; SetDefaults(); IsAdmin()
internal/store       SQLite: users, identities, stats_cache, traffic_deltas
internal/service     interfaces: Collector (GetTraffic) + Provisioner (Provision/Deprovision)
internal/services/*  xray, awg3xui, telemt — each impl Collector (+ Provisioner)
internal/registry    builds enabled services; exposes Collectors() + Provisioners()
internal/manager     Register/Delete a user across ALL services (used by bot AND web)
internal/bot         Telegram: reports + /register, /device, /deldevice, /regen, /regenall, /deluser
internal/nginxmap    регенерация SNI-карты nginx stream (shvpn-sni-map.conf)
internal/components  версии компонентов стека + async-обновления с живым логом
internal/version     версия бинаря (ldflags: CI — тег, Makefile — git describe)
internal/web         HTTP API + embedded SPA (go:embed static/index.html); login+password (фикс. логин web.login)
internal/scheduler   cron: collect every 5m, link refresh hourly, cleanup, vacuum, weekly digest
internal/report      formats summaries; Row type + HumanBytes
internal/metrics     Prometheus
internal/sysinfo     ресурсы сервера (CPU/RAM/диск/uptime): linux → /proc, иначе stub
internal/alerts      алерты в Telegram с дедупом (шлём только на смену состояния)
internal/util        UUIDv4, RandHex
```

Shared logic lives in `manager` — never duplicate provisioning in bot/web.
`Regenerate(user, svc)` / `RegenerateAll(svc)` перевыпускают конфиги через
**Deprovision + Provision** (не через нативный `regen` инсталлятора) — переиспользуют
уже оттестированные пути и работают одинаково для всех провижинеров. Идентификатор
в сервисе (= username) при этом не меняется → `traffic_deltas` (ключ service+identity)
остаются валидны, история трафика сохраняется, обновляется лишь `config` (INSERT OR
REPLACE по той же строке identities). Порядок строго Deprovision→Provision: и xray
(дубль email), и amneziawg (клиент «уже существует») не дают создать поверх живой.

## Data model (the important part)

- `users` — canonical user (username, display_name). **Создание — только по
  отображаемому имени**: `manager.RegisterByName` генерирует username транслитом
  (`util.UsernameFromDisplay`: кириллица→латиница, пробелы→'-', только
  [a-z0-9_.-], ≤36 символов), при коллизии добавляет -2/-3/…; веб-форма и бот
  `/register <имя>` шлют только display_name (legacy-API с явным username
  сохранён). Переименование меняет **только
  `display_name`** (`store.SetDisplayName`, `PATCH /api/users/{username}`, бот
  `/rename`): `username` — это identity во всех сервисах и ключ `traffic_deltas`,
  его смена потребовала бы перевыпуска конфигов и отвязала бы историю трафика.
  Пустое имя трактуется как «подписывать логином».
- `identities` — `(service, identity)` → user_id, plus `address` and `config`.
  identity = xray email (=username) / awg email (=username+"-awg", отдельная
  учётка) / telemt username (=username). **`config` stores a
  JSON array of `service.Item{label,value,qr,file}`** (connection variants) — e.g.
  xray: vless link + subscription; awg: `vpn://` (Amnezia VPN) + `.conf` (AmneziaWG);
  telemt: secret + dd/ee links. Provisioners return `Account.Items`; manager marshals
  to JSON. Web/bot render each item (icon copy, QR via `GET /api/qr?text=` using
  `go-qrcode`, share/download for items with a `file`). Old plain-text/empty configs
  are handled as a fallback (e.g. identities from older DBs). Card service order:
  xray, awg, telemt.
- **Признак активности** считается из `traffic_deltas`, отдельного поля в БД нет:
  `LastSeenByUser()` (главная, суммарно по пользователю) и `LastSeenForUser(id)`
  (карточка, по каждому сервису) берут `MAX(timestamp)` **только по строкам с
  `total_bytes > 0`** — нулевые дельты пишутся каждый сбор просто по факту опроса
  и активностью не являются. В API это `last_seen` (список и карточка) + `activity`
  (сервис→время) — везде **RFC3339 в UTC** через `rfc3339UTC()`, иначе браузер
  разберёт `'YYYY-MM-DD HH:MM:SS'` как локальное время и «был N назад» уедет на
  часовой пояс. Порог «активен сейчас» — 11 минут (два сбора по 5 мин с запасом):
  `ONLINE_MIN` в `index.html` и `ago()` в боте. Активность считается за всё время
  и не зависит от выбранного периода. В карточке строки таблицы — объединение
  сервисов с трафиком и сервисов из identities, чтобы простаивающий сервис тоже
  показывал статус.
- Traffic is stored as **deltas** in `traffic_deltas` with counter-reset
  protection (a drop > threshold% is treated as a reset, not negative traffic).
- Aggregation joins `traffic_deltas.identity → identities → users`, so unregistered
  identities show raw with `(не привязан)`. Per-user total = sum across services.
- Period filters are constant SQL exprs on `timestamp` (e.g.
  `datetime('now','start of day')`), passed to `store.TrafficByServiceIdentity` /
  `TrafficForUser` / `TrafficByUser`.

## How provisioning works per service (see the table in README too)

- **xray (3x-ui) — v3 API** (repo restructured to `internal/`; client ops moved
  from `/panel/api/inbounds/*` to `/panel/api/clients/*`). Auth is **API-token only**
  (`api_token`, sent as `Authorization: Bearer` via `uaTransport`; no login).
  Create: `flow` from inbound → gen UUID+subId → `POST /panel/api/clients/add` with
  body `{client:{...}, inboundIds:[id]}` (NOT the old `{id, settings:"<json string>"}`;
  `tgId` is a number). Delete: `POST /panel/api/clients/del/:email` (by email, no UUID
  lookup). Links: `GET /panel/api/clients/links/:email` → `{obj:[...]}`. **3x-ui builds
  the link's server address from the request Host header** (`getClientLinks` →
  `resolveHost(c)` = `c.Request.Host`), so calling the API over localhost yields
  `vless://…@localhost:…`. Fix: we do NOT override the request Host (that breaks
  routing behind nginx → non-200 → links vanish); instead `clientLinks` rewrites
  localhost/127.0.0.1/0.0.0.0/::1 in the returned links via `rewriteLinkHost` to
  `publicHost()` (from `public_host`, else the host of `sub_url`) — host only, the
  port stays the real inbound port. **`rewriteLinkHost` must patch the host bytes
  in place — never `url.Parse`+`String()`**: that re-encodes `#remark` (Cyrillic/
  spaces → `%D0%98%20…`) and normalizes the link, so clients show garbage names and
  the QR encodes a different link than the panel issued (regression test:
  `TestRewriteLinkHostPreservesEverythingElse`). **Hosts** (3x-ui v3 `/panel/api/hosts`,
  e.g. «раздавать inbound с :443» за haproxy SNI-роутером) 3x-ui применяет сама — и в
  `/clients/links`, и в подписке; мы их не читаем. Но ссылки в `identities.config`
  сохраняются при создании, поэтому смена хоста/порта в 3x-ui требует их **обновления
  без пересоздания клиента**: `service.LinkRefresher` → xray `RefreshItems(email)`
  (`GET /panel/api/clients/get/:email` → `obj.client.{uuid,subId}` + свежие `/links`) →
  `manager.RefreshLinks()` (пишет только изменившееся через `store.SetIdentityConfig`,
  при ошибке сохранённое не трогает) — scheduler при старте и раз в час (`17 * * * *`).
  НЕ `RegenerateAll`: он меняет UUID/subId и ломает тех, кто на подписке. Пустой ответ
  `/links` — ошибка, а не «нет ссылок», иначе рабочие ссылки затёрлись бы одной
  подпиской. Traffic still from `GET /panel/api/inbounds/list`
  clientStats.
  `settings`/`streamSettings` in list can be string OR object → `jsonBytes`. Browser
  User-Agent dodges WAF 403s.
- **amneziawg** (`internal/services/awg3xui`): AmneziaWG-инбаунд в панели 3x-ui
  (userspace netstack или kernel mode) — единственный бэкенд; нативный installer
  bivlked удалён (история: раньше управление шло через `manage_amneziawg.sh --json`,
  ещё раньше — Docker-контейнер Amnezia; оба пути заброшены). Имя сервиса
  каноническое `amneziawg`. **Клиент — ОТДЕЛЬНАЯ учётка `<username>-awg`**
  (не attach общей с xray!): в 3x-ui трафик хранится одной строкой на email
  (`client_traffics`, `UPDATE … SET up=up+? WHERE email=?`), и общая учётка на
  двух инбаундах давала одинаковые clientStats в обоих → задвоение трафика в
  обоих коллекторах. Разные email → разные строки → честный per-service учёт.
  Identity в vps-admin = `<username>-awg` (канонический юзер тот же; модель
  identities это прямо поддерживает). Provision: `POST /panel/api/clients/add`
  с минимальным телом `{client:{email:<username>-awg,enable,subId,...},
  inboundIds:[id]}` — ключи/IP/обфускацию генерит сама панель
  (`defaultAmneziaWGClients`). Повторный вызов идемпотентен: если email уже есть
  и состоит в нашем инбаунде (проверка по settings из inbounds/list) — возвращаем
  существующие конфиги; если занят чужой учёткой — ошибка. Конфиги:
  `GET /panel/api/clients/links/:email` → для AmneziaWG это
  **`vpn://<base64url ТЕКСТА .conf>`** (схема приложения Amnezia VPN).
  Декодируем → `patchEndpointHost` (меняем ТОЛЬКО хост в строке `Endpoint = `,
  порт и остальные байты не трогаем; панель подставляет localhost из
  Host-заголовка, поэтому нужен `public_host`) → Items: «vpn:// (Amnezia VPN)»
  (файл `<user>-awg.vpnuri`) + «Конфиг (AmneziaWG)» (файл `<user>-awg-amneziawg.conf`).
  Address = allowedIPs[0] без маски (`clients/get/:email`). Deprovision:
  `POST /panel/api/clients/del/:email` — удаляет AWG-учётку целиком (она отдельная,
  xray-клиента не трогает); «not found» → успех. Regen (del+add) **ротирует
  ключи** полностью. Подписка AWG-клиента своя (subId отдельный); Items отдают
  готовые vpn://+.conf, подписка для AWG не нужна.
  Traffic: `inbounds/list` clientStats **только inbound_id из конфига**.
  RefreshItems реализован (смена public_host/обфускации без пересоздания).
  Config: `amneziawg_3xui` (base_url, web_base_path, api_token, inbound_id,
  public_host) — server-derived значения (ключи, обфускация, порт) не
  дублируются, читаются из API. Поколения: автогенерация панели всегда 3.1;
  2.0 задаётся вручную в форме инбаунда (пустой HeaderProtectionKey = feature
  off, H1–H4 любые непересекающиеся; валидация панели это принимает).
  **Важно**: xray-коллектор пропускает инбаунды protocol=amneziawg/wireguard
  (иначе трафик AWG-клиентов задваивался бы в xray+amneziawg на одного юзера).
  Kernel mode требует модуля (`x-ui install-awg-kernel`); на Ubuntu 26.04
  (ядро 7.0) pinned tag модуля не собирается без upstream-патча 70ae2481
  (backport API setup_udp_tunnel_sock) — см. patches/amneziawg-ubuntu70.md (репо self-hosted-vpn).
  **Multi-device**: один WireGuard-ключ нельзя использовать параллельно с двух
  устройств (roaming + один туннельный IP) → на каждое устройство отдельный пир
  `<username>-awg-<slug>` (slug = транслит имени устройства). `manager.AddDevice /
  RemoveDevice` (опциональный интерфейс `DeviceProvisioner` у адаптера,
  `ProvisionDevice`); несколько identities одного сервиса на юзера — штатно,
  трафик агрегируется по user_id. AddDevice идемпотентен (повтор возвращает
  сохранённый конфиг). RemoveDevice: del в панели + DeleteIdentity +
  DeleteIdentityTraffic (история устройства стирается, чтобы не повисла
  «не привязанной»). GetIdentities: ORDER BY service, length(identity) — основная
  учётка (короче) всегда первой, Regenerate перевыпускает именно её, не устройство.
  Удаление юзера каскадно сносит устройства (FK ON DELETE CASCADE + Delete
  обходит все identities). Бот: `/device <имя> <устройство>`, `/deldevice`;
  веб: `POST/DELETE /api/users/{username}/devices[/{device}]`, в карточке юзера
  поле «＋ Устройство AWG» + 📱-блоки с удалением. MTProto и VLESS multi-device
  не требуют (один секрет/UUID работает параллельно на многих устройствах).
- **«Подключения» (антиблокировка)** — `manager/connections.go`, API
  `GET /api/connections`, `POST /api/connections/{reality,telemt,backup}`, UI-карточка.
  Ротация Reality-SNI/dest/uTLS-fingerprint основного инбаунда (пресеты
  Google/Microsoft/Apple/Amazon/Samsung + свой список); смена домена-маски
  telemt (правка tls_domain/mask_host в config.toml с сохранением владельца/прав
  и [access.users] + restart); запасной транспорт XHTTP/gRPC — отдельный инбаунд
  `shvpn-backup-<transport>` на localhost backup_port (10092) со СВОИМ набором
  SNI, все xray-клиенты attach'атся (трафик — одна строка на email, коллектор
  читает основной инбаунд → задвоения нет). Перед применением: валидация
  коллизий SNI (telemt-домен ∩ reality ∩ backup = ∅ — nginx маршрутизирует по
  SNI), probe цели (Reality: TLS 1.3 + ALPN h2; маска: валидный HTTPS-сертификат).
  После каждого изменения: `nginxmap.Apply` (атомарная запись + nginx -t + reload,
  откат при ошибке) + `RefreshLinks` (ссылки клиентов обновляются; VLESS-подписка
  обновляется сама, telemt/AWG — раздать новые). xray-адаптер: Reality/
  UpdateReality/XrayInbounds/EnsureBackup/FindBackup/SetEnabled/Attach/Detach
  (inbounds/update принимает инбаунд ЦЕЛИКОМ, settings/streamSettings — строки;
  панель перезапускает xray-core). telemt: Domain/SetDomain/RefreshItems
  (секрет для RefreshItems читается из [access.users] toml). Тестовые хуки:
  nginxMapPath/nginxReload/probeReality/probeMask (manager), restartTelemt (telemt).
- **«Компоненты» (Система)** — `internal/components`, API `GET /api/components`,
  `POST /api/components/{name}/update`. Версии: vps-admin (internal/version,
  ldflags), 3x-ui (getPanelUpdateInfo: current/latest/updateAvailable), xray
  (бинарь -version + getXrayVersion), telemt (--version + GitHub tag), awg
  (tools + modinfo, НЕ обновляется из панели — DKMS-флоу установщика), nginx
  (nginx -v). Обновления async (job + лог в буфере, UI опрашивает и показывает
  окно прогресса): vps-admin — self-update из Releases своего репо (sha256,
  атомарная замена, отложенный restart через 2с — ответ успевает уйти; после
  restart job-состояние теряется, UI судит по смене версии), 3x-ui — POST
  /server/updatePanel (официальный, данные сохраняются; обрыв соединения при
  restart панели — ожидаем, поллинг возврата), xray — POST
  /server/installXray/<latest> (официальный механизм панели), telemt — бинарь
  из GitHub Releases (tar.gz + .sha256, конфиг/юзеры не трогаются), nginx —
  apt --only-upgrade. GitHub-теги кэшируются 10 мин (rate limit 60/ч).
  Конфиг: `update.github_repo`/`update.github_token` (приватный репо).
  Тестовый хук: Runner.cmd (поле, не глобал — иначе гонка с goroutine job'а).
  Паника в горутине job'а перехватывается recover: ошибка обновления не должна
  ронять процесс (бот/панель/сбор живут в нём же).
- **Откаты провижининга**: если `AddIdentity` падает после успешного Provision —
  учётка в сервисе удаляется (иначе сирота: трафик «не привязан», логин занят);
  в `Regenerate` при сбое пересоздания старая учётка восстанавливается.
- **Трафик без задвоения**: `cron.SkipIfStillRunning` + `Store.deltaMu` +
  транзакция (дельта и baseline пишутся вместе).
- **telemt** (github.com/telemt/telemt, Rust): control-API. `POST /v1/users`
  creates the user AND writes it into telemt's `config.toml`; telemt returns the
  ready `dd`/`ee` tg:// links (`links.secure` / `links.tls`) built from its own
  `tls_domain`/`public_host`. So config here is only `base_url` + `admin_key`.
  Stats+links: `GET /v1/stats/users` (envelope `{ok,data:[…]}`, `total_octets` + `links`;
  parser is shape-tolerant). Links fetched from here if the create response is empty.
  Auth header is
  the raw token (no `Bearer`).

## Conventions / gotchas

- **Config holds no server-derived values.** If a value already exists on the
  server (obfuscation params, server pubkey, telemt tls_domain, xray flow), read
  it at runtime — do not add a config field for it. This was an explicit user
  requirement; keep honoring it.
- Comments and all user-facing strings (bot, web UI) are in **Russian**. Match that.
- Web UI is a single self-contained `internal/web/static/index.html` — **no
  external CDNs** (VPS may be firewalled). Inline CSS/JS only.
- Web UI is **responsive** (works on desktop + mobile): `<meta viewport>`; on ≤640px
  the header controls (периоды/тема/выход) collapse into a `☰` dropdown (`#burger`
  toggles `#hdrtools.open`) so they don't sprawl; the overview grid collapses to one
  column at ≤820px; a
  `@media(max-width:640px)` block stacks the create-form, shrinks paddings and the
  modal, and caps QR at `70vw`; `@media(pointer:coarse)` enlarges icon tap targets.
  Long values (vless/tg links) are single-line ellipsis and tables/`pre` live in
  `overflow-x:auto` wrappers, so nothing forces horizontal page scroll.
- **Публичная ссылка на ключи**: таблица `share_links(user_id PK, token)` — одна на
  пользователя, бессрочная, каскадно удаляется с пользователем. Токен = `RandHex(16)`
  (32 hex). Админ-эндпоинты `POST|DELETE /api/users/{username}/share`; публичные
  `GET /p/{token}` (отдаёт `static/public.html`) и `GET /p/{token}/data` (JSON:
  display_name + identities + трафик ЭТОГО пользователя, без id/чужих данных).
  Публичные пути пропускает `isPublicPath` в `auth` (туда же `/api/qr` — чистая
  функция text→PNG). Ответы идут с `no-store` + `X-Robots-Tag: noindex`.
- Вход **защищён от перебора**: `loginLimiter` (`internal/web/loginlimit.go`) —
  5 неудач с одного IP → блокировка на 15 минут (429 + `Retry-After`), удачный вход
  сбрасывает счётчик. IP берём из `RemoteAddr`, `X-Forwarded-For` НЕ доверяем
  (подделанный заголовок обошёл бы лимит; панель слушает напрямую).
- **Алерты**: `scheduler` шлёт в Telegram через `internal/alerts` — сервис не отдаёт
  статистику N сборов подряд (`collectFailsToAlert`=3, ~15 мин), диск >90%, TLS-серт
  истекает <48ч (актуально для LE-профиля `shortlived` на IP — он живёт ~6 дней).
  `Alerter` шлёт только при СМЕНЕ состояния: одно «сломалось» и одно «починилось».
- **Еженедельная сводка**: `scheduler` вешает cron `telegram.weekly_cron`
  (по умолчанию `0 10 * * 1` — пн 10:00, **локальное время сервера**: robfig/cron
  без `cron.WithLocation` берёт `time.Local`, на VPS это MSK) на
  `bot.SendWeeklyDigest` → `report.WeeklyDigest`. Окно **скользящее** (`-7 days`,
  предыдущее `-14..-7` для «+18% к прошлой неделе»), а не календарная неделя:
  рассылка по расписанию, поэтому «последние 7 дней» = ровно промежуток с прошлого
  письма. Ошибку разбора cron НЕ глотаем (в отличие от остальных `AddFunc` —
  там расписания константные, а это ввод админа). Выключается
  `telegram.weekly_report: false`; `WeeklyReportEnabled()` трактует отсутствие поля
  (`*bool == nil`) как «включено». Ручной вызов — команда `/digest`.
  `report.truncate` режет **по рунам** — имена русские, обрезка по байтам рвала бы
  кириллицу пополам.
- **PWA**: `manifest.webmanifest` + `sw.js` + `icon-{192,512}.png` в `static/`.
  Манифест/иконки/sw обязаны быть в `isPublicPath`: браузер тянет манифест БЕЗ
  cookie, за авторизацией он получил бы HTML формы входа и установка не
  предлагалась бы. `mime.AddExtensionType(".webmanifest", …)` в `init()` — Go не
  знает это расширение, а без верного MIME манифест не примут.
- **Rate-limit входа — по реальному IP**: за nginx `RemoteAddr` всегда 127.0.0.1,
  поэтому `clientIP` берёт `X-Real-IP` **только от loopback** (ставит наш nginx;
  прямой клиент подделать заголовок не может). Иначе 5 неверных паролей от кого
  угодно блокировали бы вход всем.
- **Prometheus слушает 127.0.0.1** (`prometheus.listen`): `/metrics` без
  аутентификации, наружу выставлять нельзя.
- Web auth: **фиксированный логин (`web.login`, дефолт `admi`) + пароль**. Форма
  входа — нативный `<form method=post action=/api/login>` с полями
  `autocomplete="username"` (readonly, подставляется из конфига) и
  `autocomplete="current-password"`: менеджеры паролей (Safari) сохраняют пароль
  только на реальной отправке формы. Успех → session-cookie + 303 на путь
  возврата: скрытое поле `next` (браузер подставляет `location.pathname`, т.к.
  nginx срезает секретный префикс и бэкенд его не знает) → иначе путь из
  Referer → иначе `/`. Только локальные пути и не `/api/*` (иначе открытый
  редирект / цикл). Ошибка/лимит → та же форма с текстом и сохранённым `next`;
  именно из-за потери `next` вход после неудачной попытки приводил на
  `/api/login` → 404. `GET /api/login` без cookie отдаёт форму, не 401-JSON.
  JSON-вход (`{username?,password}`)
  сохранён для API/тестов (username опционален, но если прислан — проверяется).
  Session token derived from the password via sha256, so it survives restarts/deploys. On a
  non-loopback `listen`, an empty password disables the panel.
- Web **HTTPS**: `web.tls.enabled` → serves TLS. `cert_file`+`key_file` for a real
  cert — served via `certReloader` (`tls.Config.GetCertificate` + mtime check), because
  Let's Encrypt **IP certs are `shortlived` (~6 days)** and acme.sh's cron rewrites them
  under a running service; `ListenAndServeTLS(c,k)` would keep serving the expired one.
  On a broken/half-written file the reloader keeps the last good cert. No domain is
  needed for a valid cert — see the acme.sh IP recipe in README (this is what 3x-ui's
  `x-ui.sh` → `ssl_cert_issue_for_ip` does). Else a self-signed ECDSA cert is used. The self-signed cert is **cached to
  disk** next to the DB (`<dir(db_path)>/web-selfsigned.{crt,key}`) via
  `loadOrCreateSelfSigned()` — regenerated only when missing, expiring (<30d), or
  when the SAN host set changes (`certCoversHosts`), so it stays stable across
  restarts/deploys (no repeated browser warnings). `generateSelfSigned()` mints it;
  `selfSignedCert()` is the cache-less wrapper used in tests. Cookie gets `Secure`
  when `r.TLS != nil`. telemt's 443 is MTProto (not HTTP) — a single 443 gate needs
  an SNI/TCP router (nginx stream), not an HTTP proxy in-panel.
- Список пользователей (`GET /api/users?period=`) имеет **свой** период
  (`usersPeriod` в `index.html`, селектор `#usersPeriods`), независимый от периода в
  шапке; без параметра — прежнее поведение (всё время). Раньше итог был жёстко
  `TrafficByUser("1=1")`.
- **Графики трафика** (`GET /api/charts?period=&tz=`): один гибкий store-метод
  `TrafficSeries(bucketExpr, keyExpr, where)` (все три — константный SQL из кода,
  не ввод) агрегирует по времени и произвольному ключу серии. `internal/web/charts.go`
  строит разрезы: общий ряд, по сервисам, по пользователям (`handleCharts`), а
  `handleUserCharts` (`GET /api/users/{username}/charts`) — суммарный ряд и разбивку
  по сервисам ОДНОГО пользователя (доп.условие `i.user_id = <id>` через хелперы
  `series`/`overallSeries`). **Графики в местном времени зрителя**: браузер шлёт `tz`
  (минуты к востоку от UTC, `-getTimezoneOffset()`), `bucketsFor(period, tzMin)` режет
  корзины по локальным «настенным часам» (сегодня — по часам, иначе по дням) и сдвигает
  UTC→local прямо в `strftime` модификатором `'+N minutes'`; порог `where` считается в
  Go и переводится обратно в UTC (`whereFromLocal`). Хранение остаётся в UTC; периоды
  панели (Σ-хедер, карточки) — по-прежнему UTC, поэтому у границы суток итог графика и
  Σ-хедер могут слегка разойтись. `fillSeries` заполняет нулями пропуски, `topSeries`
  оставляет топ-8 (хвост → «Прочие»). Рисуется инлайн-SVG в `index.html` (`drawStacked`
  — стек площадей; общий hover-тултип `attachTip`), **без внешних библиотек**. Главная:
  вкладки Общий/Сервисы/Пользователи. Карточка пользователя: те же два графика по нему
  со своим селектором периода `#uChartPeriods` (`userChartPeriod`, наследует период
  шапки при открытии). `drawStacked` сам чистит контейнер — без этого при перерисовке
  графики дублировались бы. Только в админке — на публичной странице графиков нет.
- **Легенда кликабельна (как в Grafana)**: клик по серии оставляет на графике только
  её (шкала Y пересчитывается), повторный клик по изолированной — возвращает все;
  Ctrl/⌘/Shift+клик прячет/показывает одну; при скрытых сериях в легенде появляется
  «↺ показать все». Скрытые серии лежат в `chartHidden[key]` (`hiddenOf`), а НЕ в
  DOM: панель перерисовывает графики каждые 15с, и без внешнего состояния выбор
  сбрасывался бы. `key` — `"main:"+chartView` для главной (у «Сервисов» и
  «Пользователей» свои наборы) и id контейнера для карточки. Легенда неактивна,
  когда серия одна (разрез «Общий»).
- Панель **сама обновляется** каждые 15с (`REFRESH_MS`): ресурсы сервера
  (`GET /api/system`), графики (`GET /api/charts`), трафик и список пользователей. Опрос пропускается при
  `document.hidden` и возобновляется на `visibilitychange`; ошибки глушатся (иначе
  при обрыве сети сыплются тосты).
- Web UI **theme**: light/dark/system toggle in the header (☀️/🌙/🖥️), preference in
  `localStorage["vps-theme"]`. Colors are CSS vars on `:root` (dark) /
  `:root[data-theme="light"]`; an early inline `<head>` script sets `data-theme`
  before paint (no flash). Code blocks use `--code-bg`/`--code-fg` so they flip too.
- Module path: `github.com/LankoRu/vps-admin`. Go 1.26, `modernc.org/sqlite`
  (pure-Go, no cgo).

## Testing the web UI visually

There's no long-running dev server (main needs a real bot token). To eyeball the
UI: create a throwaway `cmd/webpreview/main.go` (`//go:build ignore`) that seeds a
temp store and calls `web.New(...).Start()` on `127.0.0.1:8099`, add a
`.claude/launch.json` for it, use the Claude Preview tools, then **delete both**
(the `ignore` tag would otherwise break `go build ./...`). Prefer `httptest`-based
tests in `internal/web/web_test.go` for logic.

## Integration caveats to remember

- telemt `POST /v1/users` needs its config **directory** writable-mounted (not a
  single bind-mounted `config.toml`) — atomic tmp+rename otherwise fails.
- AmneziaWG `interface`/`config_path` defaults assume Amnezia's Docker image
  (`awg0`, `/opt/amnezia/awg/wg0.conf`) — verify per deployment.

## Upstream source map (for debugging these integrations fast)

When an integration breaks, the answer is almost always in upstream source, not
guesswork (we got burned guessing telemt/3x-ui APIs). How to read it fast:

- **Fetch via `curl`, not WebFetch** (WebFetch 404s on many GitHub URLs; `gh` was
  unauthenticated here). List a repo tree, then pull raw files:
  - Tree: `curl -s "https://api.github.com/repos/OWNER/REPO/git/trees/BRANCH?recursive=1"`
    then filter paths in python (`json.load(sys.stdin)['tree']`).
  - Raw file: `curl -s "https://raw.githubusercontent.com/OWNER/REPO/BRANCH/PATH"`.
  - Repo root listing: `curl -s "https://api.github.com/repos/OWNER/REPO/contents/"`.

### 3x-ui — `github.com/MHSanaei/3x-ui` (branch `main`)
v3 was **restructured into `internal/`** and client ops moved out of the inbounds
group. Key files:
- `internal/web/controller/api.go` — route groups under `/panel/api`: `checkAPIAuth`
  (token→Bearer, sets `api_authed`), CSRF middleware, and the groups
  `/inbounds`, `/clients`, `/server`, `/nodes`, `/hosts`, `/setting`, `/xray`.
- `internal/web/controller/inbound.go` `initRouter` — inbound routes: `GET /list`,
  `POST /add`, `POST /del/:id`, etc. **No client ops here anymore** (that was the 404).
- `internal/web/controller/client.go` `initRouter` — the client API:
  `POST /panel/api/clients/add`, `POST /del/:email`, `POST /update/:email`,
  `GET /list`, `GET /get/:email`, `GET /traffic/:email`, `GET /links/:email`.
- `internal/web/service/client.go` — `ClientCreatePayload{client model.Client, inboundIds []int}`
  (the add body). `internal/database/model/model.go` (~L798) — `model.Client`
  (note `tgId` is int64, `subId`/`email`/`flow` json tags).
- Gotcha: inbounds/list `settings`/`streamSettings` can be a JSON string OR object.

### Amnezia client — как конфиг попадает В ПРИЛОЖЕНИЕ (repo `amnezia-vpn/amnezia-client`)
`client/android/AndroidManifest.xml` — единственный авторитетный источник:
- `ACTION_VIEW` + `BROWSABLE`, `<data android:scheme="vpn" android:host="*"/>` —
  ссылка `vpn://…` открывает приложение. **Но WebView (встроенный браузер Telegram)
  кастомные схемы не запускает** — он пытается загрузить `vpn://` как URL и молча
  ничего не делает; нужен обычный браузер.
- `ACTION_SEND` с mime `application/octet-stream` / `text/plain` — **так `.conf`
  и попадает в приложение: через системный «Поделиться»** (Web Share API с File),
  а не через скачивание (скачанный файл просто ложится в «Загрузки»). Поэтому на
  публичной странице кнопка «Открыть в приложении» = `navigator.share({files})`,
  а загрузка с `/p/{token}/file` — только фолбэк.
- `ACTION_VIEW` на `file://`/`content://` c `pathPattern` `.*\.conf` и mime `*/*` —
  открытие уже скачанного файла из файлового менеджера (расширение важнее mime).
- Формат `vpn://` (3x-ui): `vpn://` + base64 **url-safe без `=`-паддинга** от
  **plain-текста .conf** (схема самого AmneziaVPN app; `importController.cpp`
  читает как `Base64UrlEncoding|OmitTrailingEquals`). awg3xui работает с этим
  форматом (base64.RawURLEncoding); исторический qCompress-формат installer'а
  bivlked (`pack("N", len) . compress(json)`) больше не используется.

### AmneziaWG tools (HISTORICAL — the old Docker approach, why we dropped it)
- `github.com/amnezia-vpn/amneziawg-tools` (branch `master`):
- `src/config.c` — config **parser**; I1..I5 listed in `awg_special_handshake_keys`
  (~L26), parsed ~L554. Confirms the full v2 param set lives in the `.conf`.
- `src/showconf.c` — config **dumper**; prints `Jc/S1/H1/I1..` but each guarded by
  `device->flags & WGDEVICE_HAS_*`. The module doesn't round-trip I1..I5 → showconf
  omits them → **never rely on showconf for I-params; read the `.conf`**.
- `src/wg-quick/linux.bash` `save_config()` — rebuilds `[Interface]` from
  `awg showconf`, so **`awg-quick save` strips I1..I5**. We append `[Peer]` to the
  `.conf` ourselves instead.

**Where Amnezia keeps the obfuscation params** (repo `amnezia-vpn/amnezia-client`,
branch `dev`) — this is the authoritative source, NOT `awg show`/`showconf`:
- `client/server_scripts/awg/configure_container.sh` — writes the server config to
  **`/opt/amnezia/awg/awg0.conf` INSIDE the container** (not mounted to host;
  `run_container.sh` only mounts `/lib/modules`). Jc..H4/S1..S4 are uncommented
  (applied); **I1..I5 are written COMMENTED** (`# I1 = ...`) — the server never
  applies them, but the value is preserved there so client configs can reuse it.
- `client/core/installers/awgInstaller.cpp` `generateAwgParameters()` — how values
  are made at install: Jc random 4–6; Jmin/Jmax = 10/50; S1..S4 random (unique);
  H1..H4 for AWG 2.0 are **ranges `A-B`**; **I1..I5 are hardcoded constants**
  (`protocolConstants.h` `defaultSpecialJunk1` = a fake iCloud DNS packet, I2..I5 empty).
- `client/core/configurators/awgConfigurator.cpp` — the client config generator just
  **copies specialJunk1..5 verbatim** from the stored config; no per-client generation.
- Paths + defaults live in `client/core/utils/constants/protocolConstants.h`
  (`namespace awg`: `serverConfigPath = /opt/amnezia/awg/awg0.conf`, defaults, `<r N>`/`<b 0x..>` tags).
- **Our takeaway**: `fillInterface` reads the server `.conf`, uncomments `# I1..I5`
  (skipping empty ones), and copies all obf params verbatim → client config matches
  Amnezia byte-for-byte. I1's `<r 2><b 0x..>` are awg packet-generator tags — pass through.

### telemt — `github.com/telemt/telemt` (branch `main`)
- `config.toml` — `[access.users]` (username→32-hex secret), `[general.modes]`
  (classic/secure/tls), `[general.links]` (public_host/public_port),
  `[censorship].tls_domain`, `[server.api]` (control-API listen/auth).
- `docs/Architecture/API/API.md` — endpoints: `POST /v1/users` (writes config.toml,
  returns `{user, secret}`), `DELETE /v1/users/{u}`, `GET /v1/users` &
  `GET /v1/stats/users` (envelope `{ok,data:[UserInfo]}`; `UserInfo.links` =
  `{classic,secure,tls,tls_domains}`, secure=dd, tls=ee). Auth header = raw token.

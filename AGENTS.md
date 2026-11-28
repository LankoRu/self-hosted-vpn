# AGENTS.md — self-hosted-vpn

Инструкции для агентов по этому репозиторию. Репозиторий — **инсталлятор и runbook**
связки: nginx SNI-роутер + telemt (MTProto) + 3x-ui (Xray Reality + AmneziaWG) +
vps-admin (агрегатор). Сам vps-admin вендорится в `vendor/vps-admin` — **это
канонический каталог его исходников**; оригинальный `~/workspace/vps-admin`
больше не редактировать.

## Что где

- `install.sh` — установщик (curl|bash). Все настройки — **флаги запуска**
  (`--help`), `--yes` + флаги для неинтерактива; сохранённые значения
  (/etc/shvpn/env.sh) — дефолты при повторном запуске.
  → `scripts/setup-base.sh` (nginx/telemt/3x-ui/сайт/сертификат/ufw) →
  `scripts/setup-awg.sh` (AWG-инбаунд) → `scripts/setup-vps-admin.sh`.
  Модули принимают env — внутренний интерфейс (install.sh экспортирует).
- `.github/workflows/ci.yml` — CI (gofmt/vet/test -race в vendor) + auto-release:
  push в main → авто-тег v0.1.x → Release с prebuilt-бинарями vps-admin
  (linux amd64/arm64, CGO_ENABLED=0, sha256sums.txt). Установщик качает бинарь
  оттуда — Go на сервере не нужен. Приватный репо: --github-token (Contents: Read).
- `scripts/lib.sh` — общие функции (env, рендер шаблонов `{{KEY}}`, acme-хуки).
- `config/*.tpl` — шаблоны конфигов; `config/site/index.html` — сайт-прикрытие.
- `patches/amneziawg-ubuntu70.md` + `scripts/awg-kernel-patch.py` — патч модуля AWG.
- `DEPLOY.md` — runbook (ручные шаги, проверка, траблшутинг, семантика клиентов).
- `vendor/vps-admin` — исходники vps-admin (см. его собственный AGENTS.md!).
- `servers/` — отчёты установок с секретами, **gitignored, не коммитить**.

## Архитектура (порты)

Публично только **22/tcp, 443/tcp, 443/udp** (ufw).
- 443/tcp: nginx **stream** `ssl_preread` + `proxy_protocol on` (PROXY v1) →
  SNI==TG_DOMAIN → telemt 127.0.0.1:8443; SNI из Reality-списка → Xray 127.0.0.1:10090;
  иначе → web 127.0.0.1:8444 (http-блок: сайт, acme, панели по cookie).
- 443/udp: AmneziaWG (инбаунд 3x-ui, kernel mode).
- localhost: 3x-ui панель 40323, подписки 10091, vps-admin 8088, telemt API 9091.
- Панели наружу: секретные пути + cookie-gate в nginx (`/api/login` → Set-Cookie,
  gated-локации без cookie → 404). Сертификат — Let's Encrypt **IP-cert** (acme.sh,
  HTTP-01; порт 80 открывается ТОЛЬКО на renewal через ufw-хуки).

## Грабли (проверено кровью — не наступать снова)

1. **Xray + PROXY**: приём PROXY от nginx — `tcpSettings.acceptProxyProtocol: true`.
   `realitySettings.xver` — это отправка PROXY к target'у Reality, НЕ приём; для
   нашей схемы xver=0. Без acceptProxyProtocol Reality ломается («invalid connection»).
2. **externalProxy в 3x-ui**: для VLESS/Reality обязательно `forceTls: "same"`,
   иначе панель выкидывает pbk/sid/fp/spx из ссылок. Для AWG — обычный `{dest,port}`
   (fixes Endpoint=localhost в конфигах).
3. **telemt `exclusive_mask` НЕ использовать** для смешанного трафика: mask-relay
   режет байты/время и рвёт VLESS. Роутинг делает nginx stream. telemt mask-поля
   должны отсутствовать (иначе telemt сам занимает 443).
4. **telemt API пишет в свой config.toml**: файл/каталог должны быть writable
   пользователю telemt (`telemt:telemt`, dir 770, file 660), иначе provisioning
   падает «Failed to write config file».
5. **AWG только через инбаунд 3x-ui** (протокол amneziawg, `settings:"{}"` →
   автогенерация 3.1). Нативный bivlked/installer НЕ использовать (тянет reboot,
   сносит пакеты). Поколение 2.0 — только ручные параметры в форме инбаунда
   (пустой HeaderProtectionKey = off, H1–H4 любые непересекающиеся).
6. **AWG kernel на Ubuntu 26.04 (ядро 7.0)**: pinned tag не собирается без
   upstream-патча 70ae2481 (см. patches/). `x-ui install-awg-kernel` затирает патч —
   после него прогонять `scripts/awg-kernel-patch.py`. Интерфейс xui-awgN поднимается
   только при ≥1 клиенте в инбаунде.
7. **Трафик 3x-ui — одна строка на email** (`client_traffics`, up/down глобальны по
   email). Поэтому AWG-клиент vps-admin — ОТДЕЛЬНАЯ учётка `<username>-awg`
   (identity в vps-admin = этот email). Общую учётку на двух инбаундах не делать —
   clientStats задвоятся в обоих коллекторах. `clients/get` отдаёт allowedIPs
   строкой (не массивом); settings инбаунда — JSON-строка.
8. **3x-ui API**: Bearer-токен + браузерный User-Agent (дефолтный Go UA режется).
   Токен из install-result.env может не совпадать с рабочим — валидировать запросом.
   Панель/подписки биндить на 127.0.0.1 (панель не имеет 2FA).
9. **Провайдерский SYN-proxy**: `nc -zv` покажет «open» на ЛЮБОМ порту. Проверять
   реальными запросами (curl/ssh), не nc.
10. **IP-сертификат LE**: для bare-IP только профиль `shortlived`
    (`--certificate-profile shortlived`, ~6 дней, автопродление); `--standalone`
    требует свободный :80 — на время выпуска nginx гасится и поднимается обратно.
    Сертификат, созданный установщиком 3x-ui, уже имеет этот профиль — поэтому на
    эталонном сервере наш шаг был пропущен (файл существует).
11. **1 GB RAM**: swap 2G создаётся install.sh для DKMS-сборки модуля AWG.
    vps-admin ставится prebuilt-бинарём из Releases — Go на сервере не нужен.
    telemt — готовый бинарник (Rust), не собирать.
12. **Telegram WEB-прокси** (`--tg-web-domain`, telemt ≥3.5.1): MTProto внутри HTTPS
    к СВОЕМУ домену (DuckDNS/дешёвый домен). Домен — только FQDN, **не IP** (telemt
    отклоняет), A-запись на IP сервера, не совпадает с TG_DOMAIN. WEB-листенер
    127.0.0.1:18080 (plain HTTP, без PROXY), TLS терминирует nginx-vhost
    `shvpn-telemt-web`; SNI домена идёт в `default web` (SNI-карту менять не нужно).
    **`shvpn-web` обязан быть `default_server`** на :8444 — иначе WEB-vhost (грузится
    раньше по алфавиту) отдаст свой серт на запросы по IP и сломает сайт/панель.
    WEB не поддерживает ee — профили `secret_mode="dd"` на каждый access.user;
    vps-admin создаёт профиль ПОСЛЕ юзера и убирает ДО удаления (telemt validate).
    Ссылки `tg://webproxy` vps-admin считает сам (API 3.5.17 их не отдаёт).
    Точечный деплой на существующий сервер — `scripts/deploy-web.sh` (не трогает
    3x-ui/apt); конфиг telemt валидируется `telemt healthcheck` ДО остановки сервиса.

## vps-admin (ключевое, детали в vendor/vps-admin/AGENTS.md)

- Сервисы: `xray_3xui`, `amneziawg_3xui`, `telemt`. Конфиг на сервере
  `/etc/vps-admin/config.yaml` (600). Identity: xray=username, awg=username-awg,
  telemt=username. Канонический юзер в `users`, маппинг в `identities`.
- **«Подключения»** в панели: ротация Reality-SNI/dest/fingerprint, смена
  домена-маски telemt, запасной транспорт XHTTP/gRPC. Владелец nginx SNI-карты
  `/etc/nginx/stream.d/shvpn-sni-map.conf` — vps-admin (перегенерирует из
  состояния 3x-ui + telemt; установщик создаёт её из шаблона).
- **«Компоненты»** (вкладка Система): версии vps-admin/3x-ui/xray/telemt/AWG/nginx
  + кнопки обновления с окном живого прогресса (async job + лог). AWG — только
  версии (обновление модуля через установщик). Версия vps-admin вшивается
  ldflags (CI — тег релиза, Makefile — git describe).
- **Создание — только display_name**: логин генерится транслитом
  (`util.UsernameFromDisplay`), коллизии → -2/-3. Веб-форма и бот `/register <имя>`.
- Установка на VPS: prebuilt из GitHub Releases (`setup-vps-admin.sh`, sha256-проверка,
  `VPSADMIN_VERSION` пинит версию, дефолт latest). Релизы создаёт CI автоматически
  на push в main (ветка должна называться main).
- **Бот один на сервер**: токен `telegram.token` можно опрашивать только из одного
  процесса/хоста, иначе `409 Conflict` рвёт команды на обоих. Для второго сервера —
  новый бот или `--no-bot` (панель работает без Telegram; сбор статистики идёт).
- Деплой для разработки: `cd vendor/vps-admin && make deploy DEPLOY_HOST=root@IP`
  (кросс-сборка linux/amd64 + scp + systemd). Тесты: `go test ./...` (8 пакетов).
- Правки vps-admin делать ТОЛЬКО в vendor; после — деплой и `git add` в self-hosted-vpn.

## Рабочий процесс

- Ветки: `dev` — разработка (CI только тесты), `main` — релизы (auto-release
  на push/merge PR). Коммитить в dev, новые версии сливать в main через PR.
- Секреты генерятся install.sh в `/etc/shvpn/env.sh` и `servers/<IP>-RESULT.txt`
  (gitignore). В репозиторий не коммитить токены/пароли/секреты telemt/chat_id.
- Перед коммитом: `go build ./... && go vet ./... && go test ./...` в vendor;
  `bash -n` для скриптов; grep по секретам (bot token, api tokens, TG_SECRET).
- Полный install.sh на живом сервере гонять осторожно (3x-ui installer может
  потребовать ввод); модули setup-awg/setup-vps-admin идемпотентны и rehearsed.
- Живой эталонный сервер: Ubuntu 26.04 (NL); IP и доступы — в servers/ (не в git).
  SSH по ключу, алиас хоста — в ~/.ssh/config.
- Связь с пользователем — на русском.

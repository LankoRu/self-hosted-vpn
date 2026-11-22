# shvpn — VPN-сервер, у которого наружу открыт только порт 443

Самодостаточный установщик VPN-схемы для VPS (Ubuntu 24.04/26.04, Debian 13):

- **3x-ui + Xray VLESS Reality** — основной VPN;
- **telemt** — MTProto-прокси для Telegram (FakeTLS);
- **AmneziaWG 3.1** — VPN поверх UDP-443 (kernel mode, обфускация с
  шифрованием заголовков);
- **vps-admin** — Telegram-бот и веб-панель: регистрация пользователя сразу
  во всех сервисах (логин из имени автоматически), единый учёт трафика на человека,
  multi-device для AmneziaWG, публичные ссылки с ключами, раздел «Подключения»
  (ротация Reality-SNI, смена домена-маски Telegram, запасной транспорт XHTTP/gRPC
  на случай блокировок), версии и обновление компонентов с окном прогресса;
  итоги установки публикуются в чат админа;
- **nginx SNI-роутер** на единственном публичном TCP-порту 443 + сайт-заглушка
  с валидным Let's Encrypt сертификатом на IP;
- **ufw**: наружу только 22/tcp и 443/tcp+udp. Сканеры и DPI видят обычный
  HTTPS-сайт, а по SNI — настоящие Google/Petrovich.

## Установка

На чистом VPS от root:

```bash
curl -fsSL https://raw.githubusercontent.com/LankoRu/self-hosted-vpn/main/install.sh | bash
```

или из клона репозитория:

```bash
git clone https://github.com/LankoRu/self-hosted-vpn.git && cd self-hosted-vpn
bash install.sh
```

Установщик интерактивный: спросит IP, домен-маску для Telegram, ставить ли
AmneziaWG и vps-admin (для него нужны Telegram bot token и chat_id админа).
Если 3x-ui уже стоит — использует её. Повторный запуск идемпотентен.

Автоматизация (одной командой, без интерактива) — все настройки **флагами**:

```bash
curl -fsSL https://raw.githubusercontent.com/LankoRu/self-hosted-vpn/main/install.sh \
  | bash -s -- --yes --bot-token 123:ABC... --bot-chat 123456789
```

`--yes` берёт все дефолты (IP определяется автоматически, секреты генерируются
на сервере); обязательны только `--bot-token` и `--bot-chat` (или `--no-bot` —
панель со сбором статистики, но без Telegram, или `--no-vpsadmin`).

> **Один bot-токен — один сервер.** Telegram разрешает только одного поллера на
> токен: второй сервер с тем же токеном получит `409 Conflict` и команды бота
> перестанут доходить до обоих. Для каждого VPS заводите отдельного бота
> (@BotFather); для тестового стенда удобно `--no-bot`.
Остальные флаги: `--server-ip`, `--tg-domain`, `--tg-secret`, `--with-awg/--no-awg`,
`--awg-kernel/--awg-userspace`, `--with-vpsadmin/--no-vpsadmin`, `--vpsadmin-version`
(пин версии бинаря, дефолт `latest`), `--repo`, `--branch`. Полный список: `--help`.

**Приватный репо** (форк): нужен `--github-token` (fine-grained PAT, достаточно
**Contents: Read-only** на репо) — и для тарбола установщика, и для бинаря
vps-admin из Releases:

```bash
curl -fsSL -H "Authorization: Bearer $TOKEN" \
  https://raw.githubusercontent.com/OWNER/self-hosted-vpn/main/install.sh \
  | bash -s -- --yes --github-token "$TOKEN" --repo OWNER/self-hosted-vpn \
    --bot-token 123:ABC... --bot-chat 123456789
```

vps-admin ставится **prebuilt-бинарём** из GitHub Releases (Go на сервере не
нужен): CI собирает его автоматически на каждый push в `main` (авто-тег v0.1.x,
ассеты linux/amd64+arm64 + sha256sums.txt). Первый push создаёт первый релиз.

Все секреты генерируются на сервере; итоговый отчёт — `/root/shvpn-result.txt`.

## Разработка

Ветки: **`dev`** — разработка, **`main`** — стабильные версии.

- Push в `dev` → CI прогоняет тесты (gofmt / vet / test -race), релизов нет.
- PR `dev` → `main` → тесты на PR; после merge в `main` CI автоматически
  поднимает patch-версию (v0.1.x), ставит тег и публикует GitHub Release
  с бинарями vps-admin (linux amd64/arm64 + sha256sums).
- Установщик качает бинарь из `releases/latest` (пин версии: `--vpsadmin-version`).

Локально: `cd vendor/vps-admin && go test ./...` (9 пакетов).
Деплой на тестовый сервер: `make deploy DEPLOY_HOST=root@IP`.
Рекомендуется защита `main` (Settings → Branches): запрет прямого push,
обязательный PR + зелёный CI.

## Как это работает

```
Интернет → :443/tcp  nginx stream (ssl_preread, PROXY protocol v1)
             ├─ SNI = домен-маска TG ──► telemt (127.0.0.1:8443) ──► Telegram
             │      └─ невалидный клиент ──► настоящий сайт домена-маски
             ├─ SNI ∈ Reality serverNames ──► Xray VLESS (127.0.0.1:порт)
             │      └─ не-VLESS ──► Reality шлёт на google.com
             └─ без SNI / прочее ──► nginx HTTPS (127.0.0.1:8444, LE-серт на IP)
                    ├─ /                  → сайт-заглушка
                    ├─ <путь панели>/     → 3x-ui
                    ├─ <путь подписок>/   → подписки 3x-ui
                    └─ <секретный путь>/  → vps-admin (+ /api за cookie, /p/ открыт)
         → :443/udp  AmneziaWG 3.1 (инбаунд 3x-ui, kernel mode)
         → :22/tcp   SSH
```

Подробная архитектура, ручные шаги, эксплуатация и грабли: **[DEPLOY.md](DEPLOY.md)**.

## Состав

| Путь | Что |
|---|---|
| `install.sh` | интерактивный установщик (точка входа) |
| `scripts/setup-base.sh` | 3x-ui + telemt + nginx + сертификаты + ufw |
| `scripts/setup-awg.sh` | AmneziaWG: kernel-модуль (+патч Ubuntu 7.0) + инбаунд |
| `scripts/setup-vps-admin.sh` | prebuilt-бинарь vps-admin из Releases + конфиг + nginx |
| `scripts/xui-localhost.py` | перевод 3x-ui на localhost, externalProxy для ссылок |
| `config/` | шаблоны конфигов + сайт-заглушка |
| `vendor/vps-admin/` | исходники vps-admin (github.com/LankoRu/vps-admin) |
| `patches/` | рецепт патча модуля AWG под Ubuntu 26.04 |
| `DEPLOY.md` | полный runbook |

## После установки

- Пользователи: бот `/register <имя>` или форма в панели vps-admin — учётка
  создаётся сразу в VLESS + AWG + Telegram, одна подписка на всё.
- Клиенту: публичная ссылка `/p/<токен>` из карточки пользователя (видит
  только свои ключи и свой трафик).
- Отчёты: `/today`, `/week`, `/digest` в боте; еженедельная сводка сама.
- Сертификат на IP живёт ~6 дней и продлевается автоматически (acme.sh,
  порт 80 открывается на секунды только во время валидации).

## Требования

- VPS: Ubuntu 24.04/26.04 или Debian 13, x86_64/arm64, от 1 ГБ RAM
  (установщик создаст swap для сборки и DKMS), ~5 ГБ диска.
- Домен не нужен: сертификат выпускается на голый IP (Let's Encrypt shortlived).
- Telegram-бот (@BotFather) и chat_id админа — для vps-admin.

## Безопасность

- Секреты генерируются на сервере и в репозиторий не попадают (`servers/`
  с итогами установок исключён из git).
- Панели доступны только по секретным путям за 443; API vps-admin закрыт
  cookie-гейтом (сканеры видят 404), вход с rate-limit.
- Не коммитьте `servers/`, `/etc/vps-admin/config.yaml` и `/root/shvpn-result.txt`.

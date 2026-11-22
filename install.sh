#!/usr/bin/env bash
# ============================================================================
# shvpn — установщик VPN-схемы «наружу только 443».
#
# Ставит на чистый VPS (Ubuntu 24.04/26.04, Debian 13) или дополняет существующий:
#   • 3x-ui (VLESS+Reality) + telemt (Telegram MTProto)
#   • nginx SNI-роутер на :443 + сайт-заглушка + LE-сертификат на IP
#   • AmneziaWG 3.1 (инбаунд 3x-ui, UDP-443, kernel mode с патчем Ubuntu 7.0)
#   • vps-admin (Telegram-бот + веб-панель: регистрация во всех сервисах,
#     единый учёт трафика) — prebuilt-бинарь из GitHub Releases
#   • ufw: только 22/tcp и 443/tcp+udp
#
# Запуск на VPS от root:
#   bash install.sh                     # интерактивно, из склонированного репо
#   curl -fsSL https://raw.githubusercontent.com/LankoRu/self-hosted-vpn/main/install.sh | bash
#
# Все настройки — ФЛАГИ ЗАПУСКА (см. --help), не переменные среды.
# Неинтерактивно: --yes + флаги. Повторный запуск идемпотентен: сохранённые
# значения (/etc/shvpn/env.sh) используются как дефолты, флаги их переопределяют.
# ============================================================================
set -euo pipefail

SHVPN_REPO="LankoRu/self-hosted-vpn"
SHVPN_BRANCH="main"
SHVPN_TOKEN=""
ASSUME_YES=0
NO_BOT=0

# предыдущая установка → сохранённые значения (флаги ниже их переопределят).
# Читается ДО скачивания: сохранённый токен нужен уже для тарбола приватного репо.
[ -r /etc/shvpn/env.sh ] && source /etc/shvpn/env.sh

need_val() {
    [ $# -ge 2 ] && [ -n "$2" ] || { echo "Флаг $1 требует значения (см. --help)" >&2; exit 1; }
}

while [ $# -gt 0 ]; do
    case "$1" in
        --yes|-y) ASSUME_YES=1; shift ;;
        --server-ip) need_val "$@"; SERVER_IP="$2"; shift 2 ;;
        --tg-domain) need_val "$@"; TG_DOMAIN="$2"; shift 2 ;;
        --tg-secret) need_val "$@"; TG_SECRET="$2"; shift 2 ;;
        --bot-token) need_val "$@"; BOT_TOKEN="$2"; NO_BOT=0; shift 2 ;;
        --bot-chat) need_val "$@"; BOT_CHAT="$2"; NO_BOT=0; shift 2 ;;
        --no-bot) NO_BOT=1; shift ;;
        --with-awg) WITH_AWG=1; shift ;;
        --no-awg) WITH_AWG=0; shift ;;
        --awg-kernel) AWG_KERNEL=1; shift ;;
        --awg-userspace) AWG_KERNEL=0; shift ;;
        --with-vpsadmin) WITH_VPSADMIN=1; shift ;;
        --no-vpsadmin) WITH_VPSADMIN=0; shift ;;
        --vpsadmin-version) need_val "$@"; VPSADMIN_VERSION="$2"; shift 2 ;;
        --repo) need_val "$@"; SHVPN_REPO="$2"; shift 2 ;;
        --branch) need_val "$@"; SHVPN_BRANCH="$2"; shift 2 ;;
        --github-token) need_val "$@"; SHVPN_TOKEN="$2"; shift 2 ;;
        --help|-h) cat <<'HELPEOF'
shvpn installer — VPN-схема «наружу только 443» (3x-ui + telemt + AWG + vps-admin).

Запуск на VPS от root:
  bash install.sh                        # интерактивно (из клона репозитория)
  curl -fsSL https://raw.githubusercontent.com/LankoRu/self-hosted-vpn/main/install.sh | bash

Флаги:
  --yes, -y                     все ответы по умолчанию (неинтерактивно)
  --server-ip IP                публичный IP (дефолт: определяется автоматически)
  --tg-domain DOMAIN            домен-маска Telegram FakeTLS (дефолт: petrovich.ru)
  --tg-secret SECRET            секрет MTProto (дефолт: генерируется)
  --bot-token TOKEN             Telegram bot token (@BotFather) — для vps-admin
  --bot-chat ID                 chat_id админа (@userinfobot) — для vps-admin
  --no-bot                      vps-admin без Telegram-бота (панель + сбор)
  --with-awg / --no-awg         ставить AmneziaWG 3.1, UDP-443 (дефолт: да)
  --awg-kernel / --awg-userspace  kernel mode AWG (дефолт: kernel)
  --with-vpsadmin / --no-vpsadmin  ставить vps-admin (дефолт: да)
  --vpsadmin-version VER        версия бинаря из Releases (latest | v0.1.0 …)
  --repo OWNER/NAME             репозиторий (дефолт: LankoRu/self-hosted-vpn)
  --branch BRANCH               ветка (дефолт: main)
  --github-token TOKEN          PAT для приватного репо (Contents: Read)
  --help, -h                    эта справка

Пример одной командой (без интерактива):
  curl -fsSL https://raw.githubusercontent.com/LankoRu/self-hosted-vpn/main/install.sh \
    | bash -s -- --yes --bot-token 123:ABC... --bot-chat 123456789

Приватный репо (форк):
  curl -fsSL -H "Authorization: Bearer $TOKEN" \
    https://raw.githubusercontent.com/OWNER/self-hosted-vpn/main/install.sh \
    | bash -s -- --yes --github-token "$TOKEN" --repo OWNER/self-hosted-vpn \
      --bot-token 123:ABC... --bot-chat 123456789

Без флагов — интерактивные вопросы (Enter = дефолт). При --yes обязательны
--bot-token и --bot-chat (или --no-vpsadmin). Секреты генерируются на сервере;
итоговый отчёт: /root/shvpn-result.txt.
HELPEOF
            exit 0 ;;
        *) echo "Неизвестный флаг: $1 (см. --help)"; exit 1 ;;
    esac
done
export ASSUME_YES

# --- locating the project (curl|bash → download the repo) -------------------
SELF="${BASH_SOURCE[0]:-$0}"
SCRIPT_DIR="$(cd "$(dirname "$SELF")" 2>/dev/null && pwd || true)"
if [ -z "$SCRIPT_DIR" ] || [ ! -d "$SCRIPT_DIR/config" ]; then
    echo ">>> Режим curl|bash: скачиваю репозиторий ${SHVPN_REPO}@${SHVPN_BRANCH}"
    command -v curl >/dev/null || { echo "нужен curl"; exit 1; }
    WORKDIR=$(mktemp -d /tmp/shvpn-XXXXXX)
    if [ -n "$SHVPN_TOKEN" ]; then
        # Приватный репо: тарбол через API с токеном (Contents: Read).
        curl -fsSL --retry 6 --retry-delay 3 --retry-all-errors --connect-timeout 20 --max-time 300 \
            -H "Authorization: Bearer $SHVPN_TOKEN" \
            "https://api.github.com/repos/${SHVPN_REPO}/tarball/${SHVPN_BRANCH}" \
            | tar -xz -C "$WORKDIR" || { echo "не удалось скачать репозиторий (проверьте --github-token/--repo/--branch)"; exit 1; }
    else
        curl -fsSL --retry 6 --retry-delay 3 --retry-all-errors --connect-timeout 20 --max-time 300 \
            "https://codeload.github.com/${SHVPN_REPO}/tar.gz/refs/heads/${SHVPN_BRANCH}" \
            | tar -xz -C "$WORKDIR" || { echo "не удалось скачать репозиторий (приватный? задайте --github-token)"; exit 1; }
    fi
    SCRIPT_DIR=$(find "$WORKDIR" -maxdepth 1 -mindepth 1 -type d | head -1)
    [ -n "$SCRIPT_DIR" ] && [ -d "$SCRIPT_DIR/config" ] || { echo "репозиторий скачался повреждённым"; exit 1; }
    echo ">>> Проект: $SCRIPT_DIR"
fi
cd "$SCRIPT_DIR"
source scripts/lib.sh

# ----------------------------------------------------------------------------
[ "$(id -u)" = 0 ] || die "запустите от root: sudo bash install.sh"
[ "$(uname -s)" = "Linux" ] || die "только Linux (VPS)"
command -v apt-get >/dev/null || die "нужен apt-based дистрибутив (Ubuntu/Debian)"
. /etc/os-release 2>/dev/null || true
echo ">>> shvpn installer — ${PRETTY_NAME:-Linux}, $(uname -m)"

# ----------------------------------------------------------------------------
step "Настройка (заданные флагами/сохранённые значения вопросы пропускают)"

DETECTED_IP=$(detect_ip)
[ -n "${SERVER_IP:-}" ] || ask SERVER_IP "Публичный IP сервера" "$DETECTED_IP"
[ -n "$SERVER_IP" ] || die "IP не задан (--server-ip)"

if [ -f /etc/x-ui/x-ui.db ]; then
    ok "Найдена существующая 3x-ui — использую её"
    INSTALL_XUI=0
else
    ask_yn INSTALL_XUI "3x-ui не найдена. Установить её?" "y"
    [ "$INSTALL_XUI" = "1" ] || die "без 3x-ui схема не работает (установите её и перезапустите)"
fi

[ -n "${TG_DOMAIN:-}" ] || ask TG_DOMAIN "Домен-маска для Telegram (FakeTLS SNI)" "petrovich.ru"
if [ -z "${TG_SECRET:-}" ]; then TG_SECRET=$(gen_hex 16); fi

[ -n "${WITH_AWG:-}" ] || ask_yn WITH_AWG "Ставить AmneziaWG 3.1 (UDP-443)?" "y"
if [ "$WITH_AWG" = "1" ]; then
    [ -n "${AWG_KERNEL:-}" ] || ask_yn AWG_KERNEL "Kernel mode для AWG (быстрее; модуль ядра DKMS)?" "y"
else
    AWG_KERNEL=0
fi

[ -n "${WITH_VPSADMIN:-}" ] || ask_yn WITH_VPSADMIN "Ставить vps-admin (бот + веб-панель, регистрация во всех сервисах)?" "y"
BOT_TOKEN="${BOT_TOKEN:-}"; BOT_CHAT="${BOT_CHAT:-}"
if [ "$WITH_VPSADMIN" = "1" ]; then
    if [ "$NO_BOT" = "1" ]; then
        BOT_TOKEN=""; BOT_CHAT=""
    elif [ "$ASSUME_YES" = "1" ]; then
        { [ -n "$BOT_TOKEN" ] && [ -n "$BOT_CHAT" ]; } \
            || die "--yes: задайте --bot-token и --bot-chat (или --no-bot / --no-vpsadmin)"
    else
        ask_yn _BOT_ON "Подключить Telegram-бота (управление и отчёты в чате)?" "y"
        [ "$_BOT_ON" = "1" ] || NO_BOT=1
        while [ "$NO_BOT" = "0" ] && [ -z "$BOT_TOKEN" ]; do
            ask_secret BOT_TOKEN "Telegram bot token (@BotFather)" ""
            [ -n "$BOT_TOKEN" ] || warn "токен обязателен для vps-admin"
        done
        while [ "$NO_BOT" = "0" ] && [ -z "$BOT_CHAT" ]; do
            ask BOT_CHAT "Telegram chat_id админа (узнать: @userinfobot)" ""
            [ -n "$BOT_CHAT" ] || warn "chat_id обязателен для vps-admin"
        done
    fi
fi

export SERVER_IP TG_DOMAIN TG_SECRET WITH_AWG AWG_KERNEL WITH_VPSADMIN BOT_TOKEN BOT_CHAT NO_BOT
export SHVPN_REPO SHVPN_BRANCH SHVPN_TOKEN
VPSADMIN_VERSION="${VPSADMIN_VERSION:-}"
export VPSADMIN_VERSION
# сохраняем выбор для повторных прогонов (env.sh — root-only 600, дедуп по ключу)
save_env "SHVPN_REPO=$SHVPN_REPO" "SHVPN_BRANCH=$SHVPN_BRANCH" \
         "WITH_AWG=$WITH_AWG" "AWG_KERNEL=$AWG_KERNEL" "WITH_VPSADMIN=$WITH_VPSADMIN"
[ -n "$SHVPN_TOKEN" ] && save_env "SHVPN_TOKEN=$SHVPN_TOKEN"
[ -n "$BOT_TOKEN" ] && save_env "BOT_TOKEN=$BOT_TOKEN" "BOT_CHAT=$BOT_CHAT"
[ "$NO_BOT" = "1" ] && save_env "NO_BOT=1"
[ -n "$VPSADMIN_VERSION" ] && save_env "VPSADMIN_VERSION=$VPSADMIN_VERSION"

# ----------------------------------------------------------------------------
step "План установки"
cat <<EOF
  IP:            $SERVER_IP
  3x-ui:         $([ "$INSTALL_XUI" = 1 ] && echo "новая установка" || echo "существующая")
  Telegram:      telemt, домен-маска $TG_DOMAIN
  AmneziaWG:     $([ "$WITH_AWG" = 1 ] && echo "да, UDP-443, kernel=$AWG_KERNEL" || echo "нет")
  vps-admin:     $([ "$WITH_VPSADMIN" = 1 ] && echo "да (бот + веб-панель)" || echo "нет")
  Firewall:      только 22/tcp + 443/tcp$([ "$WITH_AWG" = 1 ] && echo "+udp")
  Сайт-заглушка: https://$SERVER_IP/
EOF
if [ "$ASSUME_YES" != "1" ]; then
    ask_yn GO "Продолжить?" "y"
    [ "$GO" = "1" ] || die "отменено"
fi

# ----------------------------------------------------------------------------
bash scripts/setup-base.sh
if [ "$WITH_AWG" = "1" ]; then bash scripts/setup-awg.sh; fi
if [ "$WITH_VPSADMIN" = "1" ]; then bash scripts/setup-vps-admin.sh; fi

# ----------------------------------------------------------------------------
step "Итоги"
source /etc/shvpn/env.sh
REPORT=/root/shvpn-result.txt
{
    echo "=== shvpn установлен на $SERVER_IP ($(date -u '+%Y-%m-%d %H:%M UTC')) ==="
    if [ -n "${VPSADMIN_PATH:-}" ]; then
        echo
        echo "Панель vps-admin: https://$SERVER_IP$VPSADMIN_PATH"
        echo "  логин: $(grep -oP 'login:\s*"?\K[^"\s]+' /etc/vps-admin/config.yaml 2>/dev/null || echo admin)"
        echo "  пароль: $(grep -oP 'password:\s*"\K[^"]+' /etc/vps-admin/config.yaml 2>/dev/null || echo 'см. /etc/vps-admin/config.yaml')"
        echo "  бот: /register <имя>, /device <имя> <устройство>, /today, /listusers, /digest"
    fi
    echo
    echo "Панель 3x-ui:    https://$SERVER_IP${PANEL_PATH}"
    if [ -f /etc/x-ui/install-result.env ]; then
        grep -E 'XUI_USERNAME|XUI_PASSWORD' /etc/x-ui/install-result.env | sed 's/^/  /'
    else
        echo "  логин/пароль панели — ваши существующие"
    fi
    echo "Подписки:        https://$SERVER_IP${SUB_PATH}"
    if [ "${AWG_ID:-0}" != "0" ]; then
        echo
        echo "AmneziaWG 3.1:   UDP-443, инбаунд id=$AWG_ID (kernel=$AWG_KERNEL)"
        echo "  клиенты: /register в боте, /device <имя> <устройство> — доп. конфиг AWG"
    fi
    echo
    echo "Персональные ссылки (VLESS/AWG/Telegram): бот /user <имя> или панель vps-admin."
    echo "VLESS-клиенты: через vps-admin (/register) или Add Client в панели."
    echo "Ссылки автоматически указывают на $SERVER_IP:443."
    echo "Сайт-заглушка:   https://$SERVER_IP/"
    echo
    echo "Логи: journalctl -u telemt -f | -u x-ui -f | -u vps-admin -f"
    echo "      tail -f /var/log/nginx/error.log"
    echo "Перезапуск схемы: bash $SCRIPT_DIR/install.sh (идемпотентно)"
} > "$REPORT"
chmod 600 "$REPORT"
printf '\n'; cat "$REPORT"
printf '\n'; ok "Готово. Итоги сохранены: $REPORT"

# ----------------------------------------------------------------------------
# Публикация итогов в Telegram-бот (чат админа). HTML-разметка: заголовки
# секций жирным, каждый логин/пароль/ссылка в <code> (моноширинный текст
# копируется тапом в мобильных клиентах). Персональные ссылки юзеров НЕ
# шлём сюда — они в боте (/user) и панели; bootstrap-секрет telemt не показываем.
if [ -n "${BOT_TOKEN:-}" ] && [ -n "${BOT_CHAT:-}" ]; then
    step "Публикация итогов в бота"
    TG_API="https://api.telegram.org/bot${BOT_TOKEN}/sendMessage"
    hesc() { printf '%s' "$1" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g'; }
    XUI_USER=$(grep -oP 'XUI_USERNAME=\K.*' /etc/x-ui/install-result.env 2>/dev/null || true)
    XUI_PASS=$(grep -oP 'XUI_PASSWORD=\K.*' /etc/x-ui/install-result.env 2>/dev/null || true)
    VA_LOGIN=$(grep -oP 'login:\s*"?\K[^"\s]+' /etc/vps-admin/config.yaml 2>/dev/null || echo admin)
    VA_PASS=$(grep -oP 'password:\s*"\K[^"]+' /etc/vps-admin/config.yaml 2>/dev/null || true)

    BOT_MSG="🛡 <b>shvpn — $(hesc "$SERVER_IP")</b>
$(date -u '+%Y-%m-%d %H:%M UTC')"
    if [ -n "${VPSADMIN_PATH:-}" ]; then
        BOT_MSG+="

🤖 <b>vps-admin</b>
$(hesc "https://$SERVER_IP$VPSADMIN_PATH")
логин: <code>$(hesc "$VA_LOGIN")</code>"
        [ -n "$VA_PASS" ] && BOT_MSG+="
пароль: <code>$(hesc "$VA_PASS")</code>"
        BOT_MSG+="
бот: /register · /device · /today · /listusers"
    fi
    BOT_MSG+="

🎛 <b>Панель 3x-ui</b>
$(hesc "https://$SERVER_IP$PANEL_PATH")"
    [ -n "$XUI_USER" ] && BOT_MSG+="
логин: <code>$(hesc "$XUI_USER")</code>"
    [ -n "$XUI_PASS" ] && BOT_MSG+="
пароль: <code>$(hesc "$XUI_PASS")</code>"
    BOT_MSG+="

📡 <b>Подписки</b>
<code>$(hesc "https://$SERVER_IP$SUB_PATH")</code>"
    if [ "${AWG_ID:-0}" != "0" ]; then
        BOT_MSG+="

📱 <b>AmneziaWG</b> — UDP-443, инбаунд $AWG_ID
клиенты: /register, /device &lt;имя&gt; &lt;устройство&gt;"
    fi
    BOT_MSG+="

ℹ️ Персональные ссылки (VLESS / AWG / Telegram) — в боте: /user &lt;имя&gt;, или в панели vps-admin.
🌐 Сайт-прикрытие: $(hesc "https://$SERVER_IP/")"

    send_plain() {
        if [ "$(wc -c < "$REPORT")" -le 4000 ]; then
            curl -sS -m 15 "$TG_API" -d chat_id="${BOT_CHAT}" \
                --data-urlencode text@"$REPORT" >/dev/null 2>&1
        else
            fold -s -w 3800 "$REPORT" | while IFS= read -r part; do
                curl -sS -m 15 "$TG_API" -d chat_id="${BOT_CHAT}" \
                    --data-urlencode "text=$part" >/dev/null 2>&1 || true
            done
        fi
    }
    if [ "${#BOT_MSG}" -le 4000 ]; then
        RESP=$(curl -sS -m 15 "$TG_API" -d chat_id="${BOT_CHAT}" \
            -d parse_mode=HTML -d disable_web_page_preview=true \
            --data-urlencode "text=$BOT_MSG" 2>/dev/null || true)
        if printf '%s' "$RESP" | grep -q '"ok":true'; then
            ok "Итоги отправлены в чат ${BOT_CHAT}"
        else
            warn "HTML-сообщение не принято — шлю plain"
            send_plain && ok "Итоги отправлены в чат ${BOT_CHAT} (plain)" \
                || warn "не удалось отправить итоги в бота (отчёт: $REPORT)"
        fi
    else
        send_plain && ok "Итоги отправлены в чат ${BOT_CHAT} (частями)" \
            || warn "не удалось отправить итоги в бота (отчёт: $REPORT)"
    fi
fi

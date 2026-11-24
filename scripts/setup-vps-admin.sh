#!/usr/bin/env bash
# setup-vps-admin.sh — prebuilt-бинарь vps-admin из GitHub Releases + конфиг +
# systemd + nginx-маршрутизация панели за 443 (секретный путь + cookie-gate).
# Бинарь собирается CI (.github/workflows/ci.yml: push в main → авто-тег → Release).
# На сервере Go НЕ нужен: скачиваем vps-admin-linux-<arch> + проверяем sha256.
# Env: SERVER_IP, BOT_TOKEN, BOT_CHAT, VPSADMIN_PORT (8088), VPSADMIN_PATH,
#      WEBPASS, XUI_*, VLESS_ID, AWG_ID (0 → AWG выключен), SUB_PATH,
#      SHVPN_REPO (LankoRu/self-hosted-vpn), SHVPN_TOKEN (для приватного репо),
#      VPSADMIN_VERSION (latest | v0.1.0 …).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
source "$HERE/lib.sh"
[ -f /etc/shvpn/env.sh ] && source /etc/shvpn/env.sh

SERVER_IP="${SERVER_IP:?}"
BOT_TOKEN="${BOT_TOKEN:-}"
BOT_CHAT="${BOT_CHAT:-}"
VPSADMIN_PORT="${VPSADMIN_PORT:-8088}"
VPSADMIN_PATH="${VPSADMIN_PATH:-/vps-$(gen_hex 6)/}"
WEBPASS="${WEBPASS:-$(openssl rand -base64 12 | tr -d '/+=' | head -c 16)}"
AWG_ID="${AWG_ID:-0}"
SHVPN_REPO="${SHVPN_REPO:-LankoRu/self-hosted-vpn}"
SHVPN_TOKEN="${SHVPN_TOKEN:-}"
VPSADMIN_VERSION="${VPSADMIN_VERSION:-latest}"
export SERVER_IP VPSADMIN_PORT VPSADMIN_PATH

[ "$(id -u)" = 0 ] || die "нужен root"
[ -d "$ROOT/vendor/vps-admin" ] || die "vendor/vps-admin не найден (нужен systemd unit)"
# --no-bot: панель без Telegram (панель и сбор статистики работают).
NO_BOT="${NO_BOT:-0}"
if [ "$NO_BOT" != "1" ] && { [ -z "$BOT_TOKEN" ] || [ -z "$BOT_CHAT" ]; }; then
    die "нужны BOT_TOKEN и BOT_CHAT (или --no-bot для панели без Telegram)"
fi
# kernel-режим AWG нужен vps-admin только для проверки здоровья (модуль загружен?)
AWG_KERNEL="${AWG_KERNEL:-1}"
[ "$AWG_KERNEL" = "1" ] && AWG_KERNEL_YAML=true || AWG_KERNEL_YAML=false

# ----------------------------------------------------------------------------
step "1/5 Бинарь vps-admin из GitHub Releases ($SHVPN_REPO, $VPSADMIN_VERSION)"
ARCH=$(dpkg --print-architecture 2>/dev/null || uname -m)
case "$ARCH" in
    amd64|x86_64) GOARCH=amd64 ;;
    arm64|aarch64) GOARCH=arm64 ;;
    *) die "архитектура $ARCH не поддержана" ;;
esac
ASSET="vps-admin-linux-${GOARCH}"
if [ "$VPSADMIN_VERSION" = "latest" ]; then
    BASE="https://github.com/$SHVPN_REPO/releases/latest/download"
else
    BASE="https://github.com/$SHVPN_REPO/releases/download/$VPSADMIN_VERSION"
fi
# Приватный репо: ассеты отдаются только с токеном (Contents: Read).
AUTH=()
[ -n "$SHVPN_TOKEN" ] && AUTH=(-H "Authorization: Bearer $SHVPN_TOKEN")

mkdir -p /opt/vps-admin /var/lib/vps-admin /var/log/vps-admin
TMP=$(mktemp -d /tmp/vps-admin-XXXXXX)
trap 'rm -rf "$TMP"' EXIT
dl "${AUTH[@]}" -o "$TMP/$ASSET" "$BASE/$ASSET" \
    || die "не удалось скачать $ASSET ($BASE). Приватный репо → задайте SHVPN_TOKEN; нет релизов →(push в main создаёт автоматически) или укажите VPSADMIN_VERSION"
dl "${AUTH[@]}" -o "$TMP/sha256sums.txt" "$BASE/sha256sums.txt" \
    || die "не удалось скачать sha256sums.txt"
( cd "$TMP" && grep " $ASSET\$" sha256sums.txt | sha256sum -c - ) \
    || die "контрольная сумма бинаря не совпала — установка прервана"
install -m 755 "$TMP/$ASSET" /opt/vps-admin/vps-admin.new
mv -f /opt/vps-admin/vps-admin.new /opt/vps-admin/vps-admin
ok "бинарь установлен: $ASSET ($(stat -c%s /opt/vps-admin/vps-admin) байт, sha256 ✓)"

# ----------------------------------------------------------------------------
step "2/5 systemd unit"
install -m 644 "$ROOT/vendor/vps-admin/vps-admin.service" /etc/systemd/system/vps-admin.service
systemctl daemon-reload
systemctl enable vps-admin >/dev/null 2>&1

# ----------------------------------------------------------------------------
step "3/5 Конфиг"
mkdir -p /etc/vps-admin
if [ -f /etc/vps-admin/config.yaml ]; then
    ok "config.yaml уже есть — не затираю (секреты сохранены)"
    WEBPASS=$(grep -oP 'password:\s*"\K[^"]+' /etc/vps-admin/config.yaml 2>/dev/null || echo "$WEBPASS")
else
    if [ "$AWG_ID" != "0" ]; then AWG_ENABLED=true; else AWG_ENABLED=false; fi
    export BOT_TOKEN BOT_CHAT WEBPASS AWG_ENABLED AWG_ID AWG_KERNEL_YAML
    render_tpl "$ROOT/config/vps-admin.config.yaml.tpl" /etc/vps-admin/config.yaml
    chmod 600 /etc/vps-admin/config.yaml
fi

# ----------------------------------------------------------------------------
step "4/5 nginx: панель за 443 (секретный путь + cookie-gate)"
mkdir -p /etc/nginx/snippets
export VPSADMIN_PATH VPSADMIN_PORT
render_tpl "$ROOT/config/vps-admin-locations.conf.tpl" /etc/nginx/snippets/vps-admin-locations.conf
if ! grep -q 'vpsadmin_login' /etc/nginx/sites-available/shvpn-web; then
    die "web.conf не содержит limit_req_zone/include — сначала выполните setup-base.sh (актуальная версия)"
fi
nginx -t || die "nginx -t не прошёл"
systemctl reload nginx

# ----------------------------------------------------------------------------
step "5/5 Запуск"
systemctl restart vps-admin
sleep 3
systemctl is-active --quiet vps-admin || { journalctl -u vps-admin --no-pager -n 20; die "vps-admin не стартовал"; }
ok "vps-admin active"
journalctl -u vps-admin --no-pager -n 8 | grep -E 'собрана|бот|панель' || true

save_env "VPSADMIN_PATH=$VPSADMIN_PATH" "VPSADMIN_PORT=$VPSADMIN_PORT"

# Один bot-токен = один поллер Telegram. Если тот же токен уже стоит на другом
# сервере, оба получат "409 Conflict: terminated by other getUpdates request".
if [ -n "$BOT_TOKEN" ]; then
    # уведомление в Telegram (необязательно, не валим установку при ошибке)
    curl -sS -m 10 "https://api.telegram.org/bot${BOT_TOKEN}/sendMessage" \
        -d chat_id="${BOT_CHAT}" \
        -d text="✅ vps-admin установлен на ${SERVER_IP}. Панель: https://${SERVER_IP}${VPSADMIN_PATH} Проверь /listusers и /today." >/dev/null 2>&1 || true
    sleep 8
    if journalctl -u vps-admin --since "-20s" --no-pager 2>/dev/null | grep -q 'Conflict: terminated by other getUpdates'; then
        warn "бот-токен уже используется другим сервером (Telegram 409 Conflict)."
        warn "Заведите отдельного бота у @BotFather и переустановите с --bot-token/--bot-chat,"
        warn "либо поставьте панель без бота: --no-bot (панель и сбор статистики работают)."
    fi
else
    ok "Telegram-бот отключён (--no-bot): панель и сбор статистики работают без уведомлений"
fi

ok "Панель vps-admin: https://$SERVER_IP$VPSADMIN_PATH"
ok "Пароль: $WEBPASS"
if [ -n "$BOT_TOKEN" ]; then
    ok "Бот: /register <имя>, /device <имя> <устройство>, /today, /listusers, /digest"
else
    ok "Бот выключен: управление — в веб-панели"
fi

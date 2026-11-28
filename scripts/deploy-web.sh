#!/usr/bin/env bash
# deploy-web.sh — точечное добавление WEB-прокси Telegram к УЖЕ установленной схеме
# shvpn (без переустановки 3x-ui/apt/telemt-бинаря). Переиспользует шаблоны и
# хелперы репозитория. Запуск от root на сервере, где рядом лежат config/ + scripts/.
#
# ВАЖНО: берёт ТЕКУЩИЙ /etc/telemt/telemt.toml как есть (сохраняет tls_domain,
# всех пользователей и секреты) и ДОПИСЫВАЕТ WEB-блок. Ничего не сбрасывает.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
source "$HERE/lib.sh"
[ -f /etc/shvpn/env.sh ] && source /etc/shvpn/env.sh

SERVER_IP="${SERVER_IP:?}"
TG_WEB_DOMAIN="${TG_WEB_DOMAIN:?}"
TELEMT_WEB_PORT="${TELEMT_WEB_PORT:-18080}"
CERT_DIR_WEB="${CERT_DIR_WEB:-/root/cert/web}"
export SERVER_IP TG_WEB_DOMAIN TELEMT_WEB_PORT CERT_DIR_WEB
[ "$(id -u)" = 0 ] || die "нужен root"

step "1/5 DNS"
python3 "$HERE/check-web-dns.py" "$TG_WEB_DOMAIN" "$SERVER_IP" \
    || die "DNS: $TG_WEB_DOMAIN не указывает на $SERVER_IP"

step "2/5 Сертификат на $TG_WEB_DOMAIN"
ACME="$HOME/.acme.sh/acme.sh"
[ -x "$ACME" ] || die "acme.sh не найден ($ACME)"
mkdir -p "$CERT_DIR_WEB"
if [ ! -f "$CERT_DIR_WEB/fullchain.pem" ]; then
    ufw allow 80/tcp >/dev/null 2>&1 || true
    # standalone занимает :80. Гасим nginx ТОЛЬКО если он слушает :80 — иначе
    # :443-роутер продолжает работать без простоя.
    NGINX_STOPPED=0
    if ss -tln 2>/dev/null | grep -q ':80 '; then
        systemctl is-active --quiet nginx && { systemctl stop nginx; NGINX_STOPPED=1; }
    fi
    "$ACME" --issue --standalone -d "$TG_WEB_DOMAIN" --ecc --server letsencrypt --force \
        || {
            [ "$NGINX_STOPPED" = "1" ] && systemctl start nginx
            ufw delete allow 80/tcp >/dev/null 2>&1 || true
            die "выпуск WEB-сертификата не удался"
        }
    [ "$NGINX_STOPPED" = "1" ] && systemctl start nginx
    ufw delete allow 80/tcp >/dev/null 2>&1 || true
fi
"$ACME" --install-cert -d "$TG_WEB_DOMAIN" --ecc \
    --key-file "$CERT_DIR_WEB/privkey.pem" \
    --fullchain-file "$CERT_DIR_WEB/fullchain.pem" \
    --reloadcmd "systemctl is-active --quiet nginx && systemctl reload nginx || true" \
    --pre-hook "ufw allow 80/tcp" \
    --post-hook "ufw delete allow 80/tcp" >/dev/null
ok "WEB-сертификат готов"

step "3/5 telemt: WEB-листенер + vhost + профили (валидация до остановки)"
mkdir -p /var/www/shvpn
cp "$ROOT/config/site/index.html" /var/www/shvpn/index.html
# Повторный запуск при уже настроенном WEB не дублирует блок.
if grep -q '^\[web\]' /etc/telemt/telemt.toml 2>/dev/null; then
    die "WEB уже настроен в telemt.toml. Для смены домена используйте install.sh --tg-web-domain."
fi
NEW_CFG=$(mktemp /tmp/shvpn-telemt.XXXXXX)
cp /etc/telemt/telemt.toml "$NEW_CFG"
render_tpl "$ROOT/config/telemt-web.toml.tpl" /tmp/shvpn-telemt-web.fragment
python3 "$HERE/telemt-web-profiles.py" "$NEW_CFG" /tmp/shvpn-telemt-web.fragment \
    || die "не удалось добавить WEB-профили"
rm -f /tmp/shvpn-telemt-web.fragment
python3 - "$NEW_CFG" <<'PYEOF' || die "telemt.toml не парсится как TOML"
import sys
try:
    import tomllib
except ModuleNotFoundError:
    sys.exit(0)
tomllib.load(open(sys.argv[1], "rb"))
PYEOF
if command -v telemt >/dev/null 2>&1; then
    HC_OUT=$(telemt healthcheck "$NEW_CFG" 2>&1 || true)
    if printf '%s' "$HC_OUT" | grep -q 'config load failed'; then
        rm -f "$NEW_CFG"
        die "новый telemt.toml не прошёл валидацию (старый telemt не остановлен): $HC_OUT"
    fi
fi
systemctl stop telemt 2>/dev/null || true
install -m 660 "$NEW_CFG" /etc/telemt/telemt.toml
rm -f "$NEW_CFG"
chown telemt:telemt /etc/telemt /etc/telemt/telemt.toml
chmod 770 /etc/telemt && chmod 660 /etc/telemt/telemt.toml
systemctl start telemt
sleep 1
systemctl is-active --quiet telemt || die "telemt не стартовал (journalctl -u telemt)"
ok "telemt с WEB-прокси активен"

step "4/5 nginx WEB-vhost"
render_tpl "$ROOT/config/telemt-web.conf.tpl" /etc/nginx/sites-available/shvpn-telemt-web
ln -sf /etc/nginx/sites-available/shvpn-telemt-web /etc/nginx/sites-enabled/shvpn-telemt-web
# shvpn-web обязан быть default_server на этом порту: WEB-vhost грузится раньше
# по алфавиту и иначе отдавал бы свой доменный серт на запросы по IP/без SNI.
if [ -f /etc/nginx/sites-available/shvpn-web ] && ! grep -q "default_server" /etc/nginx/sites-available/shvpn-web; then
    sed -i "s/listen 127.0.0.1:${WEB_PORT} ssl proxy_protocol;/listen 127.0.0.1:${WEB_PORT} ssl proxy_protocol default_server;/" /etc/nginx/sites-available/shvpn-web
fi
nginx -t || die "nginx -t не прошёл"
systemctl reload nginx
ok "nginx WEB-vhost включён"

step "5/5 Сохранение настроек"
save_env "TG_WEB_DOMAIN=$TG_WEB_DOMAIN" "TELEMT_WEB_PORT=$TELEMT_WEB_PORT" "CERT_DIR_WEB=$CERT_DIR_WEB"
ok "WEB-прокси развёрнут: https://$TG_WEB_DOMAIN/"
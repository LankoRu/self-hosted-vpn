#!/usr/bin/env bash
# setup-base.sh — базовая схема shvpn: 3x-ui + telemt + nginx SNI-роутер +
# сайт-заглушка + LE-сертификат на IP + ufw (только 22 и 443/tcp).
# Запуск от root. Env: SERVER_IP, TG_DOMAIN, TG_SECRET, TELEMT_PORT, WEB_PORT,
# CERT_DIR; опц. XUI_API_TOKEN. Подключает lib.sh сам.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
source "$HERE/lib.sh"

SERVER_IP="${SERVER_IP:?}"
TG_DOMAIN="${TG_DOMAIN:-petrovich.ru}"
TG_SECRET="${TG_SECRET:-$(gen_hex 16)}"
TELEMT_PORT="${TELEMT_PORT:-8443}"
WEB_PORT="${WEB_PORT:-8444}"
CERT_DIR="${CERT_DIR:-/root/cert/ip}"
XUI_DB="${XUI_DB:-/etc/x-ui/x-ui.db}"
# WEB-прокси Telegram (опционально): свой домен + отдельный WEB-листенер telemt.
TG_WEB_DOMAIN="${TG_WEB_DOMAIN:-}"
TELEMT_WEB_PORT="${TELEMT_WEB_PORT:-18080}"
CERT_DIR_WEB="${CERT_DIR_WEB:-/root/cert/web}"
export SERVER_IP TG_DOMAIN TG_SECRET TELEMT_PORT WEB_PORT CERT_DIR XUI_DB
export TG_WEB_DOMAIN TELEMT_WEB_PORT CERT_DIR_WEB

[ "$(id -u)" = 0 ] || die "нужен root"
export DEBIAN_FRONTEND=noninteractive

# WEB-домен должен быть настоящим FQDN (не IP) и отличаться от домена-маски
# FakeTLS и от самого IP — иначе stream-роутер уведёт его не в web-бэкенд.
if [ -n "$TG_WEB_DOMAIN" ]; then
    python3 - "$TG_WEB_DOMAIN" "$TG_DOMAIN" "$SERVER_IP" <<'PYEOF' || die "некорректный --tg-web-domain"
import re, sys
web, mask, ip = sys.argv[1], sys.argv[2], sys.argv[3]
web = web.strip().lower()
if web == mask.lower() or web == ip.lower():
    sys.exit("WEB-домен не должен совпадать с доменом-маской FakeTLS или IP сервера")
if re.match(r'^\d+\.\d+\.\d+\.\d+$', web) or ':' in web:
    sys.exit("WEB-домен должен быть FQDN, а не IP-адресом (telemt WEB не принимает IP)")
if not re.match(r'^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$', web):
    sys.exit("WEB-домен должен быть полным доменным именем (например tg.example.com)")
PYEOF
fi

# ----------------------------------------------------------------------------
step "1/9 Зависимости"
apt-get update -qq
apt-get install -y -qq nginx libnginx-mod-stream sqlite3 python3 curl openssl git ufw >/dev/null
ensure_swap

# ----------------------------------------------------------------------------
step "2/9 Панель 3x-ui"
if [ -f "$XUI_DB" ]; then
    ok "3x-ui уже установлена ($XUI_DB)"
else
    echo "Устанавливаю 3x-ui (неинтерактивно, случайные креды)..."
    # Установщик прибит к тегу: master мог поменять интерактивность/пути между
    # прогонами. Версию панели установщик берёт из releases/latest (своего
    # аргумента версии у него нет) — фактическую версию печатаем после установки.
    XUI_INSTALLER_TAG="${XUI_INSTALLER_TAG:-v3.11.0}"
    dl "https://raw.githubusercontent.com/MHSanaei/3x-ui/${XUI_INSTALLER_TAG}/install.sh" -o /tmp/xui-install.sh \
        || die "не удалось скачать установщик 3x-ui ${XUI_INSTALLER_TAG}"
    XUI_NONINTERACTIVE=1 bash /tmp/xui-install.sh </dev/null || die "установка 3x-ui не удалась"
    rm -f /tmp/xui-install.sh
    ok "3x-ui установлена: $(/usr/local/x-ui/x-ui -v 2>/dev/null | head -1) (установщик ${XUI_INSTALLER_TAG})"
    [ -f "$XUI_DB" ] || die "3x-ui не появилась после установки"
fi
systemctl is-active --quiet x-ui || systemctl start x-ui
sleep 2

# реквизитыfresh-установки (если есть)
if [ -f /etc/x-ui/install-result.env ]; then
    # shellcheck disable=SC1091
    source /etc/x-ui/install-result.env 2>/dev/null || true
fi

# ----------------------------------------------------------------------------
step "3/9 API-токен панели"
XUI_PORT=$(sqlite3 "$XUI_DB" "SELECT value FROM settings WHERE key='webPort';")
XUI_PATH=$(sqlite3 "$XUI_DB" "SELECT value FROM settings WHERE key='webBasePath';" | tr -d '/')
# панель может слушать HTTPS (свой серт) — определяем протокол
if [ -n "$(sqlite3 "$XUI_DB" "SELECT value FROM settings WHERE key='webCertFile';")" ]; then
    XUI_SCHEME=https
else
    XUI_SCHEME=http
fi
export XUI_SCHEME

# кандидаты: явный env → токены из БД → токен fresh-установки; проверяем каждый
CANDIDATES="${XUI_API_TOKEN:-}"
while IFS= read -r t; do [ -n "$t" ] && CANDIDATES="$CANDIDATES $t"; done \
    < <(sqlite3 "$XUI_DB" "SELECT token FROM api_tokens WHERE enabled=1 ORDER BY id;" 2>/dev/null || true)
[ -n "${XUI_API_TOKEN_FROM_ENV:-}" ] && CANDIDATES="$CANDIDATES $XUI_API_TOKEN_FROM_ENV"
XUI_TOKEN=""
for t in $CANDIDATES; do
    XUI_TOKEN="$t"
    if xui_api GET /inbounds/list | grep -q '"obj"'; then
        ok "API-токен принят панелью"
        break
    fi
    XUI_TOKEN=""
done
if [ -z "$XUI_TOKEN" ]; then
    XUI_TOKEN=$(gen_hex 32)
    sqlite3 "$XUI_DB" "INSERT INTO api_tokens(name,token,enabled,created_at,scope,expires_at) VALUES('shvpn','$XUI_TOKEN',1,$(date +%s%3N),'admin',0);"
    systemctl restart x-ui; sleep 2
    xui_api GET /inbounds/list | grep -q '"obj"' || die "API панели не отвечает даже с новым токеном"
    ok "создан API-токен shvpn"
fi
export XUI_PORT XUI_PATH XUI_TOKEN
ok "API доступен: 127.0.0.1:$XUI_PORT/$XUI_PATH"

# ----------------------------------------------------------------------------
step "4/9 VLESS Reality инбаунд (если нет)"
HAS_TLS=$(python3 "$HERE/xui-localhost.py" --read-only | python3 -c "import json,sys;print(len(json.load(sys.stdin)['sni_map']))")
if [ "$HAS_TLS" = "0" ]; then
    echo "Создаю VLESS+Reality инбаунд (цель google.com:443)..."
    XRAY_BIN=$(find /usr/local/x-ui/bin -name 'xray-linux-*' | head -1)
    KEYS=$("$XRAY_BIN" x25519)
    PRIV=$(echo "$KEYS" | awk '/[Pp]rivate/{print $NF}')
    PUB=$(echo "$KEYS" | awk '/[Pp]ublic/{print $NF}')
    VPORT=$((20000 + RANDOM % 20000))
    python3 - "$PRIV" "$PUB" "$VPORT" <<'PYEOF' > /tmp/shvpn-vless.json
import json, sys, secrets
priv, pub, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
snis = ["google.com","google-analytics.com","g.co","goo.gl","www.goo.gl",
        "android.com","ai.android","urchin.com","googlecommerce.com","android.clients.google.com"]
# 3x-ui 3.11 строит vless-ссылку из ВЛОЖЕННОГО settings{publicKey,fingerprint,
# spiderX}. Без publicKey панель молча выбрасывает pbk/fp из ссылки, и клиент
# не может подключиться (Reality без публичного ключа не работает). Пишем оба
# формата: вложенный settings (для ссылок) + target/xver (для xray-core).
stream = {"network":"tcp","security":"reality","realitySettings":{
    "privateKey":priv,"serverNames":snis,
    "shortIds":[secrets.token_hex(8),secrets.token_hex(4),secrets.token_hex(2)],
    "target":"google.com:443","xver":0,
    "settings":{"publicKey":pub,"fingerprint":"chrome","spiderX":"/"}},
    "tcpSettings":{"acceptProxyProtocol":True,"header":{"type":"none"}}}
print(json.dumps({"up":0,"down":0,"total":0,"remark":"vless-reality","enable":True,
    "expiryTime":0,"listen":"","port":port,"protocol":"vless",
    "settings":json.dumps({"clients":[],"decryption":"none"}),
    "streamSettings":json.dumps(stream),"sniffing":json.dumps({"enabled":False})}))
PYEOF
    xui_api POST /inbounds/add "$(cat /tmp/shvpn-vless.json)" | grep -q '"success":true' \
        || die "не удалось создать VLESS-инбаунд"
    rm -f /tmp/shvpn-vless.json
    ok "VLESS-инбаунд создан (порт $VPORT, клиенты добавляются через vps-admin/панель)"
else
    ok "TLS-инбаунды уже есть ($HAS_TLS SNI)"
fi

# ----------------------------------------------------------------------------
step "5/9 Читаю настройки панели (пути, порты, SNI)"
python3 "$HERE/xui-localhost.py" --read-only > "$ROOT/xui-info.json"
VLESS_ID=$(python3 -c "
import json
d = json.load(open('$ROOT/xui-info.json'))
print(d['xray_ports'][0] if d['xray_ports'] else 0)")
# id инбаунда по порту
VLESS_ID=$(sqlite3 "$XUI_DB" "SELECT id FROM inbounds WHERE port=$VLESS_ID AND protocol IN ('vless','vmess','trojan') LIMIT 1;")
SUB_PATH=$(python3 -c "import json;print(json.load(open('$ROOT/xui-info.json'))['sub_path'])")
PANEL_PATH=$(python3 -c "import json;print(json.load(open('$ROOT/xui-info.json'))['panel_path'])")
export VLESS_ID SUB_PATH PANEL_PATH
# переменные для рендера шаблонов nginx из xui-info.json
eval "$(python3 - "$ROOT/xui-info.json" <<'PYEOF'
import json, sys
d = json.load(open(sys.argv[1]))
print(f"export PANEL_PORT={d['panel_port']} SUB_PORT={d['sub_port']}")
print(f"export SUB_JSON_PATH='{d['sub_json_path']}' SUB_CLASH_PATH='{d['sub_clash_path']}'")
sni_map = "\n".join(f"    {m['sni']} xray_{m['port']};" for m in d['sni_map'])
ups = "\n\n".join(f"upstream xray_{p} {{\n    server 127.0.0.1:{p};\n}}" for p in d['xray_ports'])
import shlex
print("export REALITY_SNI_MAP=" + shlex.quote(sni_map))
print("export XRAY_UPSTREAMS=" + shlex.quote(ups))
PYEOF
)"

# ----------------------------------------------------------------------------
step "6/9 telemt (Telegram MTProto, 127.0.0.1:$TELEMT_PORT)"
if ! command -v telemt >/dev/null 2>&1; then
    # -l en обязателен: без него установщик telemt спрашивает язык через
    # `read </dev/tty` и вешает неинтерактивную установку (--yes / curl|bash).
    # Тег фиксирует и скрипт, и версию бинаря (воспроизводимость).
    TELEMT_TAG="${TELEMT_TAG:-3.5.17}"
    dl "https://raw.githubusercontent.com/telemt/telemt/${TELEMT_TAG}/install.sh" -o /tmp/telemt-install.sh \
        || die "не удалось скачать установщик telemt ${TELEMT_TAG}"
    VERSION="$TELEMT_TAG" sh /tmp/telemt-install.sh -l en -p "$TELEMT_PORT" -s "$TG_SECRET" -d "$TG_DOMAIN" \
        || die "установка telemt не удалась"
    rm -f /tmp/telemt-install.sh
fi
mkdir -p /etc/nginx/stream.d /var/www/shvpn /etc/telemt
# Сайт-прикрытие кладём ДО старта telemt: WEB-режим строит immutable-снимок
# каталога при старте (decoy static_directory). В шаге 8 копия повторится (идемпотентно).
cp "$ROOT/config/site/index.html" /var/www/shvpn/index.html
# Новый конфиг собираем во ВРЕМЕННОМ файле и валидируем ДО остановки telemt:
# невалидный конфиг не уронит работающий прокси.
NEW_CFG=$(mktemp /tmp/shvpn-telemt.XXXXXX)
# сохраняем существующих пользователей (созданных через API/vps-admin) при повторном прогоне
OLD_USERS=""
if [ -f /etc/telemt/telemt.toml ]; then
    OLD_USERS=$(python3 - /etc/telemt/telemt.toml <<'PYEOF'
import re, sys
s = open(sys.argv[1]).read()
m = re.search(r"\[access\.users\]\n(.*?)(?=\n\[|\Z)", s, re.S)
if m:
    for line in m.group(1).splitlines():
        if re.match(r'^\s*[A-Za-z0-9_-]+\s*=\s*"[0-9a-f]{32}"', line):
            print(line.strip())
PYEOF
)
fi
render_tpl "$ROOT/config/telemt.toml.tpl" "$NEW_CFG"
if [ -n "$OLD_USERS" ]; then
    python3 - "$NEW_CFG" "$OLD_USERS" <<'PYEOF'
import re, sys
path, old = sys.argv[1], sys.argv[2]
s = open(path).read()
existing = dict(re.findall(r'^([A-Za-z0-9_-]+)\s*=\s*"([0-9a-f]{32})"', s, re.M))
for line in old.splitlines():
    m = re.match(r'^([A-Za-z0-9_-]+)\s*=\s*"([0-9a-f]{32})"', line)
    if m and m.group(1) not in existing:
        s = s.rstrip() + f"\n{m.group(1)} = \"{m.group(2)}\"\n"
open(path, "w").write(s)
PYEOF
    ok "сохранены существующие пользователи telemt"
fi
# WEB-прокси: дописываем WEB-листенер + [web] + vhost и генерируем профиль (dd)
# для каждого пользователя [access.users]. Рендер с нуля каждый прогон, поэтому
# дублей профилей нет. Фрагмент — отдельный шаблон (только при TG_WEB_DOMAIN).
if [ -n "$TG_WEB_DOMAIN" ]; then
    render_tpl "$ROOT/config/telemt-web.toml.tpl" /tmp/shvpn-telemt-web.fragment
    python3 "$HERE/telemt-web-profiles.py" "$NEW_CFG" /tmp/shvpn-telemt-web.fragment \
        || die "не удалось добавить WEB-профили telemt"
    rm -f /tmp/shvpn-telemt-web.fragment
fi
# Валидация нового конфига ДО остановки сервиса: tomllib (структура TOML) +
# telemt healthcheck (полная валидация telemt через parse_source). У healthcheck
# важен только этап загрузки конфига ("config load failed"); API-проба может не
# пройти (напр. свежая установка) — это не ошибка конфига.
python3 - "$NEW_CFG" <<'PYEOF' || die "telemt.toml не парсится как TOML"
import sys
try:
    import tomllib
except ModuleNotFoundError:
    sys.exit(0)  # старый python без tomllib — проверит telemt ниже
tomllib.load(open(sys.argv[1], "rb"))
PYEOF
if command -v telemt >/dev/null 2>&1; then
    HC_OUT=$(telemt healthcheck "$NEW_CFG" 2>&1 || true)
    if printf '%s' "$HC_OUT" | grep -q 'config load failed'; then
        rm -f "$NEW_CFG"
        die "новый telemt.toml не прошёл валидацию (старый telemt не остановлен): $HC_OUT"
    fi
fi
# Конфиг валиден — останавливаем старый telemt и устанавливаем новый.
systemctl stop telemt 2>/dev/null || true
install -m 660 "$NEW_CFG" /etc/telemt/telemt.toml
rm -f "$NEW_CFG"
# владелец telemt + group-write: API-провижининг атомарно перезаписывает конфиг
# Логи telemt — в файл (см. [logging] в шаблоне); каталог принадлежит telemt.
install -d -o telemt -g telemt -m 750 /var/log/telemt
# Журнал systemd ограничиваем: без лимита он растёт до 10% диска (на 10-ГБ VPS
# это ~1 ГБ, а при болтливом сервисе — забивает диск).
install -d -m 755 /etc/systemd/journald.conf.d
printf '[Journal]\nSystemMaxUse=200M\n' > /etc/systemd/journald.conf.d/90-size.conf
systemctl restart systemd-journald 2>/dev/null || true
chown telemt:telemt /etc/telemt /etc/telemt/telemt.toml
chmod 770 /etc/telemt && chmod 660 /etc/telemt/telemt.toml
systemctl start telemt
sleep 1
systemctl is-active --quiet telemt || die "telemt не стартовал (journalctl -u telemt)"

# ----------------------------------------------------------------------------
step "7/9 Сертификат Let's Encrypt на IP"
mkdir -p "$CERT_DIR"
ACME="$HOME/.acme.sh/acme.sh"
if [ ! -x "$ACME" ]; then
    # Установщик acme.sh качает архив с GitHub (10-30 c). Без ретраев он падал
    # на «свежей» сети; ошибку глушить нельзя — иначе непонятный «не установился».
    for attempt in 1 2 3; do
        if dl https://get.acme.sh -o /tmp/acme-install.sh \
            && sh /tmp/acme-install.sh email="admin@${SERVER_IP}.nip.io" >/dev/null; then
            break
        fi
        warn "установка acme.sh не удалась (попытка $attempt/3), повтор через 5 c"
        sleep 5
    done
    rm -f /tmp/acme-install.sh
    ACME="$HOME/.acme.sh/acme.sh"
fi
[ -x "$ACME" ] || die "acme.sh не установился"
if [ ! -f "$CERT_DIR/fullchain.pem" ]; then
    # Let's Encrypt для bare-IP выдаёт серты ТОЛЬКО из профиля shortlived
    # (6 дней, продление автоматически). Без профиля LE откажет.
    # --standalone требует свободный :80, а nginx уже слушает его — гасим на время.
    ufw allow 80/tcp >/dev/null 2>&1 || true
    NGINX_WAS_ACTIVE=0
    systemctl is-active --quiet nginx && NGINX_WAS_ACTIVE=1
    [ "$NGINX_WAS_ACTIVE" = "1" ] && systemctl stop nginx
    "$ACME" --issue --standalone -d "$SERVER_IP" --ecc --server letsencrypt \
        --certificate-profile shortlived --force \
        || {
            [ "$NGINX_WAS_ACTIVE" = "1" ] && systemctl start nginx
            ufw delete allow 80/tcp >/dev/null 2>&1 || true
            die "выпуск сертификата не удался (нужен свободный :80 и профиль shortlived)"
        }
    [ "$NGINX_WAS_ACTIVE" = "1" ] && systemctl start nginx
    ufw delete allow 80/tcp >/dev/null 2>&1 || true
fi
"$ACME" --install-cert -d "$SERVER_IP" --ecc \
    --key-file "$CERT_DIR/privkey.pem" \
    --fullchain-file "$CERT_DIR/fullchain.pem" \
    --reloadcmd "systemctl is-active --quiet nginx && systemctl reload nginx || true" \
    --pre-hook "ufw allow 80/tcp" \
    --post-hook "ufw delete allow 80/tcp" >/dev/null
ok "сертификат на месте (продление автоматически, :80 открывается на секунды)"

# WEB-прокси: отдельный сертификат на СВОЙ домен TG_WEB_DOMAIN (не IP, поэтому
# обычный профиль LE, не shortlived). Нужна A-запись домена на IP сервера.
if [ -n "$TG_WEB_DOMAIN" ]; then
    python3 "$HERE/check-web-dns.py" "$TG_WEB_DOMAIN" "$SERVER_IP" \
        || die "DNS: $TG_WEB_DOMAIN не указывает на $SERVER_IP (нужна A-запись)"
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
                die "выпуск WEB-сертификата не удался (нужен свободный :80 и A-запись $TG_WEB_DOMAIN)"
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
    ok "WEB-сертификат на $TG_WEB_DOMAIN готов"
fi

# ----------------------------------------------------------------------------
step "8/9 nginx: SNI-роутер :443 + сайт/панель/подписки"
cp "$ROOT/config/site/index.html" /var/www/shvpn/index.html
# SNI-карта создаётся только при первой установке: после неё владелец —
# vps-admin («Подключения» перегенерирует её из состояния 3x-ui + telemt).
# Повторный прогон установщика не должен откатывать ротацию SNI/транспорты.
if [ ! -f /etc/nginx/stream.d/shvpn-sni-map.conf ]; then
    render_tpl "$ROOT/config/stream-sni-map.conf.tpl" /etc/nginx/stream.d/shvpn-sni-map.conf
else
    ok "SNI-карта уже есть — не перезаписываю (владелец: vps-admin)"
fi
render_tpl "$ROOT/config/stream-router.conf.tpl" /etc/nginx/stream.d/shvpn-router.conf
render_tpl "$ROOT/config/web.conf.tpl" /etc/nginx/sites-available/shvpn-web
if ! grep -qE '^stream\s*\{' /etc/nginx/nginx.conf; then
    printf '\nstream {\n    include /etc/nginx/stream.d/*.conf;\n}\n' >> /etc/nginx/nginx.conf
fi
ln -sf /etc/nginx/sites-available/shvpn-web /etc/nginx/sites-enabled/shvpn-web
# WEB-прокси vhost (только при TG_WEB_DOMAIN): тот же loopback-порт, выбор по SNI.
# SNI этого домена в stream-карте попадает в `default web` — отдельная запись карты
# не нужна. Без домена — убираем vhost (идемпотентное отключение).
if [ -n "$TG_WEB_DOMAIN" ]; then
    render_tpl "$ROOT/config/telemt-web.conf.tpl" /etc/nginx/sites-available/shvpn-telemt-web
    ln -sf /etc/nginx/sites-available/shvpn-telemt-web /etc/nginx/sites-enabled/shvpn-telemt-web
else
    rm -f /etc/nginx/sites-enabled/shvpn-telemt-web /etc/nginx/sites-available/shvpn-telemt-web
fi
rm -f /etc/nginx/sites-enabled/default
nginx -t || die "nginx -t не прошёл"
systemctl enable nginx >/dev/null 2>&1
systemctl restart nginx

# ----------------------------------------------------------------------------
step "9/9 3x-ui на localhost + firewall"
systemctl stop x-ui
python3 "$HERE/xui-localhost.py" > "$ROOT/xui-applied.json"
systemctl start x-ui
sleep 3
systemctl is-active --quiet x-ui || die "x-ui не стартовала"

if command -v ufw >/dev/null 2>&1; then
    ufw allow 22/tcp >/dev/null
    ufw allow 443/tcp >/dev/null
    ufw status | grep -q 'Status: active' || { echo "y" | ufw enable; }
else
    warn "ufw не найден — настройте firewall вручную (22, 443/tcp)"
fi

save_env "SERVER_IP=$SERVER_IP" "TG_DOMAIN=$TG_DOMAIN" "TG_SECRET=$TG_SECRET" \
    "TELEMT_PORT=$TELEMT_PORT" "WEB_PORT=$WEB_PORT" "CERT_DIR=$CERT_DIR" \
    "TG_WEB_DOMAIN=$TG_WEB_DOMAIN" "TELEMT_WEB_PORT=$TELEMT_WEB_PORT" "CERT_DIR_WEB=$CERT_DIR_WEB" \
    "XUI_PORT=$XUI_PORT" "XUI_PATH=$XUI_PATH" "XUI_TOKEN=$XUI_TOKEN" "XUI_SCHEME=$XUI_SCHEME" \
    "VLESS_ID=$VLESS_ID" "SUB_PATH=$SUB_PATH" "PANEL_PATH=$PANEL_PATH"

ok "Базовая схема готова."
ok "  сайт:    https://$SERVER_IP/"
ok "  панель:  https://$SERVER_IP$PANEL_PATH"
ok "  подписки: https://$SERVER_IP$SUB_PATH"
if [ -n "$TG_WEB_DOMAIN" ]; then
    ok "  Telegram WEB-прокси: https://$TG_WEB_DOMAIN/"
fi

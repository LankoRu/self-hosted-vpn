#!/usr/bin/env bash
# lib.sh — общие хелперы установщика shvpn. Подключается через source.

C_STEP='\033[1;36m'; C_OK='\033[1;32m'; C_ERR='\033[1;31m'; C_WARN='\033[1;33m'; C_OFF='\033[0m'
step() { printf "\n${C_STEP}>>> %s${C_OFF}\n" "$*"; }
ok()   { printf "${C_OK}%s${C_OFF}\n" "$*"; }
warn() { printf "${C_WARN}ВНИМАНИЕ: %s${C_OFF}\n" "$*" >&2; }
die()  { printf "${C_ERR}ОШИБКА: %s${C_OFF}\n" "$*" >&2; exit 1; }

# dl ARGS... — curl для установщика: ретраи на транзиентные сбои.
# На свежем VPS сеть/DNS первые секунды часто не готовы («Could not resolve
# host»), и раньше установка падала на первом же скачивании.
dl() {
    curl -fsSL --retry 6 --retry-delay 3 --retry-all-errors \
        --connect-timeout 20 --max-time 300 "$@"
}

# ask VAR "вопрос" "default" — чтение с /dev/tty (работает и при curl|bash).
# Пустой ввод → default. В неинтерактивном режиме (ASSUME_YES=1) → default.
# ВАЖНО: внутренняя переменная не должна называться как у вызывающего кода.
# Bash связывает имена динамически: `local __ans` в ask() перекрывает __ans
# из ask_yn(), и printf -v __ans писал в перекрытую копию — ask_yn() всегда
# получал пустую строку и отвечал «нет» на любой вопрос.
ask() {
    local __var="$1" __prompt="$2" __def="${3:-}" __ask_reply=""
    if [ "${ASSUME_YES:-0}" = "1" ]; then
        printf -v "$__var" '%s' "$__def"
        return 0
    fi
    if [ -n "$__def" ]; then
        printf "%s [%s]: " "$__prompt" "$__def" >&2
    else
        printf "%s: " "$__prompt" >&2
    fi
    read -r __ask_reply </dev/tty || __ask_reply=""
    [ -z "$__ask_reply" ] && __ask_reply="$__def"
    printf -v "$__var" '%s' "$__ask_reply"
}

# ask_yn VAR "вопрос" "y|n" → 0/1 в переменную
ask_yn() {
    local __var="$1" __prompt="$2" __def="${3:-y}" __ans=""
    ask __ans "$__prompt (y/n)" "$__def"
    case "$__ans" in
        y|Y|yes|YES|да|Да|ДА) printf -v "$__var" '1' ;;
        *) printf -v "$__var" '0' ;;
    esac
}

# ask_secret VAR "вопрос" [default] — без эха (токены/пароли)
ask_secret() {
    local __var="$1" __prompt="$2" __def="${3:-}" __ask_reply=""
    if [ "${ASSUME_YES:-0}" = "1" ]; then
        printf -v "$__var" '%s' "$__def"
        return 0
    fi
    printf "%s: " "$__prompt" >&2
    read -rs __ask_reply </dev/tty || __ask_reply=""
    printf "\n" >&2
    [ -z "$__ask_reply" ] && __ask_reply="$__def"
    printf -v "$__var" '%s' "$__ask_reply"
}

# render_tpl SRC DST — подстановка __PLACEHOLDER__ из экспортированных env.
render_tpl() {
    python3 - "$1" "$2" <<'PYEOF'
import os, re, sys
src, dst = sys.argv[1], sys.argv[2]
text = open(src).read()
names = set(re.findall(r"__([A-Z0-9_]+)__", text))
missing = [n for n in names if n not in os.environ]
if missing:
    sys.exit(f"render_tpl {src}: не заданы env: {sorted(missing)}")
for n in names:
    text = text.replace(f"__{n}__", os.environ[n])
open(dst, "w").write(text)
print("  создан:", dst)
PYEOF
}

# ensure_swap — swap >=2G нужен для DKMS (сборка модуля AWG) на 1-ГБ VPS.
ensure_swap() {
    if swapon --show 2>/dev/null | grep -q .; then return 0; fi
    local ram_mb
    ram_mb=$(free -m | awk '/^Mem:/{print $2}')
    if [ "${ram_mb:-0}" -ge 2048 ]; then return 0; fi
    step "Создаю swap 2G (мало RAM: ${ram_mb}M)"
    if [ ! -f /swapfile ]; then
        fallocate -l 2G /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=2048 status=none
        chmod 600 /swapfile
        mkswap /swapfile >/dev/null
    fi
    swapon /swapfile
    grep -q '/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
    ok "swap включён"
}

detect_ip() {
    curl -s4 -m 8 https://api.ipify.org 2>/dev/null \
        || curl -s4 -m 8 https://ifconfig.me 2>/dev/null \
        || hostname -I | awk '{print $1}'
}

gen_hex() { openssl rand -hex "${1:-16}"; }

# xui_api — curl к API панели с токеном. $1=method, $2=endpoint, $3=body
xui_api() {
    local method="$1" endpoint="$2" body="${3:-}"
    local url="${XUI_SCHEME:-http}://127.0.0.1:${XUI_PORT}/${XUI_PATH}/panel/api${endpoint}"
    local -a extra=()
    [ "${XUI_SCHEME:-http}" = "https" ] && extra+=(-k)
    if [ -n "$body" ]; then
        curl -sS -m 30 "${extra[@]}" -X "$method" -H "Authorization: Bearer ${XUI_TOKEN}" \
            -H "Content-Type: application/json" -d "$body" "$url"
    else
        curl -sS -m 30 "${extra[@]}" -X "$method" -H "Authorization: Bearer ${XUI_TOKEN}" "$url"
    fi
}

# save_env KEY=VALUE... — дописывает в /etc/shvpn/env.sh (перезаписывая ключи)
save_env() {
    mkdir -p /etc/shvpn
    touch /etc/shvpn/env.sh
    chmod 600 /etc/shvpn/env.sh
    local kv key
    for kv in "$@"; do
        key="${kv%%=*}"
        sed -i "/^export ${key}=/d" /etc/shvpn/env.sh
        echo "export ${kv}" >> /etc/shvpn/env.sh
    done
}

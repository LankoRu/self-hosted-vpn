#!/usr/bin/env bash
# setup-awg.sh — AmneziaWG 3.1 инбаунд в 3x-ui на UDP-443 (+ kernel mode).
# Env: SERVER_IP, AWG_KERNEL (0/1), XUI_PORT/XUI_PATH/XUI_TOKEN/XUI_SCHEME,
#      AWG_PORT (443). Требует пройденный setup-base.sh.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
source "$HERE/lib.sh"
[ -f /etc/shvpn/env.sh ] && source /etc/shvpn/env.sh

SERVER_IP="${SERVER_IP:?}"
AWG_KERNEL="${AWG_KERNEL:-1}"
AWG_PORT="${AWG_PORT:-443}"
XUI_DB="${XUI_DB:-/etc/x-ui/x-ui.db}"
export SERVER_IP XUI_DB

[ "$(id -u)" = 0 ] || die "нужен root"

# ----------------------------------------------------------------------------
if [ "$AWG_KERNEL" = "1" ]; then
    step "AmneziaWG: kernel module + tools"
    if lsmod | grep -q '^amneziawg' && command -v awg >/dev/null 2>&1; then
        ok "модуль уже загружен: $(cat /sys/module/amneziawg/version 2>/dev/null), tools: $(awg --version 2>/dev/null)"
    else
        # DKMS не соберёт модуль без заголовков ядра. На Ubuntu они обычно уже
        # есть, на минимальном Debian (образцы облаков) — нет: ставим сами.
        KVER="$(uname -r)"
        if [ ! -d "/lib/modules/$KVER/build" ]; then
            echo "Ставлю заголовки ядра ($KVER)..."
            apt-get install -y -qq "linux-headers-$KVER" >/dev/null 2>&1 \
                || apt-get install -y -qq linux-headers-amd64 >/dev/null 2>&1 \
                || apt-get install -y -qq linux-headers-generic >/dev/null 2>&1 \
                || warn "заголовки ядра не установились автоматически"
        fi
        apt-get install -y -qq dkms build-essential >/dev/null 2>&1 || true
        [ -d "/lib/modules/$KVER/build" ] || die "нет заголовков ядра $KVER — DKMS не сможет собрать модуль (apt install linux-headers-$KVER)"

        if /usr/local/x-ui/x-ui install-awg-kernel; then
            ok "install-awg-kernel отработал"
        else
            # Патч 70ae2481 нужен только Ubuntu-ядрам 7.0 (бэкпорт API). На
            # Debian/других ядрах он неприменим — не тратим время и не путаем лог.
            if [ "$(uname -r | cut -d. -f1-2)" = "7.0" ]; then
                warn "install-awg-kernel упал — применяю патч Ubuntu-бэкпорта API (upstream 70ae2481)"
                python3 "$HERE/awg-kernel-patch.py"
            else
                warn "install-awg-kernel упал (ядро $(uname -r)) — патч нужен только для Ubuntu 7.0, пропускаю"
            fi
            dkms build -m amneziawg -v 1.0.0 --force || die "DKMS-сборка не удалась (см. /var/lib/dkms/amneziawg/1.0.0/build/make.log; можно поставить с --awg-userspace)"
            dkms install -m amneziawg -v 1.0.0 --force
            modprobe amneziawg || die "modprobe amneziawg не удался (Secure Boot?)"
            if ! command -v awg >/dev/null 2>&1; then
                (cd /usr/local/src/amneziawg-tools/src && make -j1 >/dev/null && make install >/dev/null) \
                    || die "сборка amneziawg-tools не удалась"
            fi
        fi
        lsmod | grep -q '^amneziawg' || modprobe amneziawg
        ok "модуль: $(cat /sys/module/amneziawg/version 2>/dev/null), tools: $(awg --version 2>/dev/null)"
    fi
else
    step "AmneziaWG: userspace (kernel mode выключен)"
fi

# ----------------------------------------------------------------------------
step "AmneziaWG: инбаунд в панели (UDP $AWG_PORT)"
AWG_ID=$(sqlite3 "$XUI_DB" "SELECT id FROM inbounds WHERE protocol='amneziawg' ORDER BY id LIMIT 1;")
if [ -z "$AWG_ID" ]; then
    RESP=$(xui_api POST /inbounds/add "$(python3 -c "
import json
print(json.dumps({'up':0,'down':0,'total':0,'remark':'amneziawg','enable':True,
  'expiryTime':0,'listen':'','port':$AWG_PORT,'protocol':'amneziawg',
  'settings':'{}','streamSettings':'{}','sniffing':'{}'}))")")
    echo "$RESP" | grep -q '"success":true' || die "создание AWG-инбаунда: $RESP"
    AWG_ID=$(echo "$RESP" | python3 -c "import json,sys;print(json.load(sys.stdin)['obj']['id'])")
    ok "инбаунд создан: id=$AWG_ID (панель сгенерировала профиль 3.1)"
else
    ok "инбаунд уже есть: id=$AWG_ID"
fi

#kernelMode + externalProxy (иначе Endpoint=localhost в ссылках/подписках)
python3 - "$AWG_ID" "$AWG_KERNEL" "$SERVER_IP" "$AWG_PORT" <<'PYEOF' > /tmp/shvpn-awg-update.json
import json, subprocess, sys, os
awg_id, kernel, ip, port = sys.argv[1], sys.argv[2] == "1", sys.argv[3], int(sys.argv[4])
scheme = os.environ.get("XUI_SCHEME", "http")
xport, xpath, tok = os.environ["XUI_PORT"], os.environ["XUI_PATH"], os.environ["XUI_TOKEN"]
url = f"{scheme}://127.0.0.1:{xport}/{xpath}/panel/api/inbounds/list"
cmd = ["curl", "-sS", "-m", "30", "-H", f"Authorization: Bearer {tok}", url]
if scheme == "https": cmd.insert(1, "-k")
data = json.loads(subprocess.check_output(cmd))
ib = next(i for i in data["obj"] if i["id"] == int(awg_id))
settings = json.loads(ib["settings"]) if isinstance(ib["settings"], str) else ib["settings"]
settings["server"]["kernelMode"] = kernel
ib["settings"] = json.dumps(settings)
stream = json.loads(ib["streamSettings"]) if isinstance(ib.get("streamSettings"), str) and ib["streamSettings"].strip() else (ib.get("streamSettings") or {})
stream["externalProxy"] = [{"dest": ip, "port": port}]
ib["streamSettings"] = json.dumps(stream)
print(json.dumps(ib))
PYEOF
UPD=$(xui_api POST "/inbounds/update/$AWG_ID" "$(cat /tmp/shvpn-awg-update.json)")
rm -f /tmp/shvpn-awg-update.json
echo "$UPD" | grep -q '"success":true' || die "обновление AWG-инбаунда: $UPD"
ok "kernelMode=$AWG_KERNEL, externalProxy=$SERVER_IP:$AWG_PORT"

# ----------------------------------------------------------------------------
step "Firewall: 443/udp"
if command -v ufw >/dev/null 2>&1; then
    ufw allow 443/udp >/dev/null
    ok "443/udp открыт"
else
    warn "ufw не найден — откройте 443/udp вручную"
fi

save_env "AWG_ID=$AWG_ID" "AWG_PORT=$AWG_PORT" "AWG_KERNEL=$AWG_KERNEL"
ok "AmneziaWG готов. Интерфейс поднимется при появлении первого клиента."
ok "Клиенты: /register в vps-admin-боте или Add Client в панели (инбаунд amneziawg)."

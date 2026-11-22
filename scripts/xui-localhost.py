#!/usr/bin/env python3
"""Переводит 3x-ui на localhost за реверс-прокси (схема shvpn).

Режимы:
  --read-only  только читает БД и печатает JSON с путями/портами/SNI (для рендера конфигов)
  (без флага)  вносит изменения (запускать при ОСТАНОВЛЕННОМ x-ui!)

Что делает:
  settings:  webListen/subListen = 127.0.0.1, убирает TLS-серты у панели и подписок
             (TLS терминирует nginx), прописывает публичные subURI/subClashURI/subJsonURI
  inbounds:  listen = 127.0.0.1; acceptProxyProtocol=true на транспорте (приём PROXY
             protocol v1/v2 от nginx); reality xver=0 (xver шлёт PROXY на цель, не клиенту!);
             externalProxy = [{dest: SERVER_IP, port: PUBLIC_PORT}] — ссылки/подписки
             будут указывать на внешний адрес:443, хотя Xray слушает локальный порт.

Печатает JSON: пути панели/подписок, порты, список SNI для stream-роутера.
"""
import json
import os
import sqlite3
import sys

DB = os.environ.get("XUI_DB", "/etc/x-ui/x-ui.db")
SERVER_IP = os.environ["SERVER_IP"]
PUBLIC_PORT = int(os.environ.get("PUBLIC_PORT", "443"))
READ_ONLY = "--read-only" in sys.argv


def get_setting(cur, key, default=""):
    row = cur.execute("SELECT value FROM settings WHERE key=?", (key,)).fetchone()
    return row[0] if row else default


def set_setting(cur, key, value):
    cur.execute("UPDATE settings SET value=? WHERE key=?", (value, key))
    if cur.rowcount == 0:
        cur.execute("INSERT INTO settings(key,value) VALUES(?,?)", (key, value))


def main():
    con = sqlite3.connect(DB)
    cur = con.cursor()

    panel_path = get_setting(cur, "webBasePath", "/")
    sub_path = get_setting(cur, "subPath", "/sub/")
    sub_json_path = get_setting(cur, "subJsonPath", "/json/")
    sub_clash_path = get_setting(cur, "subClashPath", "/clash/")
    panel_port = int(get_setting(cur, "webPort", "2053"))
    sub_port = int(get_setting(cur, "subPort", "2096"))

    sni_map, xray_ports, warnings = [], [], []
    rows = cur.execute(
        "SELECT id,remark,port,protocol,stream_settings,enable FROM inbounds"
    ).fetchall()
    updates = []
    for iid, remark, port, protocol, stream_raw, enable in rows:
        if not enable:
            continue
        # stream разбираем ДО ветвлений: AWG-ветка правит stream["externalProxy"],
        # а объявление ниже делало UnboundLocalError (или брало stream прошлого
        # инбаунда и портило externalProxy чужого транспорта).
        stream = json.loads(stream_raw) if stream_raw else {}
        if protocol == "amneziawg":
            # AWG — UDP, через SNI-роутер не маршрутизируется; слушает свой порт
            # напрямую (наружу открывается в ufw). Но ссылки/подписки панель
            # строит из Host-заголовка (localhost) —.externalProxy исправляет адрес.
            if not READ_ONLY:
                stream["externalProxy"] = [{"dest": SERVER_IP, "port": port}]
                updates.append((json.dumps(stream), None, iid))
            continue
        if protocol in ("tunnel", "dokodemo-door", "socks", "http", "mixed", "wireguard", "tuic", "hysteria"):
            warnings.append(f"inbound {iid} ({protocol}): не TCP/TLS — через SNI-роутер не маршрутизируется, пропущен")
            continue
        security = stream.get("security", "none")
        xray_ports.append(port)

        if security == "reality":
            rs = stream.setdefault("realitySettings", {})
            for name in rs.get("serverNames", []):
                if not any(m["sni"] == name for m in sni_map):
                    sni_map.append({"sni": name, "port": port})
            if not READ_ONLY:
                # ВАЖНО: xver — это отправка PROXY-заголовка НА ЦЕЛЬ (google.com),
                # а не приём от клиента! Приём PROXY v1/v2 от nginx включает
                # acceptProxyProtocol на транспорте (оборачивает listener).
                rs["xver"] = 0
        elif security == "tls":
            ts = stream.get("tlsSettings", {})
            name = ts.get("serverName")
            if name and not any(m["sni"] == name for m in sni_map):
                sni_map.append({"sni": name, "port": port})
        else:
            warnings.append(f"inbound {iid} ({protocol}/{security}): без TLS — через :443 не маршрутизируется")
            continue

        if not READ_ONLY:
            net = stream.get("network", "tcp")
            key = {"tcp": "tcpSettings", "ws": "wsSettings", "httpupgrade": "httpupgradeSettings",
                   "grpc": "grpcSettings", "xhttp": "xhttpSettings"}.get(net)
            if key:
                stream.setdefault(key, {})["acceptProxyProtocol"] = True
            else:
                warnings.append(f"inbound {iid}: транспорт {net} — acceptProxyProtocol не настроен")

        if not READ_ONLY:
            # ссылки/подписки -> внешний IP:443 (Xray при этом слушает локальный порт).
            # forceTls="same" обязателен: иначе 3x-ui вырезает из ссылки
            # Reality-параметры (sni/pbk/sid/fp) — см. dropBaseRealityParams.
            stream["externalProxy"] = [{"dest": SERVER_IP, "port": PUBLIC_PORT, "forceTls": "same"}]
            updates.append((json.dumps(stream), "127.0.0.1", iid))

    if not READ_ONLY:
        for key, val in {
            "webListen": "127.0.0.1",
            "webCertFile": "",
            "webKeyFile": "",
            "subListen": "127.0.0.1",
            "subCertFile": "",
            "subKeyFile": "",
        }.items():
            set_setting(cur, key, val)
        base = f"https://{SERVER_IP}"
        if get_setting(cur, "subEnable") == "true":
            set_setting(cur, "subURI", base + sub_path)
        if get_setting(cur, "subClashEnable") == "true":
            set_setting(cur, "subClashURI", base + sub_clash_path)
        if get_setting(cur, "subJsonEnable") == "true":
            set_setting(cur, "subJsonURI", base + sub_json_path)
        for stream_json, listen, iid in updates:
            if listen is None:
                cur.execute("UPDATE inbounds SET stream_settings=? WHERE id=?", (stream_json, iid))
            else:
                cur.execute(
                    "UPDATE inbounds SET stream_settings=?, listen=? WHERE id=?",
                    (stream_json, listen, iid),
                )
        con.commit()
    con.close()

    print(json.dumps({
        "panel_path": panel_path, "sub_path": sub_path,
        "sub_json_path": sub_json_path, "sub_clash_path": sub_clash_path,
        "panel_port": panel_port, "sub_port": sub_port,
        "xray_ports": sorted(set(xray_ports)), "sni_map": sni_map,
        "warnings": warnings, "read_only": READ_ONLY,
    }, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()

# telemt WEB-прокси (новый тип прокси Telegram, Desktop 7.1+): MTProto внутри
# обычных HTTPS/WebSocket-запросов. Фрагмент ДОПИСЫВАЕТСЯ к /etc/telemt/telemt.toml
# установщиком, когда задан TG_WEB_DOMAIN. TLS терминирует nginx (vhost
# shvpn-telemt-web), сюда приходит plain HTTP/1.1 по loopback.
#
# ВАЖНО: WEB-режим НЕ поддерживает ee-секреты (FakeTLS) — только plain/dd.
# Используем dd с тем же 16-байтным секретом пользователя, что и для MTProto.
# Профили [[web.vhosts.profiles]] генерирует установщик по [access.users].

# Отдельный WEB-листенер: plain HTTP, БЕЗ PROXY-протокола (реальный IP клиента
# nginx передаёт заголовком X-Forwarded-For). Нельзя reuse_allow/client_mss.
[[server.listeners]]
ip = "127.0.0.1"
port = __TELEMT_WEB_PORT__
transport = "web"
proxy_protocol = false
reuse_allow = false
web_client_ip_source = "x_forwarded_for"
web_trusted_proxy_cidrs = ["127.0.0.1/32", "::1/128"]

[web]
enabled = true
# https — единственный carrier, который понимают и Desktop, и iOS. HTTPS-запросы
# с long-polling; снаружи неотличимо от обычного сайта. (https-lanes/websocket
# быстрее, но iOS их не поддерживает — оставлено на будущее.)
carrier = "https"

# Выделенный домен (TG_WEB_DOMAIN): весь vhost отдаётся WEB-прокси. Запросы без
# аутентифицированного carrier уходят на сайт-прикрытие (decoy). SNI этого домена
# в stream-карте попадает в `default web` -> nginx терминирует TLS здесь.
[[web.vhosts]]
host = "__TG_WEB_DOMAIN__"
base_path = ""
# Публичный endpoint, который видит клиент (nginx :443), а не локальный листенер.
public_addr = "__SERVER_IP__:443"

# Сайт-прикрытие для не-WebRequest'ов: immutable-снимок /var/www/shvpn.
[web.vhosts.decoy]
mode = "static_directory"
directory = "/var/www/shvpn"
index = "index.html"
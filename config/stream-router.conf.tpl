# nginx stream: единственный публичный порт 443.
# Читает SNI из TLS ClientHello (ssl_preread) и маршрутизирует TCP-поток
# БЕЗ терминации TLS и без лимитов на объём/длительность (чистый L4-релей).
# Во все апстримы отправляется PROXY protocol v1 (proxy_protocol on).
# Файл кладётся в /etc/nginx/stream.d/, подключается из nginx.conf (wildcard).
# SNI-карта и апстримы Xray — в shvpn-sni-map.conf (рядом, тот же wildcard;
# управляется vps-admin). Явный include НЕ нужен — будет дубль.

upstream telemt {
    server 127.0.0.1:__TELEMT_PORT__;
}

upstream web {
    server 127.0.0.1:__WEB_PORT__;
}

server {
    listen 443;
    listen [::]:443;

    ssl_preread on;
    proxy_pass $shvpn_backend;

    proxy_protocol on;
    proxy_connect_timeout 5s;
    proxy_timeout 24h;
    proxy_socket_keepalive on;
}

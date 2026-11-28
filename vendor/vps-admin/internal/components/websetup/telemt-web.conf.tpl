# nginx HTTPS-vhost для telemt WEB-прокси (Telegram Desktop 7.1+).
# Рендерится установщиком ТОЛЬКО когда задан TG_WEB_DOMAIN.
# Слушает тот же loopback-порт, что и shvpn-web (__WEB_PORT__); выбирается по
# SNI/Host = TG_WEB_DOMAIN (точное совпадение бьёт catch-all `server_name _`).
# TLS терминируется здесь (серт на TG_WEB_DOMAIN), в telemt уходит plain HTTP/1.1.
# Клиентский IP: stream-роутер шлёт PROXY v1 -> real_ip_header -> $remote_addr,
# который отдаём telemt заголовком X-Forwarded-For (РОВНО один адрес, не append).

map $http_upgrade $telemt_web_connection_upgrade {
    default upgrade;
    ''      '';
}

server {
    listen 127.0.0.1:__WEB_PORT__ ssl proxy_protocol;
    server_name __TG_WEB_DOMAIN__;

    set_real_ip_from 127.0.0.1;
    real_ip_header proxy_protocol;

    ssl_certificate     __CERT_DIR_WEB__/fullchain.pem;
    ssl_certificate_key __CERT_DIR_WEB__/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_prefer_server_ciphers off;

    # Carrier-credentials (bridge capability, bearer) не должны попадать в логи.
    access_log off;

    # >= web.limits.max_body_bytes (дефолт 2 MiB).
    client_max_body_size 2m;

    # Весь vhost отдаём telemt WEB (base_path пуст). Нельзя разбивать только
    # carrier-эндпоинты: обычный и аутентифицированный трафик должен идти одним
    # маршрутом, иначе обходится decoy-политика telemt.
    location / {
        proxy_pass http://127.0.0.1:__TELEMT_WEB_PORT__;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        # Перезаписываем (не append): telemt принимает ровно один IP.
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $telemt_web_connection_upgrade;

        proxy_connect_timeout 5s;
        # > long-poll (25s) и > 2x WebSocket-liveness; 65s покрывает дефолты.
        proxy_send_timeout 65s;
        proxy_read_timeout 65s;
        # Стримим тело и ответ (long-poll/downlink), без буферизации и ретраев:
        # bridge сам делает byte-identical повторы, WebSocket не переигрывается.
        proxy_request_buffering off;
        proxy_buffering off;
        proxy_next_upstream off;
    }
}
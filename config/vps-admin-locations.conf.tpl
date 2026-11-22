# vps-admin (127.0.0.1:__VPSADMIN_PORT__) — маршрутизация через единый :443.
# Панель использует абсолютные пути (/api/..., /p/...), поэтому:
#   - страница панели живёт на секретном пути __VPSADMIN_PATH__ (префикс срезается);
#   - /api/* проксируется ТОЛЬКО при наличии cookie vps_session (сканеры видят 404);
#   - /api/login, /api/qr и /p/* открыты (вход, QR для публичных страниц,
#     клиентские ссылки с ключами);
#   - PWA-статика за cookie (без входа панель не обнаруживается).
# Подключается внутри server-блока shvpn-web: include /etc/nginx/snippets/vps-admin-*.conf;
# Зона limit_req vpsadmin_login объявлена в web.conf.tpl (http-контекст).

# страница панели (секретный путь, префикс срезается trailing slash)
location __VPSADMIN_PATH__ {
    proxy_pass http://127.0.0.1:__VPSADMIN_PORT__/;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto https;
    proxy_read_timeout 300s;
}

# вход: открытый, с rate-limit (backend-лимитер за прокси видит всех как 127.0.0.1)
location = /api/login {
    limit_req zone=vpsadmin_login burst=5 nodelay;
    proxy_pass http://127.0.0.1:__VPSADMIN_PORT__;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto https;
}

# QR-генератор: открытый (используется публичными страницами /p/)
location = /api/qr {
    proxy_pass http://127.0.0.1:__VPSADMIN_PORT__;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto https;
}

# публичные клиентские страницы с ключами (доступ по токену в пути)
location /p/ {
    proxy_pass http://127.0.0.1:__VPSADMIN_PORT__;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto https;
}

# остальной API — только с session-cookie
location /api/ {
    if ($cookie_vps_session = "") { return 404; }
    proxy_pass http://127.0.0.1:__VPSADMIN_PORT__;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto https;
    proxy_read_timeout 300s;
}

# PWA-статика — за cookie
location = /manifest.webmanifest {
    if ($cookie_vps_session = "") { return 404; }
    proxy_pass http://127.0.0.1:__VPSADMIN_PORT__;
    proxy_set_header Host $host;
}
location = /sw.js {
    if ($cookie_vps_session = "") { return 404; }
    proxy_pass http://127.0.0.1:__VPSADMIN_PORT__;
    proxy_set_header Host $host;
}
location = /icon-192.png {
    if ($cookie_vps_session = "") { return 404; }
    proxy_pass http://127.0.0.1:__VPSADMIN_PORT__;
    proxy_set_header Host $host;
}
location = /icon-512.png {
    if ($cookie_vps_session = "") { return 404; }
    proxy_pass http://127.0.0.1:__VPSADMIN_PORT__;
    proxy_set_header Host $host;
}

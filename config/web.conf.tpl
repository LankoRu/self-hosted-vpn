# nginx HTTPS-бэкенд (только localhost): сайт-заглушка + панель 3x-ui + подписки.
# Принимает PROXY protocol v1 от stream-роутера -> реальные IP клиентов.
# Сертификат: Let's Encrypt на IP-адрес сервера (acme.sh, короткоживущий).
# Кладётся в /etc/nginx/sites-available/shvpn-web, симлинк в sites-enabled.

map $http_upgrade $shvpn_connection_upgrade {
    default upgrade;
    ''      close;
}

# rate-limit входа vps-admin (backend-лимитер за прокси видит всех как 127.0.0.1)
limit_req_zone $binary_remote_addr zone=vpsadmin_login:10m rate=5r/m;

server {
    listen 127.0.0.1:__WEB_PORT__ ssl proxy_protocol;
    server_name _;

    set_real_ip_from 127.0.0.1;
    real_ip_header proxy_protocol;

    ssl_certificate     __CERT_DIR__/fullchain.pem;
    ssl_certificate_key __CERT_DIR__/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_prefer_server_ciphers off;

    root /var/www/shvpn;
    index index.html;

    # --- панель 3x-ui (HTTP на localhost, TLS терминируется здесь) ---
    location __PANEL_PATH__ {
        proxy_pass http://127.0.0.1:__PANEL_PORT__;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $shvpn_connection_upgrade;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }

    # --- подписки 3x-ui (raw / json / clash) ---
    location __SUB_PATH__ {
        proxy_pass http://127.0.0.1:__SUB_PORT__;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        proxy_read_timeout 300s;
    }
    location __SUB_JSON_PATH__ {
        proxy_pass http://127.0.0.1:__SUB_PORT__;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        proxy_read_timeout 300s;
    }
    location __SUB_CLASH_PATH__ {
        proxy_pass http://127.0.0.1:__SUB_PORT__;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        proxy_read_timeout 300s;
    }

    # --- vps-admin (если развёрнут; wildcard-include терпит отсутствие файла) ---
    include /etc/nginx/snippets/vps-admin-*.conf;

    # --- сайт-заглушка ---
    location / {
        try_files $uri $uri/ =404;
    }
}

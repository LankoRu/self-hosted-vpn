# telemt — MTProto-прокси для Telegram (FakeTLS).
# Слушает ТОЛЬКО localhost; наружу его выводит nginx stream на :443.
# Значения-плейсхолдеры подставляет install.sh.

[general]
use_middle_proxy = true
log_level = "normal"

[general.modes]
classic = false
# dd (обфускация без TLS-маскировки) — запасной канал: не зависит от
# домена-маски, но легче детектируется DPI. Основной — ee (FakeTLS).
secure = true
tls = true

[general.links]
show = "*"
# Адрес/порт, которые подставляются в tg:// ссылки (внешние, а не локальные):
public_host = "__SERVER_IP__"
public_port = 443

[server]
port = __TELEMT_PORT__
# nginx stream передаёт PROXY protocol v1 — telemt видит реальные IP клиентов:
proxy_protocol = true
proxy_protocol_trusted_cidrs = ["127.0.0.1/32", "::1/128"]

[server.api]
enabled = true
listen = "127.0.0.1:9091"
whitelist = ["127.0.0.1/32", "::1/128"]

[[server.listeners]]
ip = "127.0.0.1"

[censorship]
# Домен-маска для Telegram FakeTLS (SNI в ClientHello).
# Ссылки вида ee<secret><hex(domain)> генерируются под этот домен.
tls_domain = "__TG_DOMAIN__"

# Невалидные MTProto-клиенты (сканеры, браузеры с этим SNI) уходят
# на НАСТОЯЩИЙ сайт домена-маски — сертификат совпадает с SNI.
mask = true
mask_host = "__TG_DOMAIN__"
mask_port = 443
# Лимиты mask-релея (для decoy-трафика; на Telegram-клиентов не действуют):
mask_relay_max_bytes = 67108864
mask_relay_timeout_ms = 300000
mask_relay_idle_timeout_ms = 30000

tls_emulation = true
tls_front_dir = "tlsfront"

# Файловый лог с ротацией. INFO пишется на каждое соединение: на эталоне это
# дало 457 МБ syslog и 756 МБ journald за сутки. 50 МБ × 3 файла — с запасом.
[logging]
destination = "file"
path = "/var/log/telemt/telemt.log"
rotation = "daily"
max_size_bytes = 52428800
max_files = 3
max_age_secs = 604800

[access.users]
# формат: "имя" = "32 hex-символа секрета"
hello = "__TG_SECRET__"

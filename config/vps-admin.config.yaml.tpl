# vps-admin — конфиг для схемы shvpn (рендерится install.sh).
db_path: "/var/lib/vps-admin/stats.db"
log_path: "/var/log/vps-admin/vps-admin.log"
poll_interval: "5m"

telegram:
  token: "__BOT_TOKEN__"
  chat_id: __BOT_CHAT__
  admins: []
  weekly_report: true
  weekly_cron: "0 10 * * 1"

services:
  xray:
    enabled: true
    provision: true
  amneziawg:
    enabled: __AWG_ENABLED__
    provision: true
  telemt:
    enabled: true
    provision: true

xray_3xui:
  base_url: "http://127.0.0.1:__XUI_PORT__"
  web_base_path: "__XUI_PATH__"
  api_token: "__XUI_TOKEN__"
  inbound_id: __VLESS_ID__
  sub_url: "https://__SERVER_IP____SUB_PATH__"
  public_host: "__SERVER_IP__"

amneziawg_3xui:
  base_url: "http://127.0.0.1:__XUI_PORT__"
  web_base_path: "__XUI_PATH__"
  api_token: "__XUI_TOKEN__"
  inbound_id: __AWG_ID__
  public_host: "__SERVER_IP__"

telemt:
  base_url: "http://127.0.0.1:9091"
  admin_key: ""

counter_reset:
  threshold_percent: 5.0

# Обновления из панели («Система» → «Компоненты»).
update:
  github_repo: "LankoRu/self-hosted-vpn"  # релизы vps-admin (источник self-update)
  github_token: ""                        # PAT для приватного репо (Contents: Read)
  check_interval: "1h"                    # как часто проверять версии/пакеты

prometheus:
  port: 2112
  enabled: true

web:
  enabled: true
  listen: "127.0.0.1:__VPSADMIN_PORT__"
  login: "admin"
  password: "__WEBPASS__"
  tls:
    enabled: false

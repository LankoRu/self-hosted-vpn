# SNI-карта shvpn + апстримы Xray.
# ВНИМАНИЕ: файл управляется vps-admin (раздел «Подключения»: ротация
# Reality-SNI, смена домена telemt, запасной транспорт). При ручной правке
# учтите — vps-admin перегенерирует его из состояния 3x-ui + telemt.
# Подключается из shvpn-router.conf (include).

map $ssl_preread_server_name $shvpn_backend {
    # Telegram FakeTLS-домен -> telemt
    __TG_DOMAIN__ telemt;

    # SNI из realitySettings.serverNames VLESS-инбаундов -> Xray
    # (по инбаунду на транспорт: основной Reality-TCP, запасной XHTTP/gRPC)
__REALITY_SNI_MAP__

    # всё остальное (без SNI, прямой доступ по IP, чужие SNI) -> сайт/панель
    default web;
}

# апстримы Xray — по одному на каждый локальный порт инбаундов
__XRAY_UPSTREAMS__

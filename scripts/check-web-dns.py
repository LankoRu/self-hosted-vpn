#!/usr/bin/env python3
"""check-web-dns.py — проверяет, что A-запись домена указывает на IP сервера.

Использование: check-web-dns.py <domain> <server-ip>
Код возврата 0 = совпадает; 1 = не совпадает / не резолвится. Ретраи учитывают
propagation DNS (например DuckDNS). Нужно перед выпуском LE-сертификата (HTTP-01).
"""
import socket
import sys
import time


def main() -> int:
    host, want = sys.argv[1], sys.argv[2]
    for _ in range(6):
        try:
            ips = {ai[4][0] for ai in socket.getaddrinfo(host, 443, socket.AF_INET)}
        except socket.gaierror:
            time.sleep(5)  # propagation DNS может занять секунды
            continue
        if want in ips:
            print(f"  DNS: {host} -> {want}")
            return 0
        print(f"DNS: {host} -> {sorted(ips)}, ожидался {want}", file=sys.stderr)
        return 1
    print(f"DNS: {host} не резолвится (проверьте A-запись у регистратора/DuckDNS)",
          file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
#!/usr/bin/env python3
"""Патч amneziawg kernel module против Ubuntu-бэкпорта API ядра 7.0.

Upstream-коммит amnezia-vpn/amneziawg-linux-kernel-module 70ae2481
«compat: cover ubuntu backport of setup_udp_tunnel_sock» (2026-11-13):
Ubuntu 7.0.0-43+ (ABI>=38) принимает struct sock* в setup_udp_tunnel_sock,
а модуль по версии ядра (<7.1.5) конвертирует sk->sk_socket → ошибка сборки.
Патчим ОБЕ копии: DKMS-исходник и клон установщика. Идемпотентно.
"""
import glob
import sys

OLD = "#define setup_udp_tunnel_sock(net, sk, sock_cfg) setup_udp_tunnel_sock(net, sk->sk_socket, sock_cfg)"
GUARD = ("#if !(defined(UTS_UBUNTU_RELEASE_ABI) && LINUX_VERSION_CODE >= KERNEL_VERSION(7, 0, 0) "
         "&& UTS_UBUNTU_RELEASE_ABI >= 38)")
NEW = GUARD + "\n" + OLD + "\n#endif"

paths = (glob.glob("/usr/src/amneziawg-*/**/compat.h", recursive=True)
         + glob.glob("/usr/local/src/amneziawg-linux-kernel-module/**/compat.h", recursive=True))
if not paths:
    sys.exit("compat.h не найден — модуль ещё не клонирован?")
patched = 0
for p in paths:
    s = open(p).read()
    if NEW in s:
        print("уже пропатчен:", p)
        continue
    if OLD not in s:
        print("не найден целевой блок (другая версия модуля?):", p)
        continue
    open(p, "w").write(s.replace(OLD, NEW))
    print("пропатчен:", p)
    patched += 1
print(f"готово, пропатчено файлов: {patched}")

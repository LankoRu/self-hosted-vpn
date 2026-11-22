# Патч: amneziawg kernel module vs Ubuntu 7.0 (backport API)

## Проблема

`sudo /usr/local/x-ui/x-ui install-awg-kernel` падает на Ubuntu 26.04
(ядро 7.0.0-43-generic):

```
socket.c: error: passing argument 2 of 'setup_udp_tunnel_sock'
from incompatible pointer type
```

Причина: модуль (pinned tag v3.1.20260906) считает, что новый API
`setup_udp_tunnel_sock(struct sock *)` появился в mainline 7.1.5, и для
ядер «старше» конвертирует `sk->sk_socket`. Ubuntu бэкпортировала новый
API уже в 7.0.0-43 (ABI ≥ 38) — проверка по версии обманывается.
При этом `udp_tunnel_sock_release` в Ubuntu 7.0 всё ещё принимает
`struct socket *` — конвертация для него нужна.

## Фикс

Upstream-коммит amnezia-vpn/amneziawg-linux-kernel-module
**70ae2481** «compat: cover ubuntu backport of setup_udp_tunnel_sock»
(2026-11-13). В `compat/compat.h`:

```c
#if LINUX_VERSION_CODE < KERNEL_VERSION(7, 1, 5)
#include <net/udp_tunnel.h>
#if !(defined(UTS_UBUNTU_RELEASE_ABI) && LINUX_VERSION_CODE >= KERNEL_VERSION(7, 0, 0) && UTS_UBUNTU_RELEASE_ABI >= 38)
#define setup_udp_tunnel_sock(net, sk, sock_cfg) setup_udp_tunnel_sock(net, sk->sk_socket, sock_cfg)
#endif
#define udp_tunnel_sock_release(sk) udp_tunnel_sock_release(sk->sk_socket)
#endif
```

## Применение на сервере (если install-awg-kernel упал)

```bash
# 1. пропатчить ОБЕ копии (DKMS-исходник и клон установщика)
python3 - <<'EOF'
import glob
OLD = '#define setup_udp_tunnel_sock(net, sk, sock_cfg) setup_udp_tunnel_sock(net, sk->sk_socket, sock_cfg)'
NEW = ('#if !(defined(UTS_UBUNTU_RELEASE_ABI) && LINUX_VERSION_CODE >= KERNEL_VERSION(7, 0, 0) && UTS_UBUNTU_RELEASE_ABI >= 38)\n'
       + OLD + '\n#endif')
for p in glob.glob('/usr/src/amneziawg-1.0.0/**/compat.h', recursive=True) + \
         glob.glob('/usr/local/src/amneziawg-linux-kernel-module/**/compat.h', recursive=True):
    s = open(p).read()
    if NEW in s: print('уже:', p); continue
    assert OLD in s, p
    open(p, 'w').write(s.replace(OLD, NEW))
    print('пропатчен:', p)
EOF

# 2. пересобрать и загрузить
dkms build -m amneziawg -v 1.0.0 --force
dkms install -m amneziawg -v 1.0.0 --force
modprobe amneziawg
cat /sys/module/amneziawg/version   # ожидаем 3.1.x

# 3. tools (скрипт падает до их установки)
cd /usr/local/src/amneziawg-tools/src && make && make install
awg --version                        # v3.1.x
```

Дальше в панели 3x-ui включить **Kernel mode** у AmneziaWG-инбаунда.

## Примечания

- DKMS сам пересоберёт модуль при обновлении ядра — патч живёт в
  `/usr/src/amneziawg-1.0.0` и сохраняется.
- **Повторный запуск `x-ui install-awg-kernel` перезатрёт патч**
  (скрипт клонирует pinned tag заново). После обновления 3x-ui, если
  модуль снова не собирается — применить патч повторно (или дождаться,
  пока 3x-ui запинит tag новее 2026-11-13).
- Secure Boot на VPS выключен (модуль не подписан, modprobe прошёл).
  На хостах с Secure Boot модуль нужно подписывать.
- Проверено: Ubuntu 26.04, ядро 7.0.0-43-generic,
  2026-11-21: модуль 3.1.20260812 + tools v3.1.20260812, сборка успешна.

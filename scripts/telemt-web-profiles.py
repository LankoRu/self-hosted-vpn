#!/usr/bin/env python3
"""telemt-web-profiles.py — дописывает WEB-фрагмент к /etc/telemt/telemt.toml и
генерирует [[web.vhosts.profiles]] (secret_mode="dd") для каждого пользователя
[access.users]. Вызывается установщиком, когда задан TG_WEB_DOMAIN.

Использование: telemt-web-profiles.py <telemt.toml> <web-fragment.toml>

Рендер telemt.toml идёт с нуля каждый прогон (render_tpl перезаписывает файл),
поэтому фрагмент и профили добавляются заново — дублей не бывает. WEB-режим
telemt требует непустой profiles при enabled=true, поэтому без пользователей
это ошибка.
"""
import re
import sys


def main() -> int:
    cfg_path, frag_path = sys.argv[1], sys.argv[2]
    s = open(cfg_path, encoding="utf-8").read()
    frag = open(frag_path, encoding="utf-8").read()

    # Пользователи — только из секции [access.users] (формат name = "32 hex").
    m = re.search(r"\[access\.users\]\n(.*?)(?=\n\[|\Z)", s, re.S)
    users = []
    if m:
        users = re.findall(r'^([A-Za-z0-9_-]+)\s*=\s*"[0-9a-f]{32}"', m.group(1), re.M)
    if not users:
        print("WEB-прокси: в [access.users] нет пользователей (нужен хотя бы один профиль)",
              file=sys.stderr)
        return 1

    profiles = "\n".join(
        f'[[web.vhosts.profiles]]\nuser = "{u}"\nsecret_mode = "dd"\n' for u in users
    )
    out = s.rstrip() + "\n\n" + frag.rstrip() + "\n\n" + profiles
    open(cfg_path, "w", encoding="utf-8").write(out)
    print(f"  WEB-прокси: {len(users)} профилей (dd) добавлено")
    return 0


if __name__ == "__main__":
    sys.exit(main())
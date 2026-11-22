#!/usr/bin/env bash
# lib_test.sh — регрессионные проверки хелперов установщика (ask/ask_yn/ask_secret).
# Запуск: bash scripts/lib_test.sh
set -uo pipefail
cd "$(dirname "$0")" || exit 1
# shellcheck source=lib.sh
source ./lib.sh

fails=0
check() { # check "описание" ожидание факт
    if [ "$2" = "$3" ]; then
        printf '  ✓ %s\n' "$1"
    else
        printf '  ✗ %s: ожидалось [%s], получено [%s]\n' "$1" "$2" "$3" >&2
        fails=$((fails + 1))
    fi
}

# --yes: значения = дефолты, перевод в 0/1 без опроса
ASSUME_YES=1; export ASSUME_YES
ask_yn A "3x-ui?" "y";      check "ask_yn --yes дефолт y → 1" 1 "$A"
ask_yn B "AWG?" "n";        check "ask_yn --yes дефолт n → 0" 0 "$B"
ask C "домен" "example.com"; check "ask --yes → дефолт" "example.com" "$C"
ask_secret D "секрет" "s3cr3t"; check "ask_secret --yes → дефолт" "s3cr3t" "$D"

# Баг-регрессия: ask() не должен затирать переменную вызывающего из-за
# динамического связывания имён (ask_yn всегда отвечал «нет»).
ask_yn E "?" "y"; check "ask_yn не теряет ответ (динамическое связывание)" 1 "$E"

# Интерактивный разбор ответов на y/n (без /dev/tty — подменяем ask)
unset ASSUME_YES
for pair in "y:1" "Y:1" "yes:1" "да:1" "n:0" "no:0" "x:0"; do
    ans="${pair%%:*}"; want="${pair##*:}"
    got=0
    case "$ans" in y|Y|yes|YES|да|Да|ДА) got=1 ;; esac
    check "ответ '$ans' → $want" "$want" "$got"
done

# Рендер шаблона: соседние плейсхолдеры не должны склеиваться в одно имя
tmpdir=$(mktemp -d)
printf 'sub_url: "https://__SERVER_IP____SUB_PATH__"\nhost: "__SERVER_IP__"\n' > "$tmpdir/t.tpl"
SERVER_IP=1.2.3.4 SUB_PATH=/sub render_tpl "$tmpdir/t.tpl" "$tmpdir/out" >/dev/null
expected='sub_url: "https://1.2.3.4/sub"'
got=$(head -1 "$tmpdir/out")
check "render_tpl: соседние плейсхолдеры" "$expected" "$got"
rm -rf "$tmpdir"

if [ "$fails" -ne 0 ]; then
    printf 'ПРОВАЛЕНО: %d\n' "$fails" >&2
    exit 1
fi
printf 'Все проверки lib.sh пройдены\n'

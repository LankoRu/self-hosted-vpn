// Package util — небольшие помощники без внешних зависимостей.
package util

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// UUIDv4 генерирует случайный UUID версии 4.
func UUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // версия 4
	b[8] = (b[8] & 0x3f) | 0x80 // вариант
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// RandHex возвращает n случайных байт в hex-представлении (2n символов).
func RandHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// translitMap — транслитерация кириллицы (русский + украинские буквы) в латиницу
// для генерации логинов из отображаемых имён.
var translitMap = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
	'і': "i", 'ї': "yi", 'є': "ye", 'ґ': "g",
}

// UsernameFromDisplay генерирует логин из отображаемого имени: транслитерация,
// нижний регистр, пробелы/разделители → '-', допустимы только [a-z0-9_.-].
// Уникальность НЕ гарантирует — вызывающий (manager) добавляет суффикс при
// коллизии. Пустой/слишком короткий результат → "user".
func UsernameFromDisplay(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		var piece string
		if t, ok := translitMap[r]; ok {
			piece = t
		} else if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' {
			piece = string(r)
		} else {
			piece = "-" // пробелы и любые прочие символы — в разделитель
		}
		if piece == "-" {
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
			}
			prevDash = true
			continue
		}
		b.WriteString(piece)
		prevDash = false
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 36 { // запас под суффикс уникальности (лимит логина 40)
		s = strings.TrimRight(s[:36], "-")
	}
	if len(s) < 2 {
		return "user"
	}
	return s
}

// Package nginxmap регенерирует SNI-карту nginx stream (shvpn-sni-map.conf):
// какой SNI на какой локальный апстрим уходит. Файл управляется vps-admin
// (раздел «Подключения»: ротация Reality-SNI, смена домена telemt, запасной
// транспорт). Установка создаёт его из шаблона, дальше владелец — vps-admin.
package nginxmap

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// DefaultPath — путь карты на сервере (рядом с shvpn-router.conf; nginx.conf
// включает stream.d/*.conf wildcard — явный include не нужен, будет дубль).
const DefaultPath = "/etc/nginx/stream.d/shvpn-sni-map.conf"

// Inbound — включённый xray-инбаунд: локальный порт и его Reality-SNI.
type Inbound struct {
	Port        int
	ServerNames []string
}

// State — полное состояние маршрутизации: домен telemt + все xray-инбаунды.
type State struct {
	TelemtDomain string
	Inbounds     []Inbound
}

// Render собирает содержимое карты. Валидирует коллизии: один SNI не может
// маршрутироваться в два места (telemt-домен и serverNames обязаны различаться).
func Render(s State) (string, error) {
	if strings.TrimSpace(s.TelemtDomain) == "" {
		return "", fmt.Errorf("не задан домен telemt")
	}
	seen := map[string]string{s.TelemtDomain: "telemt"}
	for _, ib := range s.Inbounds {
		if ib.Port <= 0 || ib.Port > 65535 {
			return "", fmt.Errorf("некорректный порт инбаунда: %d", ib.Port)
		}
		if len(ib.ServerNames) == 0 {
			return "", fmt.Errorf("инбаунд на порту %d без serverNames", ib.Port)
		}
		for _, sni := range ib.ServerNames {
			sni = strings.TrimSpace(sni)
			if sni == "" {
				return "", fmt.Errorf("пустой SNI в инбаунде на порту %d", ib.Port)
			}
			if owner, dup := seen[sni]; dup {
				return "", fmt.Errorf("SNI %q занят дважды (%s и xray_%d) — маршруты столкнутся", sni, owner, ib.Port)
			}
			seen[sni] = fmt.Sprintf("xray_%d", ib.Port)
		}
	}

	var b strings.Builder
	b.WriteString("# SNI-карта shvpn + апстримы Xray. Управляется vps-admin («Подключения»).\n")
	b.WriteString("# Не редактировать вручную: перегенерируется из состояния 3x-ui + telemt.\n\n")
	b.WriteString("map $ssl_preread_server_name $shvpn_backend {\n")
	b.WriteString("    # Telegram FakeTLS-домен -> telemt\n")
	fmt.Fprintf(&b, "    %s telemt;\n", s.TelemtDomain)
	b.WriteString("\n    # Reality-SNI VLESS-инбаундов -> Xray (по инбаунду на транспорт)\n")
	for _, ib := range s.Inbounds {
		for _, sni := range ib.ServerNames {
			fmt.Fprintf(&b, "    %s xray_%d;\n", strings.TrimSpace(sni), ib.Port)
		}
	}
	b.WriteString("\n    # всё остальное -> сайт-прикрытие/панели\n    default web;\n}\n")

	ports := map[int]bool{}
	for _, ib := range s.Inbounds {
		if ports[ib.Port] {
			continue
		}
		ports[ib.Port] = true
		fmt.Fprintf(&b, "\nupstream xray_%d {\n    server 127.0.0.1:%d;\n}\n", ib.Port, ib.Port)
	}
	return b.String(), nil
}

// Apply атомарно записывает карту и перезагружает nginx. При ошибке reload
// откатывает прежнее содержимое (nginx продолжает работать на старом конфиге).
func Apply(path string, s State, reload func() error) error {
	content, err := Render(s)
	if err != nil {
		return err
	}
	old, readErr := os.ReadFile(path)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("запись карты: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("замена карты: %w", err)
	}
	if err := reload(); err != nil {
		if readErr == nil {
			_ = os.WriteFile(path, old, 0o644)
		} else {
			_ = os.Remove(path)
		}
		return fmt.Errorf("nginx не принял новую карту (откат): %w", err)
	}
	return nil
}

// DefaultReload — nginx -t && systemctl reload nginx (боевой reload).
func DefaultReload() error {
	if out, err := exec.Command("nginx", "-t").CombinedOutput(); err != nil {
		return fmt.Errorf("nginx -t: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "reload", "nginx").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl reload nginx: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

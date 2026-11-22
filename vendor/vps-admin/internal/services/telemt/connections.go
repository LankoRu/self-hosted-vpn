package telemt

// connections.go — смена домена-маски FakeTLS из панели vps-admin и обновление
// ссылок без пересоздания учётки. Домен живёт в config.toml telemt
// ([censorship] tls_domain + mask_host); ссылки telemt генерирует сам, поэтому
// после смены домена достаточно перечитать их (RefreshItems).

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/LankoRu/vps-admin/internal/service"
)

var (
	reTLSDomain = regexp.MustCompile(`(?m)^tls_domain\s*=\s*"([^"]+)"`)
	reMaskHost  = regexp.MustCompile(`(?m)^(tls_domain|mask_host)\s*=\s*"[^"]*"`)
)

// Domain — текущий домен-маска (tls_domain из config.toml).
func (s *Service) Domain() (string, error) {
	b, err := os.ReadFile(s.cfg.ConfigPath)
	if err != nil {
		return "", fmt.Errorf("чтение %s: %w", s.cfg.ConfigPath, err)
	}
	m := reTLSDomain.FindSubmatch(b)
	if m == nil {
		return "", fmt.Errorf("в %s не найден tls_domain", s.cfg.ConfigPath)
	}
	return string(m[1]), nil
}

// SetDomain меняет tls_domain и mask_host, сохраняя остальной конфиг (включая
// [access.users] — секреты пользователей не меняются, меняется только домен в
// ссылках). Права/владелец файла сохраняются (telemt должен писать в него через
// свой API). После правки — restart telemt и ожидание active.
func (s *Service) SetDomain(domain string) error {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return fmt.Errorf("пустой домен")
	}
	path := s.cfg.ConfigPath
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("чтение %s: %w", path, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !reMaskHost.Match(b) {
		return fmt.Errorf("в %s не найдены tls_domain/mask_host", path)
	}
	nb := reMaskHost.ReplaceAll(b, []byte(`${1} = "`+domain+`"`))

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, nb, st.Mode().Perm()); err != nil {
		return fmt.Errorf("запись %s: %w", tmp, err)
	}
	// сохраняем владельца (telemt:telemt) — иначе API telemt потеряет доступ
	if uid, gid, ok := fileOwner(st); ok {
		_ = os.Chown(tmp, uid, gid)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("замена %s: %w", path, err)
	}

	return restartTelemt()
}

// restartTelemt — restart + ожидание active (в тестах подменяется).
var restartTelemt = func() error {
	if out, err := exec.Command("systemctl", "restart", "telemt").CombinedOutput(); err != nil {
		return fmt.Errorf("restart telemt: %v: %s", err, strings.TrimSpace(string(out)))
	}
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		if exec.Command("systemctl", "is-active", "--quiet", "telemt").Run() == nil {
			return nil
		}
	}
	return fmt.Errorf("telemt не стал active после смены домена (journalctl -u telemt)")
}

// RefreshItems — свежие ссылки существующего пользователя (домен мог смениться).
// Секрет дочитывается из [access.users] config.toml; ссылки — из API telemt.
func (s *Service) RefreshItems(username string) ([]service.Item, error) {
	links := s.userLinksFor(username)
	if links.empty() {
		return nil, fmt.Errorf("telemt не вернул ссылки для %q", username)
	}
	secret := ""
	if b, err := os.ReadFile(s.cfg.ConfigPath); err == nil {
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(username) + `\s*=\s*"([0-9a-fA-F]+)"`)
		if m := re.FindSubmatch(b); m != nil {
			secret = string(m[1])
		}
	}
	return buildItems(secret, links), nil
}

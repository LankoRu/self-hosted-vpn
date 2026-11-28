package components

// websetup.go — донастройка WEB-прокси Telegram из панели («Компоненты»).
//
// Кнопка «Обновить» меняет только бинарь vps-admin; сам WEB-прокси требует
// инфраструктуры: LE-серт на свой домен, telemt WEB-листенер + [web] + vhost +
// dd-профили, nginx-vhost. Эта кнопка делает всё это одним async-job'ом с живым
// прогрессом (переиспользует Runner/jobState/streamCmd). Логика повторяет
// scripts/deploy-web.sh установщика (точечная донастройка, без 3x-ui/apt).
//
// Шаблоны telemt/nginx встроены в бинарь (go:embed): исходные config/*.tpl живут
// вне Go-модуля, поэтому здесь их копии — держать синхронными с config/.

import (
	"context"
	"embed"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed websetup/telemt-web.toml.tpl websetup/telemt-web.conf.tpl
var websetupFS embed.FS

const webJobName = "web-proxy"

// webDomainRe — полный FQDN (как в установщике/telemt).
var webDomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// StartWebSetup запускает донастройку WEB-прокси для домена (async).
func (r *Runner) StartWebSetup(domain string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if err := validateWebDomain(domain, r.maskDomain(), r.serverIP()); err != nil {
		return err
	}
	r.mu.Lock()
	if j := r.jobs[webJobName]; j != nil && j.snapshot().State == "running" {
		r.mu.Unlock()
		return fmt.Errorf("донастройка WEB уже выполняется")
	}
	j := &jobState{state: "running", started: time.Now()}
	r.jobs[webJobName] = j
	r.mu.Unlock()

	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				j.write(fmt.Sprintf("ОШИБКА: паника при донастройке: %v\n", rec))
				j.finish("error")
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		j.write(">>> Донастройка WEB-прокси: " + domain + "\n")
		if err := setupWebProxy(ctx, r, j, domain); err != nil {
			j.write("ОШИБКА: " + err.Error() + "\n")
			j.finish("error")
			return
		}
		j.write(">>> Готово\n")
		j.finish("done")
		r.Refresh(context.Background())
	}()
	return nil
}

// setupWebProxy — оркестрация: DNS → серт → telemt → nginx → сохранение env.
func setupWebProxy(ctx context.Context, r *Runner, j *jobState, domain string) error {
	ip := r.serverIP()
	if ip == "" {
		return fmt.Errorf("не определён IP сервера (public_host в конфиге vps-admin)")
	}
	webPort := envValue("WEB_PORT", "8444")
	telemtWebPort := envValue("TELEMT_WEB_PORT", "18080")
	certDir := envValue("CERT_DIR_WEB", "/root/cert/web")

	j.write(">>> DNS\n")
	if err := checkWebDNS(domain, ip); err != nil {
		return err
	}
	j.write("DNS: " + domain + " -> " + ip + "\n")

	j.write(">>> Сертификат Let's Encrypt на " + domain + "\n")
	if err := issueWebCert(ctx, r, j, domain, certDir); err != nil {
		return err
	}

	j.write(">>> telemt: WEB-листенер + vhost + профили\n")
	if err := setupTelemtWeb(ctx, r, j, domain, ip, telemtWebPort); err != nil {
		return err
	}

	j.write(">>> nginx WEB-vhost\n")
	if err := setupNginxWeb(ctx, r, j, domain, webPort, certDir, telemtWebPort); err != nil {
		return err
	}

	if err := saveEnvFile("TG_WEB_DOMAIN="+domain, "TELEMT_WEB_PORT="+telemtWebPort, "CERT_DIR_WEB="+certDir); err != nil {
		return fmt.Errorf("сохранение /etc/shvpn/env.sh: %w", err)
	}
	j.write("Настройки сохранены в /etc/shvpn/env.sh\n")
	return nil
}

// --- чистые хелперы (тестируются) -------------------------------------------

// validateWebDomain — FQDN, не IP, не совпадает с доменом-маской/IP сервера.
func validateWebDomain(domain, mask, ip string) error {
	if domain == "" {
		return fmt.Errorf("не задан домен")
	}
	if domain == strings.ToLower(strings.TrimSpace(mask)) || domain == strings.ToLower(strings.TrimSpace(ip)) {
		return fmt.Errorf("WEB-домен не должен совпадать с доменом-маской FakeTLS или IP сервера")
	}
	if net.ParseIP(domain) != nil {
		return fmt.Errorf("WEB-домен должен быть FQDN, а не IP (telemt WEB не принимает IP)")
	}
	if !webDomainRe.MatchString(domain) {
		return fmt.Errorf("WEB-домен должен быть полным доменным именем (например tg.example.com)")
	}
	return nil
}

// renderWebTpl подставляет __KEY__ из vars.
func renderWebTpl(tpl string, vars map[string]string) string {
	for k, v := range vars {
		tpl = strings.ReplaceAll(tpl, "__"+k+"__", v)
	}
	return tpl
}

var (
	accessSectionRe = regexp.MustCompile(`(?s)\[access\.users\]\n(.*?)(?:\n\[|$)`)
	accessUserRe    = regexp.MustCompile(`(?m)^([A-Za-z0-9_-]+)\s*=\s*"[0-9a-f]{32}"`)
)

// extractAccessUsers — имена пользователей из секции [access.users].
func extractAccessUsers(cfg string) []string {
	m := accessSectionRe.FindStringSubmatch(cfg)
	if m == nil {
		return nil
	}
	var users []string
	for _, mm := range accessUserRe.FindAllStringSubmatch(m[1], -1) {
		users = append(users, mm[1])
	}
	return users
}

// buildTelemtWebConfig дописывает WEB-фрагмент и dd-профили к текущему конфигу
// (сохраняя tls_domain/пользователей/секреты). Как scripts/telemt-web-profiles.py.
func buildTelemtWebConfig(existing, fragment string) (string, error) {
	users := extractAccessUsers(existing)
	if len(users) == 0 {
		return "", fmt.Errorf("в [access.users] нет пользователей (нужен хотя бы один WEB-профиль)")
	}
	blocks := make([]string, len(users))
	for i, u := range users {
		blocks[i] = "[[web.vhosts.profiles]]\nuser = \"" + u + "\"\nsecret_mode = \"dd\"\n"
	}
	profiles := strings.Join(blocks, "\n")
	return strings.TrimRight(existing, "\n") + "\n\n" +
		strings.TrimRight(fragment, "\n") + "\n\n" + profiles, nil
}

// --- окружение / файлы ------------------------------------------------------

func (r *Runner) serverIP() string {
	if h := strings.TrimSpace(r.cfg.Xray3xUI.PublicHost); h != "" {
		return h
	}
	return strings.TrimSpace(r.cfg.AmneziaWG3xUI.PublicHost)
}

// maskDomain — tls_domain из telemt.toml (для проверки коллизии).
func (r *Runner) maskDomain() string {
	b, err := os.ReadFile(r.cfg.TeleMT.ConfigPath)
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`(?m)^tls_domain\s*=\s*"([^"]+)"`).FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// webDomain — текущий WEB-домен (env.sh, затем telemt.toml).
func (r *Runner) webDomain() string {
	if d := envValue("TG_WEB_DOMAIN", ""); d != "" {
		return d
	}
	b, err := os.ReadFile(r.cfg.TeleMT.ConfigPath)
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`(?s)\[\[web\.vhosts\]\].*?host\s*=\s*"([^"]+)"`).FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// envValue читает KEY из /etc/shvpn/env.sh (export KEY=VALUE), иначе default.
func envValue(key, def string) string {
	b, err := os.ReadFile("/etc/shvpn/env.sh")
	if err != nil {
		return def
	}
	re := regexp.MustCompile(`(?m)^export ` + regexp.QuoteMeta(key) + `=(.*)$`)
	m := re.FindSubmatch(b)
	if m == nil {
		return def
	}
	v := strings.TrimSpace(string(m[1]))
	v = strings.Trim(v, `"'`)
	if v == "" {
		return def
	}
	return v
}

// saveEnvFile дописывает KEY=VALUE в /etc/shvpn/env.sh (перезаписывая ключи).
func saveEnvFile(kvs ...string) error {
	const path = "/etc/shvpn/env.sh"
	b, _ := os.ReadFile(path)
	s := string(b)
	for _, kv := range kvs {
		key := strings.SplitN(kv, "=", 2)[0]
		re := regexp.MustCompile(`(?m)^export ` + regexp.QuoteMeta(key) + `=.*\n?`)
		s = re.ReplaceAllString(s, "")
		s = strings.TrimRight(s, "\n") + "\nexport " + kv + "\n"
	}
	return os.WriteFile(path, []byte(s), 0o600)
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/root"
}

// --- DNS --------------------------------------------------------------------

func checkWebDNS(domain, ip string) error {
	for attempt := 0; attempt < 6; attempt++ {
		addrs, err := net.LookupHost(domain)
		if err != nil {
			time.Sleep(5 * time.Second) // propagation DNS
			continue
		}
		for _, a := range addrs {
			if a == ip {
				return nil
			}
		}
		return fmt.Errorf("DNS: %s указывает на %v, ожидался %s (нужна A-запись)", domain, addrs, ip)
	}
	return fmt.Errorf("DNS: %s не резолвится (проверьте A-запись у регистратора/DuckDNS)", domain)
}

// --- сертификат -------------------------------------------------------------

func issueWebCert(ctx context.Context, r *Runner, j *jobState, domain, certDir string) error {
	acme := filepath.Join(homeDir(), ".acme.sh", "acme.sh")
	if _, err := os.Stat(acme); err != nil {
		return fmt.Errorf("acme.sh не найден (%s) — сначала выполните установщик", acme)
	}
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return err
	}
	fullchain := filepath.Join(certDir, "fullchain.pem")
	if _, err := os.Stat(fullchain); os.IsNotExist(err) {
		_ = r.streamCmd(ctx, j, "ufw", "allow", "80/tcp")
		// standalone занимает :80; гасим nginx только если он слушает :80.
		nginxStopped := false
		if out, _ := r.runOut(ctx, "ss", "-tln"); strings.Contains(out, ":80 ") {
			if r.cmd(ctx, "systemctl", "is-active", "--quiet", "nginx").Run() == nil {
				if err := r.streamCmd(ctx, j, "systemctl", "stop", "nginx"); err == nil {
					nginxStopped = true
				}
			}
		}
		issueErr := r.streamCmd(ctx, j, acme, "--issue", "--standalone", "-d", domain,
			"--ecc", "--server", "letsencrypt", "--force")
		if nginxStopped {
			_ = r.streamCmd(ctx, j, "systemctl", "start", "nginx")
		}
		_ = r.streamCmd(ctx, j, "ufw", "delete", "allow", "80/tcp")
		if issueErr != nil {
			return fmt.Errorf("выпуск WEB-сертификата не удался (нужен свободный :80 и A-запись %s)", domain)
		}
	}
	return r.streamCmd(ctx, j, acme, "--install-cert", "-d", domain, "--ecc",
		"--key-file", filepath.Join(certDir, "privkey.pem"),
		"--fullchain-file", fullchain,
		"--reloadcmd", "systemctl is-active --quiet nginx && systemctl reload nginx || true",
		"--pre-hook", "ufw allow 80/tcp",
		"--post-hook", "ufw delete allow 80/tcp")
}

// --- telemt -----------------------------------------------------------------

func setupTelemtWeb(ctx context.Context, r *Runner, j *jobState, domain, ip, telemtWebPort string) error {
	cfgPath := r.cfg.TeleMT.ConfigPath
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("чтение %s: %w", cfgPath, err)
	}
	existing := string(b)
	if regexp.MustCompile(`(?m)^\[web\]`).MatchString(existing) {
		return fmt.Errorf("WEB уже настроен в telemt.toml; для смены домена используйте install.sh --tg-web-domain")
	}
	fragBytes, err := websetupFS.ReadFile("websetup/telemt-web.toml.tpl")
	if err != nil {
		return err
	}
	fragment := renderWebTpl(string(fragBytes), map[string]string{
		"TELEMT_WEB_PORT": telemtWebPort,
		"TG_WEB_DOMAIN":   domain,
		"SERVER_IP":       ip,
	})
	newCfg, err := buildTelemtWebConfig(existing, fragment)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "shvpn-telemt-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(newCfg); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	// Валидация ДО остановки: healthcheck грузит конфиг (parse_source); этап
	// «config load failed» = невалиден. API-проба может не пройти — не ошибка.
	out, _ := r.runOut(ctx, "telemt", "healthcheck", tmp.Name())
	if strings.Contains(out, "config load failed") {
		return fmt.Errorf("новый telemt.toml не прошёл валидацию (старый telemt не остановлен): %s", out)
	}
	j.write("Конфиг валиден, применяю\n")
	if err := r.streamCmd(ctx, j, "systemctl", "stop", "telemt"); err != nil {
		return err
	}
	if err := r.streamCmd(ctx, j, "install", "-m", "660", tmp.Name(), cfgPath); err != nil {
		return err
	}
	_ = r.streamCmd(ctx, j, "chown", "telemt:telemt", "/etc/telemt", cfgPath)
	_ = r.streamCmd(ctx, j, "chmod", "770", "/etc/telemt")
	_ = r.streamCmd(ctx, j, "chmod", "660", cfgPath)
	if err := r.streamCmd(ctx, j, "systemctl", "start", "telemt"); err != nil {
		return err
	}
	time.Sleep(time.Second)
	if r.cmd(ctx, "systemctl", "is-active", "--quiet", "telemt").Run() != nil {
		return fmt.Errorf("telemt не стартовал (journalctl -u telemt)")
	}
	j.write("telemt с WEB-прокси активен\n")
	return nil
}

// --- nginx ------------------------------------------------------------------

func setupNginxWeb(ctx context.Context, r *Runner, j *jobState, domain, webPort, certDir, telemtWebPort string) error {
	tplBytes, err := websetupFS.ReadFile("websetup/telemt-web.conf.tpl")
	if err != nil {
		return err
	}
	conf := renderWebTpl(string(tplBytes), map[string]string{
		"WEB_PORT":        webPort,
		"TG_WEB_DOMAIN":   domain,
		"CERT_DIR_WEB":    certDir,
		"TELEMT_WEB_PORT": telemtWebPort,
	})
	const avail = "/etc/nginx/sites-available/shvpn-telemt-web"
	const enabled = "/etc/nginx/sites-enabled/shvpn-telemt-web"
	if err := os.WriteFile(avail, []byte(conf), 0o644); err != nil {
		return err
	}
	_ = os.Remove(enabled)
	if err := os.Symlink(avail, enabled); err != nil {
		return err
	}
	// shvpn-web обязан быть default_server (иначе WEB-vhost, грузясь раньше по
	// алфавиту, отдаст свой серт на запросы по IP и сломает сайт/панель).
	const webAvail = "/etc/nginx/sites-available/shvpn-web"
	if wb, err := os.ReadFile(webAvail); err == nil && !strings.Contains(string(wb), "default_server") {
		updated := strings.ReplaceAll(string(wb),
			"listen 127.0.0.1:"+webPort+" ssl proxy_protocol;",
			"listen 127.0.0.1:"+webPort+" ssl proxy_protocol default_server;")
		if err := os.WriteFile(webAvail, []byte(updated), 0o644); err != nil {
			return err
		}
	}
	if err := r.streamCmd(ctx, j, "nginx", "-t"); err != nil {
		return fmt.Errorf("nginx -t не прошёл")
	}
	return r.streamCmd(ctx, j, "systemctl", "reload", "nginx")
}

// --- статус для «Компонентов» -----------------------------------------------

func (r *Runner) webProxy() Component {
	c := Component{Name: webJobName, Title: "Telegram WEB-прокси (HTTPS)", Setup: "web"}
	ip := r.serverIP()
	if d := r.webDomain(); d != "" {
		c.WebDomain = d
		c.Version = d
		c.Note = "Настроен. Смена домена — install.sh --tg-web-domain."
	} else {
		c.Version = "не настроен"
		c.Note = "MTProto внутри HTTPS к своему домену (Telegram Desktop 7.1+). Нужен домен с A-записью на " + ip + "."
	}
	return c
}

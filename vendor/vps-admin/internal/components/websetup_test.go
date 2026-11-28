package components

import (
	"strings"
	"testing"
)

func TestValidateWebDomain(t *testing.T) {
	cases := []struct {
		domain, mask, ip string
		ok               bool
	}{
		{"tg.example.duckdns.org", "petrovich.ru", "1.2.3.4", true},
		{"m3s1178.duckdns.org", "petrovich.ru", "45.15.146.84", true},
		{"", "petrovich.ru", "1.2.3.4", false},             // пусто
		{"1.2.3.4", "petrovich.ru", "1.2.3.4", false},      // IP
		{"petrovich.ru", "petrovich.ru", "1.2.3.4", false}, // == маска
		{"1.2.3.4", "petrovich.ru", "9.9.9.9", false},      // IP (не совпадает с ip, но всё равно IP)
		{"nodots", "petrovich.ru", "1.2.3.4", false},       // не FQDN
		{"-bad.example.com", "petrovich.ru", "1.2.3.4", false},
	}
	for _, c := range cases {
		err := validateWebDomain(c.domain, c.mask, c.ip)
		if (err == nil) != c.ok {
			t.Errorf("validateWebDomain(%q,%q,%q) err=%v, want ok=%v", c.domain, c.mask, c.ip, err, c.ok)
		}
	}
}

func TestRenderWebTpl(t *testing.T) {
	got := renderWebTpl("port=__PORT__ host=__HOST__ ip=__IP__:443", map[string]string{
		"PORT": "18080", "HOST": "tg.example.com", "IP": "1.2.3.4",
	})
	want := "port=18080 host=tg.example.com ip=1.2.3.4:443"
	if got != want {
		t.Errorf("renderWebTpl = %q, want %q", got, want)
	}
}

const sampleCfg = `[censorship]
tls_domain = "petrovich.ru"

[access.users]
hello = "0123456789abcdef0123456789abcdef"
me = "aabbccdd00112233aabbccdd00112233"
`

func TestExtractAccessUsers(t *testing.T) {
	users := extractAccessUsers(sampleCfg)
	if len(users) != 2 || users[0] != "hello" || users[1] != "me" {
		t.Errorf("users = %v, want [hello me]", users)
	}
	if got := extractAccessUsers("[censorship]\ntls_domain=\"x\"\n"); len(got) != 0 {
		t.Errorf("без [access.users] должно быть пусто, got %v", got)
	}
}

func TestBuildTelemtWebConfig(t *testing.T) {
	fragment := "[[server.listeners]]\nport = __TELEMT_WEB_PORT__\ntransport = \"web\"\n\n[web]\nenabled = true\n\n[[web.vhosts]]\nhost = \"__TG_WEB_DOMAIN__\"\npublic_addr = \"__SERVER_IP__:443\"\n\n[web.vhosts.decoy]\nmode = \"static_directory\"\n"
	rendered := renderWebTpl(fragment, map[string]string{
		"TELEMT_WEB_PORT": "18080", "TG_WEB_DOMAIN": "tg.example.com", "SERVER_IP": "1.2.3.4",
	})
	out, err := buildTelemtWebConfig(sampleCfg, rendered)
	if err != nil {
		t.Fatalf("buildTelemtWebConfig: %v", err)
	}
	// Существующее сохранено.
	if !strings.Contains(out, `tls_domain = "petrovich.ru"`) ||
		!strings.Contains(out, `me = "aabbccdd00112233aabbccdd00112233"`) {
		t.Errorf("исходный конфиг повреждён:\n%s", out)
	}
	// WEB-блок и профили на обоих пользователей.
	if !strings.Contains(out, "[web]\nenabled = true") || !strings.Contains(out, `host = "tg.example.com"`) {
		t.Errorf("WEB-блок не добавлен:\n%s", out)
	}
	for _, u := range []string{"hello", "me"} {
		if !strings.Contains(out, "[[web.vhosts.profiles]]\nuser = \""+u+"\"\nsecret_mode = \"dd\"") {
			t.Errorf("нет dd-профиля %s:\n%s", u, out)
		}
	}
	// Без пользователей — ошибка.
	if _, err := buildTelemtWebConfig("[censorship]\ntls_domain=\"x\"\n", rendered); err == nil {
		t.Errorf("ожидалась ошибка при пустом [access.users]")
	}
}

func TestEmbeddedWebTemplates(t *testing.T) {
	for _, name := range []string{"websetup/telemt-web.toml.tpl", "websetup/telemt-web.conf.tpl"} {
		b, err := websetupFS.ReadFile(name)
		if err != nil || len(b) == 0 {
			t.Errorf("embedded %s: err=%v len=%d", name, err, len(b))
		}
	}
	// Ключевые плейсхолдеры на месте (рендер их подставляет).
	b, _ := websetupFS.ReadFile("websetup/telemt-web.toml.tpl")
	for _, ph := range []string{"__TELEMT_WEB_PORT__", "__TG_WEB_DOMAIN__", "__SERVER_IP__"} {
		if !strings.Contains(string(b), ph) {
			t.Errorf("в telemt-web.toml.tpl нет плейсхолдера %s", ph)
		}
	}
}

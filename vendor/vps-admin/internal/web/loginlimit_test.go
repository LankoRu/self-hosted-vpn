package web

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/manager"
	"github.com/LankoRu/vps-admin/internal/registry"
	"github.com/LankoRu/vps-admin/internal/store"
)

func TestLoginBruteForceBlocked(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetDefaults()
	cfg.Web.Password = "s3cret"

	st, err := store.New(filepath.Join(t.TempDir(), "bf.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(New(cfg, st, manager.New(st, registry.New(cfg, st))).Handler())
	t.Cleanup(ts.Close)

	post := func(pw string) int {
		r, err := http.Post(ts.URL+"/api/login", "application/json",
			strings.NewReader(`{"password":"`+pw+`"}`))
		if err != nil {
			t.Fatalf("POST login: %v", err)
		}
		defer r.Body.Close()
		return r.StatusCode
	}

	// Первые maxLoginFails неудач — обычный 401.
	for i := 0; i < maxLoginFails; i++ {
		if code := post("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("попытка %d: ждали 401, получили %d", i+1, code)
		}
	}
	// Дальше IP заблокирован — 429, и ДАЖЕ с верным паролем.
	if code := post("wrong"); code != http.StatusTooManyRequests {
		t.Errorf("после лимита ждали 429, получили %d", code)
	}
	if code := post("s3cret"); code != http.StatusTooManyRequests {
		t.Errorf("верный пароль при блокировке: ждали 429, получили %d", code)
	}
}

func TestLoginSuccessResetsLimiter(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < maxLoginFails-1; i++ {
		l.fail("1.2.3.4")
	}
	l.reset("1.2.3.4") // удачный вход обнуляет счётчик
	for i := 0; i < maxLoginFails-1; i++ {
		l.fail("1.2.3.4")
	}
	if d := l.blockedFor("1.2.3.4"); d > 0 {
		t.Errorf("после сброса не должно блокировать, осталось %v", d)
	}
	l.fail("1.2.3.4") // добираем до порога
	if d := l.blockedFor("1.2.3.4"); d <= 0 {
		t.Error("после превышения порога должна быть блокировка")
	}
	// Блокировка не должна задевать другие адреса.
	if d := l.blockedFor("5.6.7.8"); d > 0 {
		t.Error("заблокирован не тот IP")
	}
}

// Манифест и иконки обязаны отдаваться БЕЗ авторизации, иначе браузер (он тянет
// манифест без cookie) не предложит установку PWA.
func TestPWAAssetsPublic(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetDefaults()
	cfg.Web.Password = "s3cret"

	st, err := store.New(filepath.Join(t.TempDir(), "pwa.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(New(cfg, st, manager.New(st, registry.New(cfg, st))).Handler())
	t.Cleanup(ts.Close)

	for _, p := range []string{"/manifest.webmanifest", "/sw.js", "/icon-192.png", "/icon-512.png"} {
		r, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		if r.StatusCode != http.StatusOK {
			t.Errorf("%s без входа: status %d, ожидался 200", p, r.StatusCode)
		}
		if p == "/manifest.webmanifest" {
			if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "manifest+json") {
				t.Errorf("манифест отдан с Content-Type %q — браузер его не примет", ct)
			}
			var m struct {
				Name  string `json:"name"`
				Icons []struct {
					Src string `json:"src"`
				} `json:"icons"`
			}
			if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
				t.Errorf("манифест не парсится: %v", err)
			}
			if m.Name == "" || len(m.Icons) == 0 {
				t.Errorf("манифест неполный: %+v", m)
			}
		}
		r.Body.Close()
	}

	// А сама панель без входа по-прежнему закрыта.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	r, err := client.Get(ts.URL + "/api/users")
	if err != nil {
		t.Fatalf("GET /api/users: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("API без входа: status %d, ожидался 401", r.StatusCode)
	}
}

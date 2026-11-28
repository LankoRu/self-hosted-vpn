package telemt

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LankoRu/vps-admin/internal/config"
)

// webConfigBody — ответ GET /v1/config с включённым WEB и одним vhost.
// profiles задаёт начальный список профилей.
func webConfigBody(profiles string) string {
	return `{"ok":true,"data":{"web":{"enabled":true,"carrier":"https","vhosts":[{
		"host":"tg.example.com","base_path":"","public_addr":"203.0.113.10:443",
		"decoy":{"mode":"static_directory","directory":"/var/www/shvpn","index":"index.html"},
		"profiles":[` + profiles + `]}]}},"revision":"r1"}`
}

// webAPI — фейковый telemt API: считает PATCH-и и хранит последний patch-тело.
type webAPI struct {
	mu          sync.Mutex
	patches     int
	lastPatch   map[string]interface{}
	profiles    string // начальные profiles для GET /v1/config
	createdUser string
	deletedUser string
}

func (a *webAPI) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/config":
			a.mu.Lock()
			profiles := a.profiles
			a.mu.Unlock()
			_, _ = w.Write([]byte(webConfigBody(profiles)))
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/config":
			body, _ := io.ReadAll(r.Body)
			var m map[string]interface{}
			_ = json.Unmarshal(body, &m)
			a.mu.Lock()
			a.patches++
			a.lastPatch = m
			// После patch возвращаем обновлённые profiles (чтобы повторный GET видел их).
			if web, ok := m["web"].(map[string]interface{}); ok {
				if vhosts, ok := web["vhosts"].([]interface{}); ok && len(vhosts) > 0 {
					if vh, ok := vhosts[0].(map[string]interface{}); ok {
						if pj, err := json.Marshal(vh["profiles"]); err == nil {
							a.profiles = strings.Trim(string(pj), "[]")
						}
					}
				}
			}
			a.mu.Unlock()
			if r.URL.Query().Get("reload") != "instant" {
				t.Errorf("PATCH /v1/config должен идти с ?reload=instant, got %q", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"ok":true,"data":{},"revision":"r2"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/users":
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			a.mu.Lock()
			a.createdUser, _ = body["username"].(string)
			a.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"user":{"username":"alice","links":{
				"secure":["tg://proxy?server=203.0.113.10&port=443&secret=ddSECRET"],
				"tls":["tg://proxy?server=203.0.113.10&port=443&secret=eeSECRET"]
			}},"secret":"0123456789abcdef0123456789abcdef"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/users/"):
			a.mu.Lock()
			a.deletedUser = strings.TrimPrefix(r.URL.Path, "/v1/users/")
			a.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"ok":true,"data":{"username":"alice"}}`))
		default:
			t.Errorf("неожиданный запрос: %s %s", r.Method, r.URL.Path)
		}
	}
}

func TestProvisionAddsWebProfileAndLink(t *testing.T) {
	api := &webAPI{profiles: `{"user":"hello","secret_mode":"dd"}`}
	ts := httptest.NewServer(api.handler(t))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.TeleMT.BaseURL = ts.URL
	acc, err := New(cfg).Provision("alice")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Должен быть ровно один PATCH (добавили профиль alice).
	api.mu.Lock()
	patches := api.patches
	api.mu.Unlock()
	if patches != 1 {
		t.Fatalf("ожидался 1 PATCH /v1/config, got %d", patches)
	}
	// В patch есть профиль alice с secret_mode=dd.
	if !strings.Contains(itemsText(acc), "tg://webproxy?server=tg.example.com&secret=dd0123456789abcdef0123456789abcdef") {
		t.Errorf("нет WEB-ссылки:\n%s", itemsText(acc))
	}
	// MTProto-ссылки на месте.
	if !strings.Contains(itemsText(acc), "secret=ddSECRET") || !strings.Contains(itemsText(acc), "secret=eeSECRET") {
		t.Errorf("пропали MTProto-ссылки:\n%s", itemsText(acc))
	}
}

func TestProvisionWebProfileIdempotent(t *testing.T) {
	// Профиль alice уже есть → PATCH не нужен, ссылка всё равно есть.
	api := &webAPI{profiles: `{"user":"hello","secret_mode":"dd"},{"user":"alice","secret_mode":"dd"}`}
	ts := httptest.NewServer(api.handler(t))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.TeleMT.BaseURL = ts.URL
	acc, err := New(cfg).Provision("alice")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	api.mu.Lock()
	patches := api.patches
	api.mu.Unlock()
	if patches != 0 {
		t.Errorf("профиль уже есть — PATCH не нужен, got %d", patches)
	}
	if !strings.Contains(itemsText(acc), "tg://webproxy?server=tg.example.com") {
		t.Errorf("WEB-ссылка должна быть и без PATCH:\n%s", itemsText(acc))
	}
}

func TestProvisionWebDisabledNoLink(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/config":
			_, _ = w.Write([]byte(webDisabledConfig))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/users":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"user":{"username":"alice","links":{
				"secure":["tg://proxy?server=h&port=443&secret=ddX"]}},"secret":"ab"}`))
		default:
			t.Errorf("неожиданный запрос: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.TeleMT.BaseURL = ts.URL
	acc, err := New(cfg).Provision("alice")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if strings.Contains(itemsText(acc), "webproxy") {
		t.Errorf("WEB выключен — web-ссылки быть не должно:\n%s", itemsText(acc))
	}
}

func TestDeprovisionRemovesWebProfileBeforeDelete(t *testing.T) {
	api := &webAPI{profiles: `{"user":"hello","secret_mode":"dd"},{"user":"alice","secret_mode":"dd"}`}
	ts := httptest.NewServer(api.handler(t))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.TeleMT.BaseURL = ts.URL
	if err := New(cfg).Deprovision("alice"); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.patches != 1 {
		t.Fatalf("ожидался 1 PATCH (удаление профиля), got %d", api.patches)
	}
	if api.deletedUser != "alice" {
		t.Errorf("пользователь не удалён: %q", api.deletedUser)
	}
	// В последнем patch профиль alice отсутствует.
	pj, _ := json.Marshal(api.lastPatch)
	if strings.Contains(string(pj), `"user":"alice"`) {
		t.Errorf("профиль alice должен быть удалён из patch: %s", pj)
	}
}

func TestWebLinkFormat(t *testing.T) {
	got := webLink("tg.example.com", "0123456789abcdef0123456789abcdef")
	want := "tg://webproxy?server=tg.example.com&secret=dd0123456789abcdef0123456789abcdef"
	if got != want {
		t.Errorf("webLink = %q, want %q", got, want)
	}
}

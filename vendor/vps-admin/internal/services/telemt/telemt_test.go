package telemt

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/service"
)

func itemsText(acc *service.Account) string {
	s := ""
	for _, it := range acc.Items {
		s += it.Label + ": " + it.Value + "\n"
	}
	return s
}

// webDisabledConfig — ответ GET /v1/config с выключенным WEB: провижининг MTProto
// не трогает WEB-профили и не добавляет web-ссылку.
const webDisabledConfig = `{"ok":true,"data":{"web":{"enabled":false}},"revision":"r1"}`

func TestProvisionUsesTelemtLinks(t *testing.T) {
	var gotBody map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/config" {
			_, _ = w.Write([]byte(webDisabledConfig))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/users" {
			t.Errorf("неожиданный запрос: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// Ответ в формате CreateUserResponse со сгенерированными ссылками.
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"user":{"username":"alice","links":{
				"secure":["tg://proxy?server=h&port=443&secret=dd..."],
				"tls":["tg://proxy?server=h&port=443&secret=ee..."]
			}},
			"secret":"deadbeefdeadbeefdeadbeefdeadbeef"
		}`))
	}))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.TeleMT.BaseURL = ts.URL

	acc, err := New(cfg).Provision("alice")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// В config.toml telemt пишется username.
	if gotBody["username"] != "alice" {
		t.Errorf("в telemt отправлен username=%v, ожидался alice", gotBody["username"])
	}
	// Ссылки берём из ответа telemt (dd и ee).
	if !strings.Contains(itemsText(acc), "secret=dd") {
		t.Errorf("нет dd-ссылки:\n%s", itemsText(acc))
	}
	if !strings.Contains(itemsText(acc), "secret=ee") {
		t.Errorf("нет ee-ссылки:\n%s", itemsText(acc))
	}
	if acc.Identity != "alice" {
		t.Errorf("identity=%q, ожидался alice", acc.Identity)
	}
}

func TestProvisionFiltersIPv6Links(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"user":{"username":"alice","links":{
			"secure":[
				"tg://proxy?server=1.2.3.4&port=443&secret=ddV4",
				"tg://proxy?server=2001:db8::1&port=443&secret=ddV6"
			],
			"tls":[
				"tg://proxy?server=1.2.3.4&port=443&secret=eeV4",
				"tg://proxy?server=2001:db8::2&port=443&secret=eeV6"
			]
		}},"secret":"x"}`))
	}))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.TeleMT.BaseURL = ts.URL

	acc, err := New(cfg).Provision("alice")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !strings.Contains(itemsText(acc), "secret=ddV4") || !strings.Contains(itemsText(acc), "secret=eeV4") {
		t.Errorf("IPv4-ссылки должны остаться:\n%s", itemsText(acc))
	}
	if strings.Contains(itemsText(acc), "ddV6") || strings.Contains(itemsText(acc), "eeV6") {
		t.Errorf("IPv6-ссылки должны быть отброшены:\n%s", itemsText(acc))
	}
}

func TestProvisionFallsBackToStatsForLinks(t *testing.T) {
	// POST /v1/users возвращает пользователя БЕЗ ссылок (частый случай),
	// ссылки должны дочитаться из GET /v1/stats/users.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/config":
			_, _ = w.Write([]byte(webDisabledConfig))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/users":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"user":{"username":"alice","links":{}},"secret":"deadbeef"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/stats/users":
			// Реальный telemt отдаёт конверт {ok, data:[…]}.
			_, _ = w.Write([]byte(`{"ok":true,"data":[{"username":"alice","links":{
				"tls":["tg://proxy?server=h&port=443&secret=ee123"],
				"tls_domains":[{"domain":"example.com","link":"tg://proxy?secret=eeDOM"}]
			}}]}`))
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
	if !strings.Contains(itemsText(acc), "secret=ee123") {
		t.Errorf("ee-ссылка из stats не подхватилась:\n%s", itemsText(acc))
	}
	if !strings.Contains(itemsText(acc), "example.com") || !strings.Contains(itemsText(acc), "secret=eeDOM") {
		t.Errorf("ссылка из tls_domains не подхватилась:\n%s", itemsText(acc))
	}
}

func TestDeprovisionAccepts202(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/config" {
			_, _ = w.Write([]byte(webDisabledConfig))
			return
		}
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/users/router" {
			t.Errorf("неожиданный запрос: %s %s", r.Method, r.URL.Path)
		}
		// Реальный telemt отвечает 202 Accepted с {"ok":true,...}.
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true,"data":{"username":"router","in_runtime":true}}`))
	}))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.TeleMT.BaseURL = ts.URL
	if err := New(cfg).Deprovision("router"); err != nil {
		t.Fatalf("Deprovision при 202 не должен возвращать ошибку: %v", err)
	}
}

func TestGetTrafficTolerantShapes(t *testing.T) {
	cases := map[string]string{
		"bare array":     `[{"username":"alice","total_octets":1000},{"username":"bob","total_octets":2000}]`,
		"users envelope": `{"generated_at":1,"users":[{"username":"alice","total_octets":1000},{"username":"bob","total_octets":2000}]}`,
		"data envelope":  `{"ok":true,"data":[{"username":"alice","total_octets":1000},{"username":"bob","total_octets":2000}]}`,
		"username map":   `{"alice":{"total_octets":1000},"bob":{"total_octets":2000}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer ts.Close()

			cfg := &config.Config{}
			cfg.TeleMT.BaseURL = ts.URL

			tr, err := New(cfg).GetTraffic()
			if err != nil {
				t.Fatalf("GetTraffic: %v", err)
			}
			if tr["alice"] != 1000 || tr["bob"] != 2000 {
				t.Errorf("трафик разобран неверно: %+v", tr)
			}
		})
	}
}

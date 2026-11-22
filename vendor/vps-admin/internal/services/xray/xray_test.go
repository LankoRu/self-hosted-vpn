package xray

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LankoRu/vps-admin/internal/config"
)

func TestJSONBytesHandlesStringAndObject(t *testing.T) {
	// settings строкой с вложенным JSON (старые версии)
	if got := string(jsonBytes(json.RawMessage(`"{\"clients\":[]}"`))); got != `{"clients":[]}` {
		t.Errorf("строка: %q", got)
	}
	// settings уже объектом (новые версии)
	if got := string(jsonBytes(json.RawMessage(`{"clients":[]}`))); got != `{"clients":[]}` {
		t.Errorf("объект: %q", got)
	}
}

func TestInboundFlowFromObjectSettings(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// settings/streamSettings приходят ОБЪЕКТАМИ, а не строками.
		_, _ = w.Write([]byte(`{"obj":[{"id":1,
			"settings":{"clients":[{"flow":"xtls-rprx-vision","email":"a"}]},
			"streamSettings":{"security":"reality"}}]}`))
	}))
	defer ts.Close()

	flow, err := newSvc(ts.URL, "tok").inboundFlow(1)
	if err != nil {
		t.Fatalf("inboundFlow: %v", err)
	}
	if flow != "xtls-rprx-vision" {
		t.Errorf("flow = %q, ожидался xtls-rprx-vision", flow)
	}
}

func newSvc(url, token string) *Service {
	cfg := &config.Config{}
	cfg.Xray3xUI.BaseURL = url
	cfg.Xray3xUI.APIToken = token
	return New(cfg)
}

func TestGetTrafficSendsBearerAndUA(t *testing.T) {
	var gotAuth, gotUA string
	var loginHit bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			loginHit = true
		}
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"obj":[{"clientStats":[{"email":"alice","up":10,"down":20}]}]}`))
	}))
	defer ts.Close()

	tr, err := newSvc(ts.URL, "secrettoken").GetTraffic()
	if err != nil {
		t.Fatalf("GetTraffic: %v", err)
	}
	if loginHit {
		t.Error("/login не должен вызываться — авторизация только по токену")
	}
	if gotAuth != "Bearer secrettoken" {
		t.Errorf("Authorization = %q, ожидался 'Bearer secrettoken'", gotAuth)
	}
	if !strings.Contains(gotUA, "Mozilla/") {
		t.Errorf("User-Agent = %q, ожидался браузерный", gotUA)
	}
	if tr["alice"] != 30 {
		t.Errorf("трафик alice = %d, ожидалось 30", tr["alice"])
	}
}

func TestNoTokenIsError(t *testing.T) {
	_, err := newSvc("http://127.0.0.1:1", "").GetTraffic()
	if err == nil || !strings.Contains(err.Error(), "api_token") {
		t.Fatalf("ожидалась ошибка про api_token, получено: %v", err)
	}
}

func TestProvisionUsesV3ClientsAPI(t *testing.T) {
	var addHit bool
	var addBody map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/panel/api/clients/add":
			addHit = true
			_ = json.NewDecoder(r.Body).Decode(&addBody)
			_, _ = w.Write([]byte(`{"success":true,"msg":"ok"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/panel/api/inbounds/list":
			_, _ = w.Write([]byte(`{"obj":[{"id":7,"settings":{"clients":[]},"streamSettings":{"security":"reality"}}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/panel/api/clients/links/testuser":
			_, _ = w.Write([]byte(`{"success":true,"obj":["vless://uuid@host:443?type=tcp#testuser"]}`))
		default:
			t.Errorf("неожиданный запрос: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.Xray3xUI.BaseURL = ts.URL
	cfg.Xray3xUI.APIToken = "tok"
	cfg.Xray3xUI.InboundID = 7

	acc, err := New(cfg).Provision("testuser")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !addHit {
		t.Fatal("не был вызван POST /panel/api/clients/add")
	}
	ids, _ := addBody["inboundIds"].([]interface{})
	if len(ids) != 1 || ids[0].(float64) != 7 {
		t.Errorf("inboundIds = %v, ожидалось [7]", addBody["inboundIds"])
	}
	joined := ""
	for _, it := range acc.Items {
		joined += it.Value + "\n"
	}
	if !strings.Contains(joined, "vless://") {
		t.Errorf("ожидались ссылки панели среди items:\n%s", joined)
	}
}

func TestClientLinksRewriteLocalhost(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/panel/api/inbounds/list":
			_, _ = w.Write([]byte(`{"obj":[{"id":7,"settings":{"clients":[]},"streamSettings":{"security":"reality"}}]}`))
		case strings.HasPrefix(r.URL.Path, "/panel/api/clients/links/"):
			// Панель отдаёт ссылку с localhost (т.к. её дёргают по 127.0.0.1).
			_, _ = w.Write([]byte(`{"success":true,"obj":["vless://uuid@localhost:54348?type=tcp&security=reality#u"]}`))
		default:
			_, _ = w.Write([]byte(`{"success":true}`))
		}
	}))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.Xray3xUI.BaseURL = ts.URL
	cfg.Xray3xUI.APIToken = "tok"
	cfg.Xray3xUI.InboundID = 7
	cfg.Xray3xUI.SubURL = "https://vpn.example.com:2096/sub" // хост отсюда

	acc, err := New(cfg).Provision("u")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// localhost в ссылке заменён на публичный хост, порт (54348) сохранён.
	joined := ""
	for _, it := range acc.Items {
		joined += it.Value + "\n"
	}
	if strings.Contains(joined, "localhost") {
		t.Errorf("localhost не заменён в ссылке:\n%s", joined)
	}
	if !strings.Contains(joined, "vpn.example.com:54348") {
		t.Errorf("ожидался vpn.example.com:54348 в ссылке:\n%s", joined)
	}
}

func TestRewriteLinkHostKeepsNormalAddress(t *testing.T) {
	link := "vless://uuid@1.2.3.4:443?type=tcp#a"
	if got := rewriteLinkHost(link, "example.com"); got != link {
		t.Errorf("нормальный адрес не должен меняться: %q", got)
	}
}

// Замена хоста не должна трогать НИ ОДНОГО другого байта ссылки. Раньше здесь
// использовался url.Parse+String(), который перекодировал #remark (кириллица и
// пробелы → %D0%98%20…) — клиент показывал мусор вместо имени, а QR кодировал
// уже другую ссылку.
func TestRewriteLinkHostPreservesEverythingElse(t *testing.T) {
	cases := []struct {
		name, in, host, want string
	}{
		{
			name: "кириллический remark и %2F в query",
			in:   "vless://uuid@localhost:54348?spx=%2F&flow=xtls-rprx-vision#Иван (reality)",
			host: "194.164.32.196",
			want: "vless://uuid@194.164.32.196:54348?spx=%2F&flow=xtls-rprx-vision#Иван (reality)",
		},
		{
			name: "без порта",
			in:   "vless://uuid@localhost?type=tcp#a",
			host: "example.com",
			want: "vless://uuid@example.com?type=tcp#a",
		},
		{
			name: "без userinfo",
			in:   "vless://127.0.0.1:443#x",
			host: "example.com",
			want: "vless://example.com:443#x",
		},
		{
			name: "IPv6-loopback в скобках",
			in:   "vless://uuid@[::1]:443?a=b#c",
			host: "example.com",
			want: "vless://uuid@example.com:443?a=b#c",
		},
		{
			name: "чужой хост не трогаем",
			in:   "vless://uuid@1.2.3.4:443?a=b#c",
			host: "example.com",
			want: "vless://uuid@1.2.3.4:443?a=b#c",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rewriteLinkHost(c.in, c.host); got != c.want {
				t.Errorf("ссылка искажена:\nполучили: %s\nожидали:  %s", got, c.want)
			}
		})
	}
}

func TestDeprovisionUsesV3DelByEmail(t *testing.T) {
	var delPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delPath = r.URL.Path
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer ts.Close()

	if err := newSvc(ts.URL, "tok").Deprovision("belikov"); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	if delPath != "/panel/api/clients/del/belikov" {
		t.Errorf("путь удаления = %q, ожидался /panel/api/clients/del/belikov", delPath)
	}
}

func TestForbiddenHintsAtToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	_, err := newSvc(ts.URL, "badtoken").GetTraffic()
	if err == nil || !strings.Contains(err.Error(), "api_token") {
		t.Fatalf("ожидалась подсказка про api_token на 403, получено: %v", err)
	}
}

func TestRefreshItemsReadsPanelWithoutRecreating(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/panel/api/clients/get/u":
			_, _ = w.Write([]byte(`{"success":true,"obj":{"client":{"id":5,"email":"u","uuid":"uuid-1","subId":"sub42"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/panel/api/clients/links/u":
			// Хост в 3x-ui: адрес и порт 443 уже подставлены панелью.
			_, _ = w.Write([]byte(`{"success":true,"obj":["vless://uuid-1@1.2.3.4:443?type=tcp#u"]}`))
		default:
			t.Errorf("неожиданный запрос (клиента трогать нельзя): %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.Xray3xUI.BaseURL = ts.URL
	cfg.Xray3xUI.APIToken = "tok"
	cfg.Xray3xUI.SubURL = "https://1.2.3.4:2096/sub/"

	items, err := New(cfg).RefreshItems("u")
	if err != nil {
		t.Fatalf("RefreshItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("ожидалось 2 items (ссылка + подписка), получено %+v", items)
	}
	if items[0].Value != "vless://uuid-1@1.2.3.4:443?type=tcp#u" {
		t.Errorf("ссылка = %q", items[0].Value)
	}
	if items[1].Value != "https://1.2.3.4:2096/sub/sub42" {
		t.Errorf("подписка = %q, ожидался прежний subId", items[1].Value)
	}
}

func TestRefreshItemsFailsInsteadOfDegrading(t *testing.T) {
	cases := map[string]string{
		"клиента нет в панели": `{"success":false,"msg":"record not found","obj":null}`,
		"панель без ссылок":    "",
	}
	for name, getResp := range cases {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/panel/api/clients/get/"):
					if getResp != "" {
						_, _ = w.Write([]byte(getResp))
						return
					}
					_, _ = w.Write([]byte(`{"success":true,"obj":{"client":{"uuid":"x","subId":"s"}}}`))
				case strings.HasPrefix(r.URL.Path, "/panel/api/clients/links/"):
					_, _ = w.Write([]byte(`{"success":true,"obj":[]}`))
				}
			}))
			defer ts.Close()
			// Без ошибки RefreshLinks перезаписал бы рабочие ссылки одной подпиской/UUID.
			if _, err := newSvc(ts.URL, "tok").RefreshItems("u"); err == nil {
				t.Fatal("ожидалась ошибка")
			}
		})
	}
}

func TestGetTrafficSkipsTunnelInbounds(t *testing.T) {
	// AmneziaWG/WireGuard-инбаунды в той же панели не должны попадать в
	// xray-трафик: их считает отдельный коллектор (иначе задвоение на юзера).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"obj":[
			{"id":1,"protocol":"vless","clientStats":[{"email":"alice","up":10,"down":20}]},
			{"id":7,"protocol":"amneziawg","clientStats":[{"email":"alice","up":1000,"down":2000}]},
			{"id":8,"protocol":"wireguard","clientStats":[{"email":"bob","up":5,"down":5}]}
		]}`))
	}))
	defer ts.Close()

	m, err := newSvc(ts.URL, "tok").GetTraffic()
	if err != nil {
		t.Fatalf("GetTraffic: %v", err)
	}
	if len(m) != 1 || m["alice"] != 30 {
		t.Errorf("traffic = %v, ожидался только vless-клиент alice=30", m)
	}
}

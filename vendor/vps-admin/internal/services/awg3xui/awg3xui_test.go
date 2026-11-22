package awg3xui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LankoRu/vps-admin/internal/config"
)

func newSvc(url, token string, inboundID int, publicHost string) *Service {
	cfg := &config.Config{}
	cfg.AmneziaWG3xUI.BaseURL = url
	cfg.AmneziaWG3xUI.APIToken = token
	cfg.AmneziaWG3xUI.InboundID = inboundID
	cfg.AmneziaWG3xUI.PublicHost = publicHost
	return New(cfg)
}

// testConf — минимальный .conf в формате панели (amneziaWGConfigText).
const testConf = `[Interface]
PrivateKey = CLIENTKEY
Address = 10.8.0.2/32
DNS = 1.1.1.1
MTU = 1280
Jc = 4
Jmin = 10
Jmax = 50
S1 = 53
S2 = 97
H1 = 5
H2 = 6
H3 = 7
H4 = 8
I1 = <r 2><b 0xdeadbeef>
HeaderProtectionKey = HPKEY
ContentPaddingAddition = 24-56

# alice-awg
[Peer]
PublicKey = SERVERKEY
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = 127.0.0.1:443
`

func vpnLink(conf string) string {
	return "vpn://" + base64.RawURLEncoding.EncodeToString([]byte(conf))
}

// inboundsListJSON — ответ inbounds/list с нашим инбаундом (для проверки
// attachedToOwnInbound при повторном Provision).
func inboundsListJSON(emails ...string) string {
	clients := make([]string, 0, len(emails))
	for _, e := range emails {
		clients = append(clients, fmt.Sprintf(`{"email":%q}`, e))
	}
	return fmt.Sprintf(`{"obj":[{"id":7,"protocol":"amneziawg","settings":{"clients":[%s]}}]}`,
		strings.Join(clients, ","))
}

func TestProvisionCreatesSeparateClientAndPatchesEndpoint(t *testing.T) {
	var addBody map[string]interface{}
	var added bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/panel/api/clients/add"):
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &addBody)
			added = true
			fmt.Fprint(w, `{"success":true}`)
		case strings.HasSuffix(r.URL.Path, "/panel/api/clients/get/alice-awg"):
			if !added {
				fmt.Fprint(w, `{"success":false,"msg":"record not found"}`)
				return
			}
			fmt.Fprint(w, `{"success":true,"obj":{"client":{"allowedIPs":["10.8.0.2/32"],"privateKey":"CLIENTKEY"}}}`)
		case strings.HasSuffix(r.URL.Path, "/panel/api/clients/links/alice-awg"):
			fmt.Fprintf(w, `{"obj":[%q]}`, vpnLink(testConf))
		default:
			t.Errorf("неожиданный запрос: %s", r.URL.Path)
			fmt.Fprint(w, `{"success":false}`)
		}
	}))
	defer ts.Close()

	acc, err := newSvc(ts.URL, "tok", 7, "203.0.113.5").Provision("alice")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// AWG-клиент — отдельная учётка (свой email → своя строка трафика в панели)
	if acc.Service != "amneziawg" || acc.Identity != "alice-awg" {
		t.Errorf("service/identity = %q/%q, ожидался amneziawg/alice-awg", acc.Service, acc.Identity)
	}
	if acc.Address != "10.8.0.2" {
		t.Errorf("Address = %q, ожидался 10.8.0.2 (без маски)", acc.Address)
	}
	cl, _ := addBody["client"].(map[string]interface{})
	if cl["email"] != "alice-awg" || cl["enable"] != true {
		t.Errorf("client body = %v", addBody)
	}
	if ids, _ := addBody["inboundIds"].([]interface{}); len(ids) != 1 || ids[0] != float64(7) {
		t.Errorf("inboundIds = %v", addBody["inboundIds"])
	}
	if cl["subId"] == "" || cl["subId"] == nil {
		t.Error("subId не сгенерирован")
	}
	if len(acc.Items) != 2 {
		t.Fatalf("Items = %d, ожидалось 2 (vpn:// + .conf)", len(acc.Items))
	}
	conf := acc.Items[1].Value
	if !strings.Contains(conf, "Endpoint = 203.0.113.5:443") {
		t.Errorf("Endpoint не заменён:\n%s", conf)
	}
	if !strings.Contains(conf, "HeaderProtectionKey = HPKEY") || !strings.Contains(conf, "I1 = <r 2><b 0xdeadbeef>") {
		t.Error("параметры обфускации повреждены")
	}
	if acc.Items[1].File != "alice-awg-amneziawg.conf" || acc.Items[0].File != "alice-awg.vpnuri" {
		t.Errorf("file names: %q / %q", acc.Items[0].File, acc.Items[1].File)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(acc.Items[0].Value, "vpn://"))
	if err != nil || string(raw) != conf {
		t.Errorf("vpn:// не совпадает с .conf: %v", err)
	}
}

func TestProvisionExistingOwnClientIsIdempotent(t *testing.T) {
	var addHit bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/panel/api/clients/add"):
			addHit = true
			fmt.Fprint(w, `{"success":true}`)
		case strings.HasSuffix(r.URL.Path, "/panel/api/inbounds/list"):
			fmt.Fprint(w, inboundsListJSON("alice-awg"))
		case strings.HasSuffix(r.URL.Path, "/panel/api/clients/get/alice-awg"):
			fmt.Fprint(w, `{"success":true,"obj":{"client":{"allowedIPs":["10.8.1.2/32"]}}}`)
		case strings.HasSuffix(r.URL.Path, "/panel/api/clients/links/alice-awg"):
			fmt.Fprintf(w, `{"obj":[%q]}`, vpnLink(testConf))
		default:
			t.Errorf("неожиданный запрос: %s", r.URL.Path)
			fmt.Fprint(w, `{"success":false}`)
		}
	}))
	defer ts.Close()

	acc, err := newSvc(ts.URL, "tok", 7, "203.0.113.5").Provision("alice")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if addHit {
		t.Error("существующего клиента не нужно создавать заново")
	}
	if acc.Identity != "alice-awg" || acc.Address != "10.8.1.2" || len(acc.Items) != 2 {
		t.Errorf("account = %+v", acc)
	}
}

func TestProvisionForeignEmailCollisionFails(t *testing.T) {
	// email alice-awg занят учёткой в ЧУЖОМ инбаунде (например, xray-клиент
	// с именем alice-awg) — создавать поверх нельзя.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/panel/api/inbounds/list"):
			fmt.Fprint(w, `{"obj":[{"id":1,"protocol":"vless","settings":{"clients":[{"email":"alice-awg"}]}}]}`)
		case strings.HasSuffix(r.URL.Path, "/panel/api/clients/get/alice-awg"):
			fmt.Fprint(w, `{"success":true,"obj":{"client":{}}}`)
		default:
			t.Errorf("неожиданный запрос: %s", r.URL.Path)
			fmt.Fprint(w, `{"success":false}`)
		}
	}))
	defer ts.Close()

	if _, err := newSvc(ts.URL, "tok", 7, "").Provision("alice"); err == nil {
		t.Error("ожидалась ошибка коллизии email")
	}
}

func TestProvisionWithoutLinksKeepsClient(t *testing.T) {
	var added bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/clients/add"):
			added = true
			fmt.Fprint(w, `{"success":true}`)
		case strings.HasSuffix(r.URL.Path, "/clients/get/alice-awg"):
			if !added {
				fmt.Fprint(w, `{"success":false,"msg":"record not found"}`)
				return
			}
			fmt.Fprint(w, `{"success":true,"obj":{"client":{"allowedIPs":["10.8.0.2/32"]}}}`)
		case strings.HasSuffix(r.URL.Path, "/clients/links/alice-awg"):
			fmt.Fprint(w, `{"obj":[]}`)
		default:
			fmt.Fprint(w, `{"success":false}`)
		}
	}))
	defer ts.Close()

	acc, err := newSvc(ts.URL, "tok", 7, "").Provision("alice")
	if err != nil {
		t.Fatalf("пустые ссылки не должны откатывать клиента: %v", err)
	}
	if len(acc.Items) != 0 {
		t.Errorf("Items = %v, ожидалось пусто", acc.Items)
	}
}

func TestGetTrafficOnlyOwnInbound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"obj":[
			{"id":1,"protocol":"vless","clientStats":[{"email":"alice","up":100,"down":200}]},
			{"id":7,"protocol":"amneziawg","clientStats":[{"email":"alice-awg","up":10,"down":20},{"email":"bob-awg","up":1,"down":2}]}
		]}`)
	}))
	defer ts.Close()

	m, err := newSvc(ts.URL, "tok", 7, "").GetTraffic()
	if err != nil {
		t.Fatalf("GetTraffic: %v", err)
	}
	if len(m) != 2 || m["alice-awg"] != 30 || m["bob-awg"] != 3 {
		t.Errorf("traffic = %v (ожидался только inbound 7: alice-awg=30, bob-awg=3)", m)
	}
}

func TestGetTrafficSendsBearerAndUA(t *testing.T) {
	var gotAuth, gotUA string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		fmt.Fprint(w, `{"obj":[]}`)
	}))
	defer ts.Close()

	if _, err := newSvc(ts.URL, "tok", 7, "").GetTraffic(); err != nil {
		t.Fatalf("GetTraffic: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotUA, "Mozilla") {
		t.Errorf("User-Agent = %q (нужен браузерный)", gotUA)
	}
}

func TestNoTokenIsError(t *testing.T) {
	s := newSvc("http://127.0.0.1:1", "", 7, "")
	if _, err := s.GetTraffic(); err == nil {
		t.Error("ожидалась ошибка об отсутствии api_token")
	}
}

func TestNoInboundIDIsError(t *testing.T) {
	s := newSvc("http://127.0.0.1:1", "tok", 0, "")
	if _, err := s.Provision("alice"); err == nil {
		t.Error("ожидалась ошибка об отсутствии inbound_id")
	}
}

func TestDeprovisionDeletesSeparateClient(t *testing.T) {
	var delPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delPath = r.URL.Path
		fmt.Fprint(w, `{"success":true}`)
	}))
	defer ts.Close()

	// identity — email AWG-клиента; это отдельная учётка, del безопасен
	if err := newSvc(ts.URL, "tok", 7, "").Deprovision("alice-awg"); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	if !strings.HasSuffix(delPath, "/panel/api/clients/del/alice-awg") {
		t.Errorf("del path = %q", delPath)
	}
}

func TestDeprovisionNotFoundIsOK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":false,"msg":"client not found"}`)
	}))
	defer ts.Close()

	if err := newSvc(ts.URL, "tok", 7, "").Deprovision("alice-awg"); err != nil {
		t.Errorf("«не найден» должен считаться успехом: %v", err)
	}
}

func TestRefreshItemsReadsPanelWithoutRecreating(t *testing.T) {
	var addHit bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/clients/add"):
			addHit = true
			fmt.Fprint(w, `{"success":true}`)
		case strings.HasSuffix(r.URL.Path, "/clients/get/alice-awg"):
			fmt.Fprint(w, `{"success":true,"obj":{"client":{"allowedIPs":["10.8.0.2/32"]}}}`)
		case strings.HasSuffix(r.URL.Path, "/clients/links/alice-awg"):
			fmt.Fprintf(w, `{"obj":[%q]}`, vpnLink(testConf))
		default:
			fmt.Fprint(w, `{"success":false}`)
		}
	}))
	defer ts.Close()

	items, err := newSvc(ts.URL, "tok", 7, "203.0.113.5").RefreshItems("alice-awg")
	if err != nil {
		t.Fatalf("RefreshItems: %v", err)
	}
	if addHit {
		t.Error("RefreshItems не должен пересоздавать клиента")
	}
	if len(items) != 2 || !strings.Contains(items[1].Value, "Endpoint = 203.0.113.5:443") {
		t.Errorf("items = %v", items)
	}
}

func TestRefreshItemsFailsOnEmptyLinks(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/clients/get/alice-awg"):
			fmt.Fprint(w, `{"success":true,"obj":{"client":{}}}`)
		case strings.HasSuffix(r.URL.Path, "/clients/links/alice-awg"):
			fmt.Fprint(w, `{"obj":[]}`)
		default:
			fmt.Fprint(w, `{"success":false}`)
		}
	}))
	defer ts.Close()

	if _, err := newSvc(ts.URL, "tok", 7, "").RefreshItems("alice-awg"); err == nil {
		t.Error("пустой ответ links — ошибка (иначе затрём сохранённые конфиги)")
	}
}

func TestBuildURLWithBasePath(t *testing.T) {
	s := newSvc("http://127.0.0.1:2053/", "tok", 7, "")
	s.cfg.WebBasePath = "/panel123/"
	got := s.buildURL("/panel/api/inbounds/list")
	want := "http://127.0.0.1:2053/panel123/panel/api/inbounds/list"
	if got != want {
		t.Errorf("buildURL = %q, want %q", got, want)
	}
}

func TestPatchEndpointHost(t *testing.T) {
	cases := []struct {
		name, conf, host, want string
	}{
		{"localhost", "Endpoint = localhost:443\n", "1.2.3.4", "Endpoint = 1.2.3.4:443\n"},
		{"loopback v4", "Endpoint = 127.0.0.1:51820", "1.2.3.4", "Endpoint = 1.2.3.4:51820"},
		{"wildcard", "Endpoint = 0.0.0.0:443", "1.2.3.4", "Endpoint = 1.2.3.4:443"},
		{"loopback v6", "Endpoint = [::1]:443", "1.2.3.4", "Endpoint = 1.2.3.4:443"},
		{"нормальный адрес не трогаем", "Endpoint = vpn.example.com:443", "1.2.3.4", "Endpoint = vpn.example.com:443"},
		{"пустой host — без изменений", "Endpoint = 127.0.0.1:443", "", "Endpoint = 127.0.0.1:443"},
		{"отступ сохраняется", "  Endpoint = 127.0.0.1:443", "1.2.3.4", "  Endpoint = 1.2.3.4:443"},
		{"без двоеточия (порт обязателен панелью)", "Endpoint = 127.0.0.1", "1.2.3.4", "Endpoint = 1.2.3.4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := patchEndpointHost(c.conf, c.host); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestPatchEndpointHostPreservesOtherBytes(t *testing.T) {
	got := patchEndpointHost(testConf, "203.0.113.5")
	want := strings.Replace(testConf, "Endpoint = 127.0.0.1:443", "Endpoint = 203.0.113.5:443", 1)
	if got != want {
		t.Errorf("конфиг изменён дальше Endpoint:\n%s", got)
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("вася pupkin"); got != "_____pupkin" {
		t.Errorf("sanitize = %q", got)
	}
	if got := sanitize("alice-1_x"); got != "alice-1_x" {
		t.Errorf("sanitize = %q", got)
	}
}

func TestClientAddressAcceptsStringAllowedIPs(t *testing.T) {
	// clients/get отдаёт allowedIPs строкой — адрес должен распарситься
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"obj":{"client":{"allowedIPs":"10.8.1.5/32"}}}`)
	}))
	defer ts.Close()

	addr, err := newSvc(ts.URL, "tok", 7, "").clientAddress("alice-awg")
	if err != nil || addr != "10.8.1.5" {
		t.Errorf("addr = %q, err = %v; want 10.8.1.5", addr, err)
	}
}

func TestProvisionDeviceCreatesNamedClient(t *testing.T) {
	var addBody map[string]interface{}
	var added bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/panel/api/clients/add"):
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &addBody)
			added = true
			fmt.Fprint(w, `{"success":true}`)
		case strings.HasSuffix(r.URL.Path, "/panel/api/clients/get/alice-awg-phone"):
			if !added {
				fmt.Fprint(w, `{"success":false,"msg":"record not found"}`)
				return
			}
			fmt.Fprint(w, `{"success":true,"obj":{"client":{"allowedIPs":"10.8.1.9/32"}}}`)
		case strings.HasSuffix(r.URL.Path, "/panel/api/clients/links/alice-awg-phone"):
			fmt.Fprintf(w, `{"obj":[%q]}`, vpnLink(testConf))
		default:
			t.Errorf("неожиданный запрос: %s", r.URL.Path)
			fmt.Fprint(w, `{"success":false}`)
		}
	}))
	defer ts.Close()

	acc, err := newSvc(ts.URL, "tok", 7, "203.0.113.5").ProvisionDevice("alice", "phone")
	if err != nil {
		t.Fatalf("ProvisionDevice: %v", err)
	}
	if acc.Identity != "alice-awg-phone" || acc.Address != "10.8.1.9" {
		t.Errorf("account = %+v", acc)
	}
	cl, _ := addBody["client"].(map[string]interface{})
	if cl["email"] != "alice-awg-phone" {
		t.Errorf("email = %v", cl["email"])
	}
	if len(acc.Items) != 2 || acc.Items[1].File != "alice-awg-phone-amneziawg.conf" {
		t.Errorf("items = %+v", acc.Items)
	}
}

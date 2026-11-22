package xray

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/service"
)

func connSvc(url string) *Service {
	cfg := &config.Config{}
	cfg.Xray3xUI.BaseURL = url
	cfg.Xray3xUI.APIToken = "tok"
	cfg.Xray3xUI.InboundID = 1
	cfg.Xray3xUI.BackupPort = 10092
	return New(cfg)
}

const realityStream = `{"network":"tcp","security":"reality","realitySettings":{"dest":"www.google.com:443","serverNames":["www.google.com","google.com"],"privateKey":"PRIV","publicKey":"PUB","shortIds":["abcd"],"fingerprint":"chrome"}}`

func panelWithInbound(stream string, extra ...map[string]interface{}) []map[string]interface{} {
	ib := map[string]interface{}{
		"id": 1, "port": 10090, "protocol": "vless", "remark": "main", "enable": true,
		"listen": "127.0.0.1", "settings": `{"clients":[{"email":"alice"}],"decryption":"none"}`,
		"streamSettings": stream,
		"sniffing":       map[string]interface{}{"enabled": true},
	}
	return append([]map[string]interface{}{ib}, extra...)
}

func TestRealityReadsSettings(t *testing.T) {
	for _, stream := range []string{realityStream, realityStream} {
		_ = stream
	}
	// строковый и объектный streamSettings
	for _, asObject := range []bool{false, true} {
		stream := realityStream
		if asObject {
			var obj interface{}
			_ = json.Unmarshal([]byte(realityStream), &obj)
			b, _ := json.Marshal(obj)
			stream = string(b)
		}
		list := panelWithInbound(stream)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "obj": list})
		}))
		rs, err := connSvc(ts.URL).Reality(1)
		ts.Close()
		if err != nil {
			t.Fatalf("Reality (object=%v): %v", asObject, err)
		}
		if rs.Dest != "www.google.com:443" || len(rs.ServerNames) != 2 || rs.Fingerprint != "chrome" || rs.PublicKey != "PUB" {
			t.Errorf("rs = %+v", rs)
		}
	}
}

func TestUpdateRealityPayload(t *testing.T) {
	var updateBody map[string]interface{}
	list := panelWithInbound(realityStream)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/inbounds/update/1") {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &updateBody)
			fmt.Fprint(w, `{"success":true}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "obj": list})
	}))
	defer ts.Close()

	err := connSvc(ts.URL).UpdateReality(1, &service.RealitySettings{
		Dest: "www.apple.com:443", ServerNames: []string{"www.apple.com"}, Fingerprint: "firefox",
	})
	if err != nil {
		t.Fatalf("UpdateReality: %v", err)
	}
	ss, ok := updateBody["streamSettings"].(string)
	if !ok {
		t.Fatalf("streamSettings не строка: %T", updateBody["streamSettings"])
	}
	var stream map[string]interface{}
	_ = json.Unmarshal([]byte(ss), &stream)
	rs := stream["realitySettings"].(map[string]interface{})
	if rs["dest"] != "www.apple.com:443" || rs["fingerprint"] != "firefox" {
		t.Errorf("realitySettings = %v", rs)
	}
	if rs["privateKey"] != "PRIV" || rs["publicKey"] != "PUB" {
		t.Errorf("ключи затронуты: %v", rs)
	}
	if names := rs["serverNames"].([]interface{}); len(names) != 1 || names[0] != "www.apple.com" {
		t.Errorf("serverNames = %v", rs["serverNames"])
	}
	if _, ok := updateBody["settings"].(string); !ok {
		t.Errorf("settings не нормализован в строку: %T", updateBody["settings"])
	}
}

func TestAttachDetach(t *testing.T) {
	var path string
	var body map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		fmt.Fprint(w, `{"success":true}`)
	}))
	defer ts.Close()

	if err := connSvc(ts.URL).Attach("alice", 7); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !strings.HasSuffix(path, "/panel/api/clients/alice/attach") {
		t.Errorf("path = %q", path)
	}
	if ids := body["inboundIds"].([]interface{}); len(ids) != 1 || ids[0] != float64(7) {
		t.Errorf("body = %v", body)
	}
	if err := connSvc(ts.URL).Detach("alice", 7); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if !strings.HasSuffix(path, "/panel/api/clients/alice/detach") {
		t.Errorf("path = %q", path)
	}
}

func TestEnsureBackupCreatesXhttp(t *testing.T) {
	var addBody map[string]interface{}
	added := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/inbounds/add"):
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &addBody)
			added = true
			fmt.Fprint(w, `{"success":true}`)
		case strings.Contains(r.URL.Path, "/inbounds/list"):
			list := panelWithInbound(realityStream)
			if added {
				list = append(list, map[string]interface{}{
					"id": 3, "port": 10092, "protocol": "vless", "remark": "shvpn-backup-xhttp", "enable": true,
					"settings": `{"clients":[]}`, "streamSettings": realityStream,
				})
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "obj": list})
		default:
			fmt.Fprint(w, `{"success":true}`)
		}
	}))
	defer ts.Close()

	id, err := connSvc(ts.URL).EnsureBackup("xhttp", 10092, &service.RealitySettings{
		Dest: "www.microsoft.com:443", ServerNames: []string{"www.microsoft.com"},
	})
	if err != nil {
		t.Fatalf("EnsureBackup: %v", err)
	}
	if id != 3 {
		t.Errorf("id = %d, want 3", id)
	}
	if addBody["remark"] != "shvpn-backup-xhttp" || addBody["port"] != float64(10092) || addBody["listen"] != "127.0.0.1" {
		t.Errorf("add body = %v", addBody)
	}
	var stream map[string]interface{}
	_ = json.Unmarshal([]byte(addBody["streamSettings"].(string)), &stream)
	if stream["network"] != "xhttp" || stream["security"] != "reality" {
		t.Errorf("stream = %v", stream)
	}
	xh := stream["xhttpSettings"].(map[string]interface{})
	if xh["path"] != "/shvpn" {
		t.Errorf("xhttpSettings = %v", xh)
	}
	rs := stream["realitySettings"].(map[string]interface{})
	if rs["privateKey"] == "" || rs["target"] != "www.microsoft.com:443" {
		t.Errorf("ключи/target: %v", rs)
	}
	nested := rs["settings"].(map[string]interface{})
	if nested["publicKey"] == "" || nested["fingerprint"] == "" {
		t.Errorf("settings не сгенерированы: %v", nested)
	}
}

const realityStreamNew = `{"network":"tcp","security":"reality","realitySettings":{"target":"google.com:443","serverNames":["google.com","g.co"],"privateKey":"PRIV","shortIds":["abcd"],"settings":{"publicKey":"PUB","fingerprint":"chrome","spiderX":"/"}}}`

func TestRealityNewFormat(t *testing.T) {
	list := panelWithInbound(realityStreamNew)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "obj": list})
	}))
	defer ts.Close()
	rs, err := connSvc(ts.URL).Reality(1)
	if err != nil {
		t.Fatalf("Reality new format: %v", err)
	}
	if rs.Dest != "google.com:443" || rs.Fingerprint != "chrome" || rs.PublicKey != "PUB" {
		t.Errorf("rs = %+v", rs)
	}
}

func TestUpdateRealityNewFormat(t *testing.T) {
	var updateBody map[string]interface{}
	list := panelWithInbound(realityStreamNew)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/inbounds/update/1") {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &updateBody)
			fmt.Fprint(w, `{"success":true}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "obj": list})
	}))
	defer ts.Close()

	err := connSvc(ts.URL).UpdateReality(1, &service.RealitySettings{
		Dest: "www.apple.com", ServerNames: []string{"www.apple.com"}, Fingerprint: "safari",
	})
	if err != nil {
		t.Fatalf("UpdateReality new format: %v", err)
	}
	var stream map[string]interface{}
	_ = json.Unmarshal([]byte(updateBody["streamSettings"].(string)), &stream)
	rs := stream["realitySettings"].(map[string]interface{})
	if rs["target"] != "www.apple.com:443" {
		t.Errorf("target = %v (ожидался host:443)", rs["target"])
	}
	// Пишем и legacy dest: иначе в инбаунде остаётся старый домен и следующее
	// применение формы (UI подставляет прочитанный dest) откатывает ротацию.
	if rs["dest"] != "www.apple.com:443" {
		t.Errorf("dest = %v, ожидался синхронный с target", rs["dest"])
	}
	nested := rs["settings"].(map[string]interface{})
	if nested["fingerprint"] != "safari" || nested["publicKey"] != "PUB" {
		t.Errorf("settings = %v", nested)
	}
}

func TestSetEnabled(t *testing.T) {
	var path string
	var body map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		fmt.Fprint(w, `{"success":true}`)
	}))
	defer ts.Close()

	if err := connSvc(ts.URL).SetEnabled(5, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if !strings.HasSuffix(path, "/panel/api/inbounds/setEnable/5") {
		t.Errorf("path = %q (нужен setEnable, не update)", path)
	}
	if body["enable"] != false {
		t.Errorf("enable = %v", body["enable"])
	}
}

func TestRealityPrefersTargetAndWritesBoth(t *testing.T) {
	// Инбаунд, где target и dest расходятся (результат старого бага).
	const divergent = `{"network":"tcp","security":"reality","realitySettings":{"dest":"www.google.com:443","target":"www.apple.com:443","serverNames":["www.apple.com"],"privateKey":"PRIV","settings":{"publicKey":"PUB","fingerprint":"chrome"}}}`
	list := panelWithInbound(divergent)
	var updateBody map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/inbounds/update/1") {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &updateBody)
			fmt.Fprint(w, `{"success":true}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "obj": list})
	}))
	defer ts.Close()

	s := connSvc(ts.URL)
	// Читаем target (приоритет xray-core), а не устаревший dest.
	cur, err := s.Reality(1)
	if err != nil {
		t.Fatalf("Reality: %v", err)
	}
	if cur.Dest != "www.apple.com:443" {
		t.Errorf("Dest = %q, ожидался target (www.apple.com:443)", cur.Dest)
	}
	// Повторное применение без изменения dest не должно откатывать target.
	if err := s.UpdateReality(1, &service.RealitySettings{ServerNames: []string{"www.apple.com", "itunes.apple.com"}, Dest: cur.Dest}); err != nil {
		t.Fatalf("UpdateReality: %v", err)
	}
	var stream map[string]interface{}
	_ = json.Unmarshal([]byte(updateBody["streamSettings"].(string)), &stream)
	rs := stream["realitySettings"].(map[string]interface{})
	if rs["target"] != "www.apple.com:443" || rs["dest"] != "www.apple.com:443" {
		t.Errorf("после повторного применения target=%v dest=%v (ожидались apple в обоих)", rs["target"], rs["dest"])
	}
}

// TestUpdateRealityRestoresPublicKey — если в инбаунде нет nested publicKey,
// UpdateReality обязан вывести его из privateKey: без publicKey 3x-ui выбрасывает
// pbk из vless-ссылок, и все клиенты теряют подключение.
func TestUpdateRealityRestoresPublicKey(t *testing.T) {
	priv, pub, err := generateRealityKeys()
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	// инбаунд «старого» вида: nested settings без publicKey
	stream := `{"network":"tcp","security":"reality","realitySettings":{` +
		`"privateKey":"` + priv + `","serverNames":["a.com"],"shortIds":["abcd"],` +
		`"target":"a.com:443","settings":{"fingerprint":"chrome"}}}`

	var sent map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/panel/api/inbounds/list"):
			io.WriteString(w, `{"success":true,"obj":[{"id":1,"remark":"vless","protocol":"vless","port":10090,"enable":true,"streamSettings":`+strconv.Quote(stream)+`}]}`)
		case strings.Contains(r.URL.Path, "/panel/api/inbounds/update/"):
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &sent)
			io.WriteString(w, `{"success":true,"msg":"ok"}`)
		default:
			io.WriteString(w, `{"success":true,"obj":{}}`)
		}
	}))
	defer srv.Close()

	svc := connSvc(srv.URL)
	if err := svc.UpdateReality(1, &service.RealitySettings{Fingerprint: "chrome"}); err != nil {
		t.Fatalf("UpdateReality: %v", err)
	}
	streams, _ := sent["streamSettings"].(string)
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(streams), &out); err != nil {
		t.Fatalf("streamSettings не JSON: %v", err)
	}
	rs := out["realitySettings"].(map[string]interface{})
	nested, _ := rs["settings"].(map[string]interface{})
	if got, _ := nested["publicKey"].(string); got != pub {
		t.Errorf("publicKey = %q, ожидался выведенный из privateKey %q", got, pub)
	}
}

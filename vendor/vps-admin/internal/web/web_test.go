package web

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/manager"
	"github.com/LankoRu/vps-admin/internal/registry"
	"github.com/LankoRu/vps-admin/internal/service"
	"github.com/LankoRu/vps-admin/internal/store"
)

func setup(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	cfg := &config.Config{}
	cfg.SetDefaults()

	st, err := store.New(filepath.Join(t.TempDir(), "test.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Сид: пользователь alice с идентичностями и трафиком.
	uid, err := st.CreateUser("alice", "Алиса", 0)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := st.AddIdentity(uid, "xray", "alice", "", "sub-link-alice"); err != nil {
		t.Fatalf("AddIdentity: %v", err)
	}
	_ = st.AddIdentity(uid, "amneziawg", "PUBKEYalice", "10.8.1.2", "[Interface]\nPrivateKey=x")
	st.UpdateDeltas("xray", map[string]uint64{"alice": 5_000_000_000})
	st.UpdateDeltas("amneziawg", map[string]uint64{"PUBKEYalice": 3_000_000_000})

	reg := registry.New(cfg, st)
	mgr := manager.New(st, reg)
	srv := New(cfg, st, mgr)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func getJSON(t *testing.T, url string, out interface{}) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func TestListUsers(t *testing.T) {
	ts, _ := setup(t)
	var users []userBrief
	getJSON(t, ts.URL+"/api/users", &users)
	if len(users) != 1 || users[0].Username != "alice" {
		t.Fatalf("ожидался alice, получено: %+v", users)
	}
	if users[0].TotalBytes != 8_000_000_000 {
		t.Errorf("total = %d, ожидалось 8e9", users[0].TotalBytes)
	}
	if len(users[0].Services) != 2 {
		t.Errorf("services = %v, ожидалось 2", users[0].Services)
	}
}

func TestOverview(t *testing.T) {
	ts, _ := setup(t)
	var ov struct {
		Total    uint64     `json:"total"`
		Services []svcBytes `json:"services"`
	}
	getJSON(t, ts.URL+"/api/overview?period=full", &ov)
	if ov.Total != 8_000_000_000 {
		t.Errorf("total = %d, ожидалось 8e9", ov.Total)
	}
	if len(ov.Services) != 2 {
		t.Errorf("services = %d, ожидалось 2", len(ov.Services))
	}
}

func TestUserDetailShowsConfigs(t *testing.T) {
	ts, _ := setup(t)
	var d struct {
		User       store.User       `json:"user"`
		Identities []store.Identity `json:"identities"`
	}
	getJSON(t, ts.URL+"/api/users/alice", &d)
	if d.User.DisplayName != "Алиса" {
		t.Errorf("display_name = %q", d.User.DisplayName)
	}
	// Настройки по сервисам должны отдаваться, чтобы их можно было раздать.
	var xrayCfg string
	for _, id := range d.Identities {
		if id.Service == "xray" {
			xrayCfg = id.Config
		}
	}
	if xrayCfg != "sub-link-alice" {
		t.Errorf("xray config = %q, ожидался 'sub-link-alice'", xrayCfg)
	}
}

func TestActivityLastSeen(t *testing.T) {
	ts, st := setup(t)
	// setup пишет дельты «сейчас» — убираем их, чтобы времена были предсказуемы.
	if _, err := st.Exec(`DELETE FROM traffic_deltas`); err != nil {
		t.Fatalf("очистка: %v", err)
	}

	// Нулевые дельты пишутся каждый сбор и активностью не считаются.
	if _, err := st.Exec(
		`INSERT INTO traffic_deltas (service, identity, total_bytes, timestamp) VALUES
		 ('xray','alice',0,'2026-07-20 10:00:00'),
		 ('xray','alice',123,'2026-07-19 08:30:00'),
		 ('amneziawg','PUBKEYalice',456,'2026-07-18 07:00:00')`); err != nil {
		t.Fatalf("сид: %v", err)
	}

	var list []userBrief
	getJSON(t, ts.URL+"/api/users", &list)
	if len(list) != 1 {
		t.Fatalf("ожидался один пользователь, получено %d", len(list))
	}
	// Отдаём RFC3339 в UTC — иначе браузер прочтёт время как локальное.
	if list[0].LastSeen != "2026-07-19T08:30:00Z" {
		t.Errorf("last_seen = %q, ожидалось 2026-07-19T08:30:00Z (нулевая дельта 20-го не в счёт)",
			list[0].LastSeen)
	}

	var d struct {
		LastSeen string            `json:"last_seen"`
		Activity map[string]string `json:"activity"`
	}
	getJSON(t, ts.URL+"/api/users/alice", &d)
	if d.LastSeen != "2026-07-19T08:30:00Z" {
		t.Errorf("карточка: last_seen = %q", d.LastSeen)
	}
	if d.Activity["xray"] != "2026-07-19T08:30:00Z" {
		t.Errorf("активность xray = %q", d.Activity["xray"])
	}
	if d.Activity["amneziawg"] != "2026-07-18T07:00:00Z" {
		t.Errorf("активность amneziawg = %q", d.Activity["amneziawg"])
	}

	// Пользователь без трафика — пустой признак, а не нулевая дата.
	if _, err := st.CreateUser("bob", "Борис", 0); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	getJSON(t, ts.URL+"/api/users", &list)
	for _, b := range list {
		if b.Username == "bob" && b.LastSeen != "" {
			t.Errorf("bob без трафика: last_seen = %q, ожидалась пустая строка", b.LastSeen)
		}
	}
}

func TestRenameUser(t *testing.T) {
	ts, st := setup(t)
	patch := func(body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/users/alice", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PATCH: %v", err)
		}
		resp.Body.Close()
		return resp
	}

	if resp := patch(`{"display_name":"  Алиса Новая  "}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status %d", resp.StatusCode)
	}
	u, err := st.GetUserByName("alice")
	if err != nil {
		t.Fatalf("GetUserByName: %v", err)
	}
	if u.DisplayName != "Алиса Новая" {
		t.Errorf("display_name = %q, ожидалось 'Алиса Новая' (с обрезкой пробелов)", u.DisplayName)
	}
	// Логин и идентичности не трогаем: они же ключи истории трафика.
	if u.Username != "alice" {
		t.Errorf("username = %q, должен остаться alice", u.Username)
	}
	ids, _ := st.GetIdentities(u.ID)
	if len(ids) != 2 {
		t.Errorf("идентичностей %d, ожидалось 2", len(ids))
	}

	// Пустое имя — возвращаем подпись к логину, а не пустую строку.
	if resp := patch(`{"display_name":"   "}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH пустым именем: status %d", resp.StatusCode)
	}
	u, _ = st.GetUserByName("alice")
	if u.DisplayName != "alice" {
		t.Errorf("display_name = %q, ожидался логин alice", u.DisplayName)
	}

	// Несуществующий пользователь — 404.
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/users/nobody", strings.NewReader(`{"display_name":"x"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH nobody: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("PATCH несуществующего: status %d, ожидался 404", resp.StatusCode)
	}
}

func TestDeleteUser(t *testing.T) {
	ts, st := setup(t)
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/users/alice", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status %d", resp.StatusCode)
	}
	if _, err := st.GetUserByName("alice"); err == nil {
		t.Error("пользователь alice должен быть удалён")
	}
}

func TestSelfSignedCertServesHTTPS(t *testing.T) {
	cert, err := selfSignedCert([]string{"vpn.example.com", "194.164.32.196"})
	if err != nil {
		t.Fatalf("selfSignedCert: %v", err)
	}
	// IP и домен должны попасть в SAN — иначе браузер отвергнет подключение по IP.
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	foundIP := false
	for _, ip := range leaf.IPAddresses {
		if ip.String() == "194.164.32.196" {
			foundIP = true
		}
	}
	if !foundIP {
		t.Errorf("IP 194.164.32.196 нет в SAN: %v", leaf.IPAddresses)
	}
	hasDNS := false
	for _, d := range leaf.DNSNames {
		if d == "vpn.example.com" {
			hasDNS = true
		}
	}
	if !hasDNS {
		t.Errorf("домен нет в SAN: %v", leaf.DNSNames)
	}

	cfg := &config.Config{}
	cfg.SetDefaults()
	st, err := store.New(filepath.Join(t.TempDir(), "tls.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := New(cfg, st, manager.New(st, registry.New(cfg, st)))

	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	defer ts.Close()

	if !strings.HasPrefix(ts.URL, "https://") {
		t.Fatalf("ожидался https URL, получен %s", ts.URL)
	}
	resp, err := ts.Client().Get(ts.URL + "/api/users") // ts.Client доверяет серту
	if err != nil {
		t.Fatalf("GET по HTTPS: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTPS status %d", resp.StatusCode)
	}
}

func TestPublicShareLink(t *testing.T) {
	// Панель под паролем — публичная ссылка всё равно должна открываться без входа.
	cfg := &config.Config{}
	cfg.SetDefaults()
	cfg.Web.Password = "s3cret"

	st, err := store.New(filepath.Join(t.TempDir(), "share.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	uid, err := st.CreateUser("alice", "Алиса", 0)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := st.AddIdentity(uid, "xray", "alice", "", `[{"label":"Ссылка (vless)","value":"vless://x@h:443"}]`); err != nil {
		t.Fatalf("AddIdentity: %v", err)
	}
	ts := httptest.NewServer(New(cfg, st, manager.New(st, registry.New(cfg, st))).Handler())
	t.Cleanup(ts.Close)

	// Без авторизации ссылку не создать.
	r, _ := http.Post(ts.URL+"/api/users/alice/share", "application/json", nil)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("создание ссылки без входа: ждали 401, получили %d", r.StatusCode)
	}
	r.Body.Close()

	// Логинимся и создаём ссылку.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	r, _ = client.Post(ts.URL+"/api/login", "application/json", strings.NewReader(`{"password":"s3cret"}`))
	r.Body.Close()

	r, err = client.Post(ts.URL+"/api/users/alice/share", "application/json", nil)
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("создание ссылки: %v / status %d", err, r.StatusCode)
	}
	var share struct{ Token, Path string }
	_ = json.NewDecoder(r.Body).Decode(&share)
	r.Body.Close()
	if len(share.Token) != 32 {
		t.Fatalf("токен должен быть 32 hex-символа, получено %q", share.Token)
	}

	// Публичные страница и данные доступны БЕЗ cookie (отдельный клиент).
	r, err = http.Get(ts.URL + share.Path)
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("публичная страница без входа: %v / status %d", err, r.StatusCode)
	}
	r.Body.Close()

	r, _ = http.Get(ts.URL + share.Path + "/data")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("публичные данные: status %d", r.StatusCode)
	}
	var data struct {
		DisplayName string `json:"display_name"`
		Identities  []struct {
			Service string `json:"service"`
			Config  string `json:"config"`
		} `json:"identities"`
	}
	_ = json.NewDecoder(r.Body).Decode(&data)
	r.Body.Close()
	if data.DisplayName != "Алиса" || len(data.Identities) != 1 {
		t.Fatalf("публичные данные неполные: %+v", data)
	}
	if !strings.Contains(data.Identities[0].Config, "vless://") {
		t.Errorf("в публичных данных нет ключа: %s", data.Identities[0].Config)
	}

	// Чужой/выдуманный токен — 404.
	r, _ = http.Get(ts.URL + "/p/" + strings.Repeat("0", 32) + "/data")
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("неизвестный токен: ждали 404, получили %d", r.StatusCode)
	}
	r.Body.Close()

	// После отзыва ссылка перестаёт работать.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/users/alice/share", nil)
	r, err = client.Do(req)
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("отзыв ссылки: %v / status %d", err, r.StatusCode)
	}
	r.Body.Close()

	r, _ = http.Get(ts.URL + share.Path + "/data")
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("после отзыва ждали 404, получили %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestUserDetailReportsSelfSignedCert(t *testing.T) {
	// Панель предупреждает админа, что самоподписанную ссылку не откроет
	// встроенный браузер Telegram — флаг должен стоять только в этом случае.
	cases := []struct {
		name     string
		tlsOn    bool
		certFile string
		want     bool
	}{
		{"tls off", false, "", false},
		{"tls + самоподписанный", true, "", true},
		{"tls + свой сертификат", true, "/etc/ssl/panel.crt", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.SetDefaults()
			cfg.Web.TLS.Enabled = c.tlsOn
			cfg.Web.TLS.CertFile = c.certFile

			st, err := store.New(filepath.Join(t.TempDir(), "ss.db"), cfg)
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			t.Cleanup(func() { st.Close() })
			if _, err := st.CreateUser("alice", "Алиса", 0); err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			ts := httptest.NewServer(New(cfg, st, manager.New(st, registry.New(cfg, st))).Handler())
			t.Cleanup(ts.Close)

			r, err := http.Get(ts.URL + "/api/users/alice")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer r.Body.Close()
			var d struct {
				SelfSigned bool `json:"self_signed"`
			}
			_ = json.NewDecoder(r.Body).Decode(&d)
			if d.SelfSigned != c.want {
				t.Errorf("self_signed = %v, ожидалось %v", d.SelfSigned, c.want)
			}
		})
	}
}

func TestCertReloaderPicksUpRenewedCert(t *testing.T) {
	// acme.sh перевыпускает shortlived-сертификат (~6 дней) под работающим
	// сервисом. Без перечитывания панель отдавала бы просроченный до рестарта.
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")

	issue := func(host string) {
		_, der, priv, err := generateSelfSigned([]string{host})
		if err != nil {
			t.Fatalf("generateSelfSigned: %v", err)
		}
		if err := saveCert(crt, key, der, priv); err != nil {
			t.Fatalf("saveCert: %v", err)
		}
	}

	issue("first.example.com")
	rl, err := newCertReloader(crt, key)
	if err != nil {
		t.Fatalf("newCertReloader: %v", err)
	}
	got, _ := rl.GetCertificate(nil)
	leaf, _ := x509.ParseCertificate(got.Certificate[0])
	if leaf.DNSNames[len(leaf.DNSNames)-1] != "first.example.com" {
		t.Fatalf("исходный сертификат не тот: %v", leaf.DNSNames)
	}

	// «Продление»: перезаписываем файлы новым сертификатом и сдвигаем mtime
	// вперёд (иначе на грубой granularity ФС время может совпасть).
	issue("renewed.example.com")
	future := time.Now().Add(2 * time.Second)
	for _, p := range []string{crt, key} {
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}

	got, _ = rl.GetCertificate(nil) // без перезапуска сервиса
	leaf, _ = x509.ParseCertificate(got.Certificate[0])
	if leaf.DNSNames[len(leaf.DNSNames)-1] != "renewed.example.com" {
		t.Errorf("сертификат не перечитан после обновления: %v", leaf.DNSNames)
	}

	// Битые файлы не должны ронять выдачу — отдаём последний рабочий.
	if err := os.WriteFile(crt, []byte("не сертификат"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	os.Chtimes(crt, future.Add(time.Second), future.Add(time.Second))
	got, err = rl.GetCertificate(nil)
	if err != nil || got == nil {
		t.Fatalf("при битом файле ждали прежний сертификат, получили err=%v", err)
	}
	leaf, _ = x509.ParseCertificate(got.Certificate[0])
	if leaf.DNSNames[len(leaf.DNSNames)-1] != "renewed.example.com" {
		t.Errorf("ожидали последний рабочий сертификат, получили %v", leaf.DNSNames)
	}
}

func TestSelfSignedCertCachedOnDisk(t *testing.T) {
	cfg := &config.Config{}
	cfg.DBPath = filepath.Join(t.TempDir(), "stats.db")
	srv := &Server{cfg: cfg}
	hosts := []string{"vpn.example.com", "194.164.32.196"}

	// Первый вызов — генерирует и сохраняет.
	c1, err := srv.loadOrCreateSelfSigned(hosts)
	if err != nil {
		t.Fatalf("первый вызов: %v", err)
	}
	crtPath, keyPath := srv.certCachePaths()
	if _, err := os.Stat(crtPath); err != nil {
		t.Fatalf("сертификат не сохранён: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("ключ не сохранён: %v", err)
	}

	// Второй вызов (эмуляция рестарта) — должен вернуть ТОТ ЖЕ сертификат из кэша.
	c2, err := srv.loadOrCreateSelfSigned(hosts)
	if err != nil {
		t.Fatalf("второй вызов: %v", err)
	}
	if !bytes.Equal(c1.Certificate[0], c2.Certificate[0]) {
		t.Error("сертификат перевыпущен, хотя должен был взяться из кэша")
	}

	// Изменение набора хостов → перевыпуск (новый серт покрывает новый IP).
	c3, err := srv.loadOrCreateSelfSigned(append(hosts, "10.0.0.5"))
	if err != nil {
		t.Fatalf("третий вызов: %v", err)
	}
	if bytes.Equal(c1.Certificate[0], c3.Certificate[0]) {
		t.Error("при новом хосте сертификат должен быть перевыпущен")
	}
}

func TestSessionTokenStableAcrossRestart(t *testing.T) {
	cfg := &config.Config{}
	cfg.Web.Password = "pw"
	// Два «старта» с тем же паролем → одинаковый токен (cookie переживает деплой).
	if New(cfg, nil, nil).sessionToken != New(cfg, nil, nil).sessionToken {
		t.Error("токен должен быть стабилен при неизменном пароле")
	}
	// Смена пароля → другой токен (старые cookie инвалидируются).
	cfg2 := &config.Config{}
	cfg2.Web.Password = "other"
	if New(cfg2, nil, nil).sessionToken == New(cfg, nil, nil).sessionToken {
		t.Error("разные пароли должны давать разные токены")
	}
}

func TestAuthFlow(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetDefaults()
	cfg.Web.Password = "s3cret"

	st, err := store.New(filepath.Join(t.TempDir(), "auth.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	reg := registry.New(cfg, st)
	ts := httptest.NewServer(New(cfg, st, manager.New(st, reg)).Handler())
	t.Cleanup(ts.Close)

	// Без cookie API закрыт.
	r, _ := http.Get(ts.URL + "/api/users")
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("без входа ждали 401, получили %d", r.StatusCode)
	}
	r.Body.Close()

	// Корень отдаёт форму входа (только поле пароля).
	r, _ = http.Get(ts.URL + "/")
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(body), "type=\"password\"") {
		t.Fatal("на / нет формы ввода пароля")
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// Неверный пароль.
	r, _ = client.Post(ts.URL+"/api/login", "application/json", strings.NewReader(`{"password":"nope"}`))
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("неверный пароль ждали 401, получили %d", r.StatusCode)
	}
	r.Body.Close()

	// Верный пароль → cookie.
	r, _ = client.Post(ts.URL+"/api/login", "application/json", strings.NewReader(`{"password":"s3cret"}`))
	if r.StatusCode != http.StatusOK {
		t.Fatalf("вход ждали 200, получили %d", r.StatusCode)
	}
	r.Body.Close()

	// Теперь API доступен.
	r, _ = client.Get(ts.URL + "/api/users")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("после входа ждали 200, получили %d", r.StatusCode)
	}
	r.Body.Close()

	// Выход сбрасывает cookie…
	r, _ = client.Post(ts.URL+"/api/logout", "application/json", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("выход ждали 200, получили %d", r.StatusCode)
	}
	r.Body.Close()

	// …и после него API снова закрыт.
	r, _ = client.Get(ts.URL + "/api/users")
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("после выхода ждали 401, получили %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestCreateUserNoProvisioners(t *testing.T) {
	// Все сервисы выключены → ни один не создаётся → ошибка и откат.
	ts, st := setup(t)
	body := `{"username":"charlie","display_name":"Чарли"}`
	resp, err := http.Post(ts.URL+"/api/users", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ожидался 400, получен %d", resp.StatusCode)
	}
	if _, err := st.GetUserByName("charlie"); err == nil {
		t.Error("charlie не должен остаться в БД после отката")
	}
}

func TestComponentPageLinks(t *testing.T) {
	// Ссылки на другие веб-страницы стека — в разделе «Компоненты»
	// (/api/components), не в шапке. Ссылки на саму эту панель нет.
	cases := []struct {
		name, host, subURL, basePath string
		wantPanel, wantSub           bool
	}{
		{"public_host + путь", "203.0.113.5", "", "panel123", true, false},
		{"хост из sub_url", "", "https://vpn.example.com/sub/", "", true, true},
		{"путь со слэшами", "203.0.113.5", "", "/panel123/", true, false},
		{"нет хоста — ссылок нет", "", "", "panel123", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.SetDefaults()
			cfg.Xray3xUI.PublicHost = c.host
			cfg.Xray3xUI.SubURL = c.subURL
			cfg.Xray3xUI.WebBasePath = c.basePath
			st, err := store.New(filepath.Join(t.TempDir(), "test.db"), cfg)
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			t.Cleanup(func() { st.Close() })
			ts := httptest.NewServer(New(cfg, st, manager.New(st, registry.New(cfg, st))).Handler())
			t.Cleanup(ts.Close)

			var resp struct {
				Links []struct {
					Label string `json:"label"`
					URL   string `json:"url"`
				} `json:"links"`
			}
			getJSON(t, ts.URL+"/api/components", &resp)
			var panel, sub string
			for _, l := range resp.Links {
				if strings.Contains(l.Label, "панель") {
					panel = l.URL
				}
				if l.Label == "Подписки" {
					sub = l.URL
				}
				if strings.Contains(l.Label, "эта панель") {
					t.Errorf("ссылка на саму панель не нужна: %v", l)
				}
			}
			if c.wantPanel && !strings.HasPrefix(panel, "https://") {
				t.Errorf("нет ссылки на панель 3x-ui: %v", resp.Links)
			}
			if !c.wantPanel && panel != "" {
				t.Errorf("не ожидалась ссылка на 3x-ui: %q", panel)
			}
			if c.wantSub && sub != c.subURL {
				t.Errorf("подписки = %q, want %q", sub, c.subURL)
			}
		})
	}
}

func TestCreateUserByDisplayNameOnly(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetDefaults()
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fake := &fakeCreateProv{}
	mgr := manager.New(st, registry.NewWithProvisioners(map[string]service.Provisioner{"xray": fake}))
	ts := httptest.NewServer(New(cfg, st, mgr).Handler())
	t.Cleanup(ts.Close)

	// только display_name → логин генерируется транслитом
	resp, err := http.Post(ts.URL+"/api/users", "application/json",
		strings.NewReader(`{"display_name":"Вася Пупкин"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		User struct {
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.User.Username != "vasya-pupkin" || out.User.DisplayName != "Вася Пупкин" {
		t.Errorf("user = %q / %q", out.User.Username, out.User.DisplayName)
	}
}

// fakeCreateProv — провижинер-заглушка для теста создания по отображаемому имени.
type fakeCreateProv struct{}

func (f *fakeCreateProv) Provision(username string) (*service.Account, error) {
	return &service.Account{Service: "xray", Identity: username}, nil
}
func (f *fakeCreateProv) Deprovision(identity string) error { return nil }

func TestLoginFormNativeWithUsername(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetDefaults()
	cfg.Web.Password = "secret"
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(New(cfg, st, manager.New(st, registry.New(cfg, st))).Handler())
	t.Cleanup(ts.Close)

	// страница входа: нативная форма, фиксированный логин, autocomplete-поля
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	for _, want := range []string{
		`<form method="post" action="/api/login">`,
		`name="username"`, `value="admi"`, `autocomplete="username"`,
		`name="password"`, `autocomplete="current-password"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("страница входа не содержит %q", want)
		}
	}

	// нативная отправка формы: верный логин+пароль → 303 + cookie + редирект на Referer-путь
	form := strings.NewReader("username=admi&password=secret")
	req, _ := http.NewRequest("POST", ts.URL+"/api/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", ts.URL+"/some/page?x=1")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r2, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("form POST: %v", err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusSeeOther {
		t.Fatalf("form login status = %d, want 303", r2.StatusCode)
	}
	if loc := r2.Header.Get("Location"); loc != "/some/page?x=1" {
		t.Errorf("Location = %q, want /some/page?x=1", loc)
	}
	var hasCookie bool
	for _, c := range r2.Cookies() {
		if c.Name == "vps_session" && c.Value != "" {
			hasCookie = true
		}
	}
	if !hasCookie {
		t.Error("нет session-cookie после form-входа")
	}

	// неверный логин (форма) → страница с ошибкой, без cookie
	bad := strings.NewReader("username=hacker&password=secret")
	r3, err := http.Post(ts.URL+"/api/login", "application/x-www-form-urlencoded", bad)
	if err != nil {
		t.Fatalf("bad form POST: %v", err)
	}
	b3, _ := io.ReadAll(r3.Body)
	r3.Body.Close()
	if !strings.Contains(string(b3), "Неверный логин") {
		t.Errorf("ожидалась ошибка логина на странице, got: %s", b3)
	}
	for _, c := range r3.Cookies() {
		if c.Name == "vps_session" && c.Value != "" {
			t.Error("cookie выдан при неверном логине")
		}
	}

	// JSON с неверным логином → 401
	r4, err := http.Post(ts.URL+"/api/login", "application/json", strings.NewReader(`{"username":"hacker","password":"secret"}`))
	if err != nil {
		t.Fatalf("json POST: %v", err)
	}
	r4.Body.Close()
	if r4.StatusCode != http.StatusUnauthorized {
		t.Errorf("json wrong login status = %d, want 401", r4.StatusCode)
	}
}

// fakeDeviceProv — провижинер с поддержкой устройств для теста device-API.
type fakeDeviceProv struct{ deleted []string }

func (f *fakeDeviceProv) Provision(username string) (*service.Account, error) {
	return &service.Account{Service: "amneziawg", Identity: username + "-awg",
		Items: []service.Item{{Label: "conf", Value: "main"}}}, nil
}
func (f *fakeDeviceProv) ProvisionDevice(username, device string) (*service.Account, error) {
	return &service.Account{Service: "amneziawg", Identity: username + "-awg-" + device,
		Items: []service.Item{{Label: "conf", Value: "dev"}}}, nil
}
func (f *fakeDeviceProv) Deprovision(identity string) error {
	f.deleted = append(f.deleted, identity)
	return nil
}

func TestDeviceAPI(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetDefaults()
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	prov := &fakeDeviceProv{}
	mgr := manager.New(st, registry.NewWithProvisioners(map[string]service.Provisioner{"amneziawg": prov}))
	ts := httptest.NewServer(New(cfg, st, mgr).Handler())
	t.Cleanup(ts.Close)

	// создаём юзера (legacy-API с явным username)
	r, err := http.Post(ts.URL+"/api/users", "application/json", strings.NewReader(`{"username":"bob","display_name":"Bob"}`))
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("create user: %v status=%d", err, r.StatusCode)
	}
	r.Body.Close()

	// добавляем устройство
	r2, err := http.Post(ts.URL+"/api/users/bob/devices", "application/json", strings.NewReader(`{"device":"phone"}`))
	if err != nil {
		t.Fatalf("add device: %v", err)
	}
	var out struct {
		Account struct {
			Identity string `json:"identity"`
		} `json:"account"`
	}
	json.NewDecoder(r2.Body).Decode(&out)
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK || out.Account.Identity != "bob-awg-phone" {
		t.Fatalf("add device status=%d identity=%q", r2.StatusCode, out.Account.Identity)
	}

	// в detail две identities
	r3, _ := http.Get(ts.URL + "/api/users/bob")
	var det struct {
		Identities []struct {
			Identity string `json:"identity"`
		} `json:"identities"`
	}
	json.NewDecoder(r3.Body).Decode(&det)
	r3.Body.Close()
	if len(det.Identities) != 2 {
		t.Fatalf("identities = %d, want 2", len(det.Identities))
	}

	// удаляем устройство
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/users/bob/devices/phone", nil)
	r4, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("del device: %v", err)
	}
	r4.Body.Close()
	if r4.StatusCode != http.StatusOK {
		t.Fatalf("del device status=%d", r4.StatusCode)
	}
	if len(prov.deleted) != 1 || prov.deleted[0] != "bob-awg-phone" {
		t.Errorf("deprovisioned = %v", prov.deleted)
	}
	r5, _ := http.Get(ts.URL + "/api/users/bob")
	json.NewDecoder(r5.Body).Decode(&det)
	r5.Body.Close()
	if len(det.Identities) != 1 || det.Identities[0].Identity != "bob-awg" {
		t.Errorf("после удаления identities = %+v", det.Identities)
	}
}

func TestConnectionsAPI(t *testing.T) {
	// фейковая панель 3x-ui с Reality-инбаундом
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"obj":[{"id":1,"port":10090,"protocol":"vless","remark":"main","enable":true,"settings":"{\"clients\":[]}","streamSettings":"{\"network\":\"tcp\",\"security\":\"reality\",\"realitySettings\":{\"dest\":\"www.google.com:443\",\"serverNames\":[\"www.google.com\"],\"fingerprint\":\"chrome\",\"publicKey\":\"PUB\",\"shortIds\":[\"ab\"]}}"}]}`))
	}))
	t.Cleanup(panel.Close)
	// telemt-конфиг во временном файле
	tomlPath := filepath.Join(t.TempDir(), "telemt.toml")
	os.WriteFile(tomlPath, []byte("[censorship]\ntls_domain = \"petrovich.ru\"\nmask_host = \"petrovich.ru\"\n"), 0o600)

	cfg := &config.Config{}
	cfg.SetDefaults()
	cfg.Xray3xUI.BaseURL = panel.URL
	cfg.Xray3xUI.APIToken = "tok"
	cfg.Xray3xUI.InboundID = 1
	cfg.TeleMT.ConfigPath = tomlPath
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(New(cfg, st, manager.New(st, registry.New(cfg, st))).Handler())
	t.Cleanup(ts.Close)

	// GET состояния
	r, err := http.Get(ts.URL + "/api/connections")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("GET state: %v status=%d", err, r.StatusCode)
	}
	var info struct {
		Reality struct {
			Dest string `json:"dest"`
		} `json:"reality"`
		TelemtDomain string `json:"telemt_domain"`
		Presets      []struct {
			Name string `json:"name"`
		} `json:"presets"`
	}
	json.NewDecoder(r.Body).Decode(&info)
	r.Body.Close()
	if info.Reality.Dest != "www.google.com:443" || info.TelemtDomain != "petrovich.ru" || len(info.Presets) == 0 {
		t.Errorf("state = %+v", info)
	}

	// POST с невалидным fingerprint → 400 (валидация до probe/панели)
	r2, _ := http.Post(ts.URL+"/api/connections/reality", "application/json", strings.NewReader(`{"fingerprint":"netscape"}`))
	if r2.StatusCode != http.StatusBadRequest {
		t.Errorf("bad fp status = %d, want 400", r2.StatusCode)
	}
	r2.Body.Close()
	// POST telemt с некорректным доменом → 400
	r3, _ := http.Post(ts.URL+"/api/connections/telemt", "application/json", strings.NewReader(`{"domain":"not a domain"}`))
	if r3.StatusCode != http.StatusBadRequest {
		t.Errorf("bad domain status = %d, want 400", r3.StatusCode)
	}
	r3.Body.Close()
	// POST telemt с доменом из Reality-SNI → 400 (коллизия)
	r4, _ := http.Post(ts.URL+"/api/connections/telemt", "application/json", strings.NewReader(`{"domain":"www.google.com"}`))
	if r4.StatusCode != http.StatusBadRequest {
		t.Errorf("collision status = %d, want 400", r4.StatusCode)
	}
	r4.Body.Close()
	// POST backup без SNI → 400
	r5, _ := http.Post(ts.URL+"/api/connections/backup", "application/json", strings.NewReader(`{"enabled":true,"transport":"xhttp","server_names":[]}`))
	if r5.StatusCode != http.StatusBadRequest {
		t.Errorf("empty backup sni status = %d, want 400", r5.StatusCode)
	}
	r5.Body.Close()
}

func TestComponentsAPI(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetDefaults()
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(New(cfg, st, manager.New(st, registry.New(cfg, st))).Handler())
	t.Cleanup(ts.Close)

	r, err := http.Get(ts.URL + "/api/components")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("GET components: %v status=%d", err, r.StatusCode)
	}
	var resp struct {
		Components []struct {
			Name      string `json:"name"`
			Updatable bool   `json:"updatable"`
		} `json:"components"`
		Links []struct {
			Label string `json:"label"`
			URL   string `json:"url"`
		} `json:"links"`
	}
	json.NewDecoder(r.Body).Decode(&resp)
	r.Body.Close()
	list := resp.Components
	if len(list) != 5 {
		t.Fatalf("компонентов = %d, want 5", len(list))
	}
	byName := map[string]bool{}
	for _, c := range list {
		byName[c.Name] = c.Updatable
	}
	if !byName["vps-admin"] || !byName["3x-ui"] || !byName["xray"] || !byName["telemt"] {
		t.Errorf("updatable = %v", byName)
	}
	if byName["amneziawg"] {
		t.Error("amneziawg не должен быть updatable")
	}
	if _, ok := byName["nginx"]; ok {
		t.Error("nginx не должен появляться в списке компонентов")
	}

	// неизвестный компонент → 400
	r2, _ := http.Post(ts.URL+"/api/components/bogus/update", "application/json", nil)
	if r2.StatusCode != http.StatusBadRequest {
		t.Errorf("bogus update status = %d, want 400", r2.StatusCode)
	}
	r2.Body.Close()
	// awg → 400 (только версии)
	r3, _ := http.Post(ts.URL+"/api/components/amneziawg/update", "application/json", nil)
	if r3.StatusCode != http.StatusBadRequest {
		t.Errorf("awg update status = %d, want 400", r3.StatusCode)
	}
	r3.Body.Close()
}

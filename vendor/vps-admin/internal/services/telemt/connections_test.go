package telemt

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LankoRu/vps-admin/internal/config"
)

const testToml = `[censorship]
tls_domain = "petrovich.ru"
mask = true
mask_host = "petrovich.ru"
mask_port = 443

[access.users]
alice = "aabbccdd00112233aabbccdd00112233"
`

func connCfg(t *testing.T, apiURL string) (*config.Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "telemt.toml")
	if err := os.WriteFile(path, []byte(testToml), 0o660); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.TeleMT.BaseURL = apiURL
	cfg.TeleMT.ConfigPath = path
	return cfg, path
}

func TestDomainAndSetDomain(t *testing.T) {
	cfg, path := connCfg(t, "http://127.0.0.1:1")
	s := New(cfg)
	d, err := s.Domain()
	if err != nil || d != "petrovich.ru" {
		t.Fatalf("Domain = %q, err = %v", d, err)
	}
	old := restartTelemt
	restartTelemt = func() error { return nil }
	t.Cleanup(func() { restartTelemt = old })
	stBefore, _ := os.Stat(path)
	wantPerm := stBefore.Mode().Perm()

	if err := s.SetDomain("example.com"); err != nil {
		t.Fatalf("SetDomain: %v", err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	if !strings.Contains(got, `tls_domain = "example.com"`) || !strings.Contains(got, `mask_host = "example.com"`) {
		t.Errorf("домен не заменён:\n%s", got)
	}
	// остальное сохранено
	if !strings.Contains(got, `alice = "aabbccdd00112233aabbccdd00112233"`) || !strings.Contains(got, "mask_port = 443") {
		t.Errorf("конфиг повреждён:\n%s", got)
	}
	// права сохранены (как были до замены)
	st, _ := os.Stat(path)
	if st.Mode().Perm() != wantPerm {
		t.Errorf("права = %v, want %v", st.Mode().Perm(), wantPerm)
	}
}

func TestRefreshItemsReadsLinksAndSecret(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"users":[{"username":"alice","total_octets":100,"links":{
			"tls":["tg://proxy?server=1.2.3.4&port=443&secret=eeabc"],
			"tls_domains":[{"domain":"example.com","link":"tg://proxy?server=1.2.3.4&port=443&secret=eeabc6578616d706c65"}]}}]}`)
	}))
	defer ts.Close()
	cfg, _ := connCfg(t, ts.URL)
	s := New(cfg)

	items, err := s.RefreshItems("alice")
	if err != nil {
		t.Fatalf("RefreshItems: %v", err)
	}
	var hasSecret, hasDomainLink bool
	for _, it := range items {
		if it.Label == "Секрет" && it.Value == "aabbccdd00112233aabbccdd00112233" {
			hasSecret = true
		}
		if strings.Contains(it.Label, "example.com") {
			hasDomainLink = true
		}
	}
	if !hasSecret || !hasDomainLink {
		t.Errorf("items = %+v (секрет из toml / ссылка с доменом)", items)
	}
}

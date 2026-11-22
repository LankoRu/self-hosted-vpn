package manager

import (
	"fmt"
	"testing"

	"github.com/LankoRu/vps-admin/internal/service"
)

// refreshProv — провижинер, чьи ссылки меняются на стороне сервера.
type refreshProv struct {
	fakeProv
	links map[string]string // identity → актуальная ссылка
	fail  map[string]bool
}

func (r *refreshProv) RefreshItems(identity string) ([]service.Item, error) {
	if r.fail[identity] {
		return nil, fmt.Errorf("панель недоступна")
	}
	return []service.Item{{Label: "key", Value: r.links[identity]}}, nil
}

func TestRefreshLinksUpdatesOnlyChangedAndKeepsOnError(t *testing.T) {
	p := &refreshProv{fakeProv: fakeProv{name: "xray"}}
	m, st := newTestManager(t, map[string]service.Provisioner{"xray": p})

	for _, u := range []string{"alice", "bob", "carol"} {
		if _, _, err := m.Register(u, ""); err != nil {
			t.Fatalf("Register %s: %v", u, err)
		}
	}
	alice, _ := st.GetUserByName("alice")
	bob, _ := st.GetUserByName("bob")
	carol, _ := st.GetUserByName("carol")
	bobOld := configValue(t, st, bob.ID, "xray")
	carolOld := configValue(t, st, carol.ID, "xray")

	p.links = map[string]string{"alice": "vless://a@host:443", "bob": bobOld}
	p.fail = map[string]bool{"carol": true}

	updated, failed := m.RefreshLinks()
	if updated != 1 || failed != 1 {
		t.Fatalf("updated=%d failed=%d, ожидалось 1 и 1", updated, failed)
	}
	if got := configValue(t, st, alice.ID, "xray"); got != "vless://a@host:443" {
		t.Errorf("alice: %q — ссылка не обновилась", got)
	}
	if got := configValue(t, st, carol.ID, "xray"); got != carolOld {
		t.Errorf("carol: %q — при ошибке сохранённое трогать нельзя", got)
	}
	if len(p.deprovisioned) != 0 || len(p.provisioned) != 3 {
		t.Errorf("учётки пересоздавались: deprov=%v prov=%v", p.deprovisioned, p.provisioned)
	}

	// Повтор без изменений на сервере — ничего не обновляется.
	if updated, _ := m.RefreshLinks(); updated != 0 {
		t.Errorf("повторный прогон обновил %d, ожидалось 0", updated)
	}
}

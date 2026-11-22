package manager

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/registry"
	"github.com/LankoRu/vps-admin/internal/service"
	"github.com/LankoRu/vps-admin/internal/store"
)

// provScript — провижинер с управляемым поведением Provision/Deprovision.
type provScript struct {
	calls       int
	failFirst   int  // сколько первых Provision провалить
	failAll     bool // проваливать всегда
	deprovision int
}

func (p *provScript) Provision(username string) (*service.Account, error) {
	p.calls++
	if p.failAll {
		return nil, errors.New("панель недоступна")
	}
	if p.calls <= p.failFirst {
		return nil, errors.New("панель недоступна")
	}
	return &service.Account{Service: "xray", Identity: username, Items: []service.Item{{Label: "key", Value: username}}}, nil
}
func (p *provScript) Deprovision(string) error { p.deprovision++; return nil }

// TestRegenerateRestoresAccountOnFailure — если пересоздание сорвалось, менеджер
// обязан попытаться вернуть учётку, а не оставить пользователя без доступа.
func TestRegenerateRestoresAccountOnFailure(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "t.db"), &config.Config{})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	prov := &provScript{}
	m := New(st, registry.NewWithProvisioners(map[string]service.Provisioner{"xray": prov}))

	u, _, err := m.Register("bob", "Bob")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// первое пересоздание падает, откат (второй Provision) успешен
	prov.calls, prov.failFirst = 0, 1
	if _, err := m.Regenerate("bob", "xray"); err == nil {
		t.Fatal("ожидалась ошибка пересоздания")
	}
	if prov.calls < 2 {
		t.Errorf("нет попытки восстановления: Provision вызван %d раз", prov.calls)
	}
	if prov.deprovision != 1 {
		t.Errorf("Deprovision = %d, ожидалась 1 попытка удаления", prov.deprovision)
	}
	// identity на месте (восстановлена)
	ids, _ := st.GetIdentities(u.ID)
	if len(ids) != 1 || ids[0].Identity != "bob" {
		t.Errorf("identity после откатa: %+v", ids)
	}
}

// TestRegenerateFailsWhenRestoreAlsoFails — двойной сбой даёт явную ошибку.
func TestRegenerateFailsWhenRestoreAlsoFails(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "t.db"), &config.Config{})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	prov := &provScript{}
	m := New(st, registry.NewWithProvisioners(map[string]service.Provisioner{"xray": prov}))
	if _, _, err := m.Register("carol", "Carol"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	prov.failAll = true
	_, err = m.Regenerate("carol", "xray")
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
}

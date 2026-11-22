package manager

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/registry"
	"github.com/LankoRu/vps-admin/internal/service"
	"github.com/LankoRu/vps-admin/internal/store"
)

// fakeProv — провижинер-заглушка: считает вызовы и на каждый Provision выдаёт
// новый (версионированный) артефакт, чтобы было видно перегенерацию.
type fakeProv struct {
	name          string
	seq           int
	provisioned   []string
	deprovisioned []string
	failProvision bool
}

func (f *fakeProv) Provision(username string) (*service.Account, error) {
	if f.failProvision {
		return nil, fmt.Errorf("boom")
	}
	f.seq++
	f.provisioned = append(f.provisioned, username)
	return &service.Account{
		Service:  f.name,
		Identity: username, // identity детерминирован от имени, как у xray/amneziawg
		Items:    []service.Item{{Label: "key", Value: fmt.Sprintf("%s-v%d", username, f.seq)}},
	}, nil
}

func (f *fakeProv) Deprovision(identity string) error {
	f.deprovisioned = append(f.deprovisioned, identity)
	return nil
}

func newTestManager(t *testing.T, provs map[string]service.Provisioner) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "t.db"), &config.Config{})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, registry.NewWithProvisioners(provs)), st
}

// configValue возвращает value первого item из сохранённого config пользователя.
func configValue(t *testing.T, st *store.Store, userID int64, svc string) string {
	t.Helper()
	ids, _ := st.GetIdentities(userID)
	for _, id := range ids {
		if id.Service != svc {
			continue
		}
		var items []service.Item
		if err := json.Unmarshal([]byte(id.Config), &items); err != nil || len(items) == 0 {
			t.Fatalf("config %s не разобран: %q", svc, id.Config)
		}
		return items[0].Value
	}
	t.Fatalf("нет идентичности %s", svc)
	return ""
}

func TestRegenerateRotatesConfigKeepingIdentity(t *testing.T) {
	xr := &fakeProv{name: "xray"}
	mgr, st := newTestManager(t, map[string]service.Provisioner{"xray": xr})

	user, _, err := mgr.Register("alice", "Alice")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := configValue(t, st, user.ID, "xray"); got != "alice-v1" {
		t.Fatalf("после Register config=%q, ожидался alice-v1", got)
	}

	acc, err := mgr.Regenerate("alice", "xray")
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if acc.Items[0].Value != "alice-v2" {
		t.Errorf("Regenerate вернул %q, ожидался alice-v2", acc.Items[0].Value)
	}
	// Старую учётку удалили перед созданием новой.
	if len(xr.deprovisioned) != 1 || xr.deprovisioned[0] != "alice" {
		t.Errorf("Deprovision не вызван для alice: %v", xr.deprovisioned)
	}
	// В БД лежит уже новый конфиг, identity прежний → история трафика сохранится.
	if got := configValue(t, st, user.ID, "xray"); got != "alice-v2" {
		t.Errorf("в БД config=%q, ожидался alice-v2", got)
	}
	ids, _ := st.GetIdentities(user.ID)
	if len(ids) != 1 || ids[0].Identity != "alice" {
		t.Errorf("identity изменился/задвоился: %+v", ids)
	}
}

func TestRegenerateErrors(t *testing.T) {
	// Есть провижинер только для xray; amneziawg включим в реестр, но учётку
	// пользователю в нём не заведём (Register создаёт лишь в наличных провах).
	mgr, _ := newTestManager(t, map[string]service.Provisioner{"xray": &fakeProv{name: "xray"}})
	if _, _, err := mgr.Register("bob", "Bob"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Сервис без провижинера.
	if _, err := mgr.Regenerate("bob", "telemt"); err == nil {
		t.Error("ожидалась ошибка для сервиса без провижинера")
	}
	// Несуществующий пользователь.
	if _, err := mgr.Regenerate("nobody", "xray"); err == nil {
		t.Error("ожидалась ошибка для несуществующего пользователя")
	}
}

func TestRegenerateProvisionFailureIsReported(t *testing.T) {
	xr := &fakeProv{name: "xray"}
	mgr, st := newTestManager(t, map[string]service.Provisioner{"xray": xr})
	user, _, err := mgr.Register("carol", "Carol")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	xr.failProvision = true
	if _, err := mgr.Regenerate("carol", "xray"); err == nil {
		t.Fatal("ожидалась ошибка при неуспешном Provision")
	}
	// Старый конфиг в БД не затёрт (перевыпуск не удался — остаётся прежний).
	if got := configValue(t, st, user.ID, "xray"); got != "carol-v1" {
		t.Errorf("после неудачи config=%q, ожидался прежний carol-v1", got)
	}
}

func TestRegenerateAllOnlyMatchingService(t *testing.T) {
	xr := &fakeProv{name: "xray"}
	awg := &fakeProv{name: "amneziawg"}
	mgr, _ := newTestManager(t, map[string]service.Provisioner{"xray": xr, "amneziawg": awg})

	for _, u := range []string{"u1", "u2", "u3"} {
		if _, _, err := mgr.Register(u, u); err != nil {
			t.Fatalf("Register %s: %v", u, err)
		}
	}
	// Все трое получили и xray, и amneziawg при Register (seq=1 у каждого прова).

	results, err := mgr.RegenerateAll("amneziawg")
	if err != nil {
		t.Fatalf("RegenerateAll: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("ожидалось 3 результата, получено %d", len(results))
	}
	for _, r := range results {
		if !r.OK {
			t.Errorf("перегенерация %s не удалась: %s", r.Username, r.Error)
		}
	}
	// Перевыпущены только amneziawg-учётки (3 удаления + 3 создания сверх Register).
	if len(awg.deprovisioned) != 3 {
		t.Errorf("amneziawg Deprovision вызван %d раз, ожидалось 3", len(awg.deprovisioned))
	}
	if len(awg.provisioned) != 6 { // 3 при Register + 3 при перегенерации
		t.Errorf("amneziawg Provision вызван %d раз, ожидалось 6", len(awg.provisioned))
	}
	// xray не тронут массовой перегенерацией amneziawg.
	if len(xr.deprovisioned) != 0 {
		t.Errorf("xray не должен перевыпускаться: deprovisioned=%v", xr.deprovisioned)
	}
	if len(xr.provisioned) != 3 { // только Register
		t.Errorf("xray Provision вызван %d раз, ожидалось 3", len(xr.provisioned))
	}
}

func TestRegenerateAllUnknownService(t *testing.T) {
	mgr, _ := newTestManager(t, map[string]service.Provisioner{"xray": &fakeProv{name: "xray"}})
	if _, err := mgr.RegenerateAll("amneziawg"); err == nil {
		t.Error("ожидалась ошибка для сервиса без провижинера")
	}
}

// подстраховка: имя сервиса в fakeProv совпадает с ключом реестра.
func TestFakeProvNameMatchesKey(t *testing.T) {
	f := &fakeProv{name: "xray"}
	acc, _ := f.Provision("x")
	if !strings.EqualFold(acc.Service, f.name) {
		t.Fatalf("service=%q != name=%q", acc.Service, f.name)
	}
}

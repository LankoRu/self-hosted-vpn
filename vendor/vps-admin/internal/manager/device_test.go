package manager

import (
	"testing"

	"github.com/LankoRu/vps-admin/internal/service"
)

// fakeAwgProv — AWG-адаптер-заглушка с поддержкой устройств (имена как в реальности).
type fakeAwgProv struct {
	added   []string
	deleted []string
}

func (f *fakeAwgProv) Provision(username string) (*service.Account, error) {
	id := username + "-awg"
	f.added = append(f.added, id)
	return &service.Account{Service: "amneziawg", Identity: id, Address: "10.8.0.2",
		Items: []service.Item{{Label: "conf", Value: "main-" + username}}}, nil
}

func (f *fakeAwgProv) ProvisionDevice(username, device string) (*service.Account, error) {
	id := username + "-awg-" + device
	f.added = append(f.added, id)
	return &service.Account{Service: "amneziawg", Identity: id, Address: "10.8.0.3",
		Items: []service.Item{{Label: "conf", Value: "dev-" + id}}}, nil
}

func (f *fakeAwgProv) Deprovision(identity string) error {
	f.deleted = append(f.deleted, identity)
	return nil
}

func TestDeviceLifecycle(t *testing.T) {
	prov := &fakeAwgProv{}
	m, st := newTestManager(t, map[string]service.Provisioner{"amneziawg": prov})

	u, _, err := m.Register("alice", "Alice")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// устройство: транслит имени, identity привязан к тому же юзеру
	acc, err := m.AddDevice("alice", "Ноутбук")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	if acc.Identity != "alice-awg-noutbuk" {
		t.Errorf("identity = %q, want alice-awg-noutbuk", acc.Identity)
	}
	ids, _ := st.GetIdentities(u.ID)
	if len(ids) != 2 {
		t.Fatalf("identities = %d, want 2 (основная + устройство)", len(ids))
	}
	// порядок: основная (короче) первой — Regenerate перевыпускает именно её
	if ids[0].Identity != "alice-awg" {
		t.Errorf("первая identity = %q, want alice-awg", ids[0].Identity)
	}

	// идемпотентность: повтор не пересоздаёт учётку, возвращает сохранённый конфиг
	addedBefore := len(prov.added)
	acc2, err := m.AddDevice("alice", "Ноутбук")
	if err != nil {
		t.Fatalf("повторный AddDevice: %v", err)
	}
	if len(prov.added) != addedBefore {
		t.Error("повторный AddDevice пересоздал учётку")
	}
	if acc2.Identity != acc.Identity || len(acc2.Items) != 1 || acc2.Items[0].Value != "dev-alice-awg-noutbuk" {
		t.Errorf("повторный AddDevice вернул %+v", acc2)
	}

	// regen перевыпускает ОСНОВНУЮ учётку, не устройство
	if _, err := m.Regenerate("alice", "amneziawg"); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if prov.deleted[len(prov.deleted)-1] != "alice-awg" {
		t.Errorf("regen удалил %q, want alice-awg (основную)", prov.deleted[len(prov.deleted)-1])
	}

	// трафик устройства есть в истории
	if _, err := st.Exec(`INSERT INTO traffic_deltas(service,identity,total_bytes) VALUES('amneziawg','alice-awg-noutbuk',123)`); err != nil {
		t.Fatalf("insert delta: %v", err)
	}

	// удаление устройства: учётка в панели + identity + история трафика
	if err := m.RemoveDevice("alice", "Ноутбук"); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	if prov.deleted[len(prov.deleted)-1] != "alice-awg-noutbuk" {
		t.Errorf("remove удалил %q", prov.deleted[len(prov.deleted)-1])
	}
	ids, _ = st.GetIdentities(u.ID)
	if len(ids) != 1 || ids[0].Identity != "alice-awg" {
		t.Errorf("после remove identities = %+v", ids)
	}
	var n int
	_ = st.QueryRow(`SELECT COUNT(*) FROM traffic_deltas WHERE identity='alice-awg-noutbuk'`).Scan(&n)
	if n != 0 {
		t.Errorf("история трафика устройства не очищена: %d строк", n)
	}

	// удаление несуществующего устройства → ошибка
	if err := m.RemoveDevice("alice", "phone"); err == nil {
		t.Error("ожидалась ошибка на несуществующем устройстве")
	}
	// пустое имя устройства → ошибка
	if _, err := m.AddDevice("alice", "  "); err == nil {
		t.Error("ожидалась ошибка на пустом имени устройства")
	}

	// удаление юзера каскадно сносит и основную учётку
	if _, err := m.Delete("alice"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if prov.deleted[len(prov.deleted)-1] != "alice-awg" {
		t.Errorf("delete юзера не снёс основную учётку: %v", prov.deleted)
	}
}

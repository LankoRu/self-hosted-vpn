package manager

import (
	"testing"

	"github.com/LankoRu/vps-admin/internal/service"
)

func TestRegisterByNameGeneratesUniqueUsername(t *testing.T) {
	m, _ := newTestManager(t, map[string]service.Provisioner{"xray": &fakeProv{name: "xray"}})

	u1, _, err := m.RegisterByName("Вася Пупкин")
	if err != nil {
		t.Fatalf("RegisterByName: %v", err)
	}
	if u1.Username != "vasya-pupkin" || u1.DisplayName != "Вася Пупкин" {
		t.Errorf("u1 = %q / %q", u1.Username, u1.DisplayName)
	}
	// коллизия → суффикс -2
	u2, _, err := m.RegisterByName("Вася Пупкин")
	if err != nil {
		t.Fatalf("RegisterByName collision: %v", err)
	}
	if u2.Username != "vasya-pupkin-2" {
		t.Errorf("u2.Username = %q, want vasya-pupkin-2", u2.Username)
	}
	// пустое имя → ошибка
	if _, _, err := m.RegisterByName("   "); err == nil {
		t.Error("ожидалась ошибка на пустом имени")
	}
}

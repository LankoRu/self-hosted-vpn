package scheduler

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/LankoRu/vps-admin/internal/alerts"
	"github.com/LankoRu/vps-admin/internal/config"
)

type fakeNotifier struct{ msgs []string }

func (f *fakeNotifier) Notify(text string) { f.msgs = append(f.msgs, text) }

// TestHealthAlertsWhenAWGModuleMissing — после обновления ядра/ребута модуль
// amneziawg может не загрузиться (modules.dep не обновлён). Без этой проверки
// UDP-443 молча не работает, и узнаём мы об этом от пользователей.
func TestHealthAlertsWhenAWGModuleMissing(t *testing.T) {
	dir := t.TempDir()
	old := modulePath
	modulePath = func(name string) string { return filepath.Join(dir, name) }
	defer func() { modulePath = old }()

	cfg := &config.Config{}
	cfg.Services.AmneziaWG.Enabled = true
	cfg.AmneziaWG3xUI.Kernel = true

	nf := &fakeNotifier{}
	CheckHealth(cfg, alerts.New(nf))
	if len(nf.msgs) != 1 {
		t.Fatalf("ожидали 1 алерт, получили %d: %v", len(nf.msgs), nf.msgs)
	}
	if !contains(nf.msgs[0], "AmneziaWG") || !contains(nf.msgs[0], "modprobe amneziawg") {
		t.Errorf("алерт без подсказки, что делать: %q", nf.msgs[0])
	}

	// модуль «появился» → алертов больше нет
	if err := os.MkdirAll(filepath.Join(dir, "amneziawg"), 0o755); err != nil {
		t.Fatal(err)
	}
	nf2 := &fakeNotifier{}
	CheckHealth(cfg, alerts.New(nf2))
	if len(nf2.msgs) != 0 {
		t.Errorf("при загруженном модуле алертов быть не должно: %v", nf2.msgs)
	}
}

// TestHealthIgnoresUserspaceAWG — в userspace-режиме модуля нет, и это норма.
func TestHealthIgnoresUserspaceAWG(t *testing.T) {
	dir := t.TempDir()
	old := modulePath
	modulePath = func(name string) string { return filepath.Join(dir, name) }
	defer func() { modulePath = old }()

	cfg := &config.Config{}
	cfg.Services.AmneziaWG.Enabled = true
	cfg.AmneziaWG3xUI.Kernel = false

	nf := &fakeNotifier{}
	CheckHealth(cfg, alerts.New(nf))
	for _, m := range nf.msgs {
		if contains(m, "AmneziaWG") {
			t.Errorf("в userspace-режиме не должно быть алерта про модуль: %q", m)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

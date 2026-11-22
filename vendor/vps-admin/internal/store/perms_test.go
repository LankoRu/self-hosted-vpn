package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/LankoRu/vps-admin/internal/config"
)

func TestDBPermissionsRestricted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "stats.db")
	st, err := New(path, &config.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()

	// каталог — 0700, файл — 0600 (внутри ключи клиентов)
	if fi, err := os.Stat(dir); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("каталог = %o, want 700", perm)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("stat db: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("БД = %o, want 600", perm)
	}
}

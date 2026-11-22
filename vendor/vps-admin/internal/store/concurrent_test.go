package store

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/LankoRu/vps-admin/internal/config"
)

// TestConcurrentWritesNoBusy — сбор статистики и обновление ссылок идут
// параллельно (в т.ч. на старте). Раньше UpdateDeltas держал write-транзакцию,
// а SetIdentityConfig писал мимо мьютекса — SQLite отдавал "database is locked",
// и ссылки не сохранялись.
func TestConcurrentWritesNoBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	st, err := New(path, &config.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()

	uid, err := st.CreateUser("test", "Тест", 0)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := st.AddIdentity(uid, "xray", "test", "10.0.0.2", "[]"); err != nil {
		t.Fatalf("AddIdentity: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			st.UpdateDeltas("xray", map[string]uint64{"test": 100})
		}()
		go func() {
			defer wg.Done()
			if _, err := st.SetIdentityConfig("xray", "test", `[{"label":"ссылка"}]`); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("запись в БД упала: %v", err)
	}
}

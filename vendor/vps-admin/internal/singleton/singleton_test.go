package singleton

import (
	"path/filepath"
	"testing"
)

func TestSecondInstanceBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	release, err := Acquire(path)
	if err != nil {
		t.Fatalf("первый Acquire: %v", err)
	}

	// Пока первый держит лок — второй должен быстро отказать.
	if _, err := acquire(path, 1, 0); err == nil {
		t.Fatal("второй экземпляр не должен был получить блокировку")
	}

	// После освобождения — снова можно взять.
	release()
	release2, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire после release: %v", err)
	}
	release2()
}

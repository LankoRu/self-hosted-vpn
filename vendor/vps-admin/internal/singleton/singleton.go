// Package singleton гарантирует, что на хосте работает только один экземпляр
// сервиса — иначе два поллера Telegram дерутся за getUpdates
// ("Conflict: terminated by other getUpdates request").
package singleton

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// Acquire берёт эксклюзивную advisory-блокировку на файл path.
// Блокировка снимается ОС при завершении процесса, поэтому «залипшего» лока не
// бывает. Несколько секунд повторяет попытки, чтобы пережить перекрытие при
// рестарте (systemd поднимает новый процесс, пока старый ещё не вышел).
func Acquire(path string) (release func(), err error) {
	return acquire(path, 20, 500*time.Millisecond)
}

func acquire(path string, tries int, wait time.Duration) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	for i := 0; i < tries; i++ {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if i < tries-1 {
			time.Sleep(wait)
		}
	}

	_ = f.Close()
	return nil, fmt.Errorf("уже запущен другой экземпляр vps-admin "+
		"(не удалось взять блокировку %s) — остановите лишний процесс/сервис", path)
}

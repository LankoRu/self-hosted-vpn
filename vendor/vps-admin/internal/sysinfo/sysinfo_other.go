//go:build !linux

package sysinfo

import "runtime"

// Collect — заглушка для не-Linux (сервис живёт на Linux-VPS; локальная сборка
// на macOS просто не показывает карточку ресурсов).
func Collect() Info {
	return Info{Available: false, Cores: runtime.NumCPU()}
}

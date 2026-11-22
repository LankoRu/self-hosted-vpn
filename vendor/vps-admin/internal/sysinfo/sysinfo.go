// Package sysinfo — метрики ресурсов самого сервера (CPU, память, диск, uptime)
// для отображения в веб-панели. Реализация зависит от ОС: на Linux читаем /proc,
// на остальных платформах отдаём Available=false (панель просто скроет карточку).
package sysinfo

// Info — снимок состояния сервера.
type Info struct {
	Available  bool    `json:"available"`   // удалось ли собрать метрики
	CPUPercent float64 `json:"cpu_percent"` // загрузка CPU, % (по дельте /proc/stat)
	Load1      float64 `json:"load1"`       // load average за 1 минуту
	Cores      int     `json:"cores"`       // число ядер
	MemUsed    uint64  `json:"mem_used"`
	MemTotal   uint64  `json:"mem_total"`
	DiskUsed   uint64  `json:"disk_used"`
	DiskTotal  uint64  `json:"disk_total"`
	Uptime     int64   `json:"uptime"` // секунды
}

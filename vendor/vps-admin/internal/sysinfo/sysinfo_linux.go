//go:build linux

package sysinfo

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Предыдущий снимок /proc/stat — нужен, чтобы посчитать загрузку CPU по дельте
// между вызовами (панель опрашивает раз в несколько секунд).
var (
	cpuMu       sync.Mutex
	lastTotal   uint64
	lastIdle    uint64
	lastPercent float64
)

// Collect собирает метрики; недоступные поля остаются нулевыми.
func Collect() Info {
	info := Info{Available: true, Cores: runtime.NumCPU()}
	info.CPUPercent = cpuPercent()
	info.Load1 = load1()
	info.MemUsed, info.MemTotal = memory()
	info.DiskUsed, info.DiskTotal = disk("/")
	info.Uptime = uptime()
	return info
}

// cpuPercent считает загрузку CPU как долю не-idle времени между двумя чтениями
// /proc/stat. При первом вызове дельты ещё нет — вернём 0.
func cpuPercent() float64 {
	total, idle, ok := readCPUStat()
	if !ok {
		return 0
	}
	cpuMu.Lock()
	defer cpuMu.Unlock()
	if lastTotal == 0 || total <= lastTotal {
		lastTotal, lastIdle = total, idle
		return lastPercent
	}
	dTotal := float64(total - lastTotal)
	dIdle := float64(idle - lastIdle)
	lastTotal, lastIdle = total, idle
	if dTotal <= 0 {
		return lastPercent
	}
	p := (1 - dIdle/dTotal) * 100
	if p < 0 {
		p = 0
	} else if p > 100 {
		p = 100
	}
	lastPercent = p
	return p
}

// readCPUStat читает суммарное и idle-время из первой строки /proc/stat.
func readCPUStat() (total, idle uint64, ok bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, false
	}
	for i, v := range f[1:] {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			continue
		}
		total += n
		if i == 3 || i == 4 { // idle + iowait
			idle += n
		}
	}
	return total, idle, total > 0
}

func load1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// memory возвращает использованную и общую память (байты), считая занятой
// total-available (MemAvailable учитывает кэш, который можно освободить).
func memory() (used, total uint64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var avail uint64
	for _, line := range strings.Split(string(b), "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			total = kb * 1024
		case "MemAvailable":
			avail = kb * 1024
		}
	}
	if total > avail {
		used = total - avail
	}
	return used, total
}

func disk(path string) (used, total uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	total = st.Blocks * bs
	free := st.Bavail * bs
	if total > free {
		used = total - free
	}
	return used, total
}

func uptime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return int64(v)
}

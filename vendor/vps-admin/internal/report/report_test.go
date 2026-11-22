package report

import (
	"strings"
	"testing"
	"time"
)

func week() (time.Time, time.Time) {
	to := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	return to.AddDate(0, 0, -7), to
}

func TestWeeklyDigest(t *testing.T) {
	rows := []Row{
		{Service: "xray", User: "Саша", Identity: "sasha", Total: 3 * 1024 * 1024 * 1024},
		{Service: "telemt", User: "Саша", Identity: "sasha", Total: 1024 * 1024 * 1024},
		{Service: "xray", User: "lanko", Identity: "lanko", Total: 2 * 1024 * 1024 * 1024},
		{Service: "amneziawg", Identity: "orphan", Total: 512 * 1024 * 1024},
	}
	// Прошлая неделя вдвое меньше → ожидаем +100%.
	prev := []Row{{Service: "xray", User: "Саша", Identity: "sasha", Total: 3328 * 1024 * 1024}}

	from, to := week()
	got := WeeklyDigest(rows, prev, from, to, 10)

	for _, want := range []string{
		"Сводка за неделю (20.07 — 27.07)",
		"Σ Всего: 6.50 GB (+100% к прошлой неделе)",
		"xray", "telemt", "amneziawg",
		"orphan (не привязан)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("в сводке нет %q:\n%s", want, got)
		}
	}

	// Пользователи отсортированы по убыванию: Саша (4 GB) → lanko (2 GB) → orphan.
	iSasha := strings.Index(got, "1. Саша")
	iLanko := strings.Index(got, "2. lanko")
	if iSasha < 0 || iLanko < 0 || iSasha > iLanko {
		t.Errorf("неверный порядок прожорливых пользователей:\n%s", got)
	}
}

func TestWeeklyDigestNoPrevWeek(t *testing.T) {
	rows := []Row{{Service: "xray", User: "Саша", Identity: "sasha", Total: 1024}}
	from, to := week()
	got := WeeklyDigest(rows, nil, from, to, 10)
	if strings.Contains(got, "к прошлой неделе") {
		t.Errorf("без данных за прошлую неделю сравнения быть не должно:\n%s", got)
	}
}

func TestWeeklyDigestEmpty(t *testing.T) {
	from, to := week()
	got := WeeklyDigest(nil, nil, from, to, 10)
	if !strings.Contains(got, "Трафика за эту неделю не было") {
		t.Errorf("ожидали пустую сводку, получили:\n%s", got)
	}
}

func TestWeeklyDigestTopN(t *testing.T) {
	rows := make([]Row, 0, 12)
	for i := 0; i < 12; i++ {
		rows = append(rows, Row{
			Service:  "xray",
			Identity: string(rune('a'+i)) + "-user",
			Total:    uint64(12-i) * 1024 * 1024,
		})
	}
	from, to := week()
	got := WeeklyDigest(rows, nil, from, to, 3)

	if !strings.Contains(got, "…и ещё 9") {
		t.Errorf("не сообщили про скрытых пользователей:\n%s", got)
	}
	if strings.Contains(got, " 4. ") {
		t.Errorf("топ-3 должен обрезать список:\n%s", got)
	}
}

// Имена русские: обрезка по байтам разрубила бы кириллицу пополам.
func TestTruncateIsRuneSafe(t *testing.T) {
	long := strings.Repeat("я", 30)
	got := truncate(long, 24)
	if r := []rune(got); len(r) != 24 {
		t.Fatalf("ожидали 24 руны, получили %d (%q)", len(r), got)
	}
	if !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("обрезка испортила строку: %q", got)
	}
}

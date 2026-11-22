// Package report формирует текстовые сводки по трафику для Telegram.
package report

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Row — строка агрегированного трафика.
// User — display_name каноничного пользователя (пусто, если идентичность
// не привязана к пользователю); Identity — сырой идентификатор в сервисе.
type Row struct {
	Service  string
	User     string
	Identity string
	Total    uint64
}

// Label — как показывать строку: имя пользователя или сырой идентификатор.
func (r Row) Label() string {
	if r.User != "" {
		return r.User
	}
	return r.Identity + " (не привязан)"
}

// HumanBytes — человекочитаемый размер.
func HumanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// ServiceSummary — сводка по сервисам (короткое сообщение).
func ServiceSummary(rows []Row, period string) string {
	byService := make(map[string]uint64)
	var total uint64
	for _, r := range rows {
		byService[r.Service] += r.Total
		total += r.Total
	}

	services := sortedByValueDesc(byService)

	var sb strings.Builder
	fmt.Fprintf(&sb, "📊 Трафик по сервисам за %s\n\n", strings.ToUpper(period))
	if len(services) == 0 {
		sb.WriteString("Нет данных за период.\n")
		return sb.String()
	}
	for _, svc := range services {
		fmt.Fprintf(&sb, "🔹 %-12s %s\n", svc, HumanBytes(byService[svc]))
	}
	fmt.Fprintf(&sb, "\nΣ Всего: %s\n", HumanBytes(total))
	return sb.String()
}

// UserSummary — суммарный трафик по каждому каноничному пользователю
// (сложены все сервисы). Именно ради этого пользователи заводятся сразу
// во всех сервисах.
func UserSummary(rows []Row, period string) string {
	byUser := make(map[string]uint64)
	for _, r := range rows {
		byUser[r.Label()] += r.Total
	}

	users := sortedByValueDesc(byUser)

	var sb strings.Builder
	fmt.Fprintf(&sb, "👥 Суммарный трафик по пользователям за %s\n\n", strings.ToUpper(period))
	if len(users) == 0 {
		sb.WriteString("Нет данных за период.\n")
		return sb.String()
	}
	for i, u := range users {
		fmt.Fprintf(&sb, "%2d. %-24s %s\n", i+1, truncate(u, 24), HumanBytes(byUser[u]))
	}
	return sb.String()
}

// WeeklyDigest — компактная еженедельная сводка для рассылки в чат: сколько
// всего потрачено, разбивка по сервисам и самые прожорливые пользователи.
// prev — те же строки за предыдущую неделю (для сравнения; может быть nil),
// topN — сколько пользователей показать (0 → все).
func WeeklyDigest(rows, prev []Row, from, to time.Time, topN int) string {
	byService := make(map[string]uint64)
	byUser := make(map[string]uint64)
	var total uint64
	for _, r := range rows {
		byService[r.Service] += r.Total
		byUser[r.Label()] += r.Total
		total += r.Total
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "🗓 Сводка за неделю (%s — %s)\n\n", from.Format("02.01"), to.Format("02.01"))
	if total == 0 {
		sb.WriteString("Трафика за эту неделю не было.\n")
		return sb.String()
	}

	fmt.Fprintf(&sb, "Σ Всего: %s%s\n\n", HumanBytes(total), vsPrevWeek(total, prev))

	sb.WriteString("📊 По сервисам:\n")
	for _, svc := range sortedByValueDesc(byService) {
		fmt.Fprintf(&sb, "🔹 %-12s %s\n", svc, HumanBytes(byService[svc]))
	}

	users := sortedByValueDesc(byUser)
	hidden := 0
	if topN > 0 && len(users) > topN {
		hidden = len(users) - topN
		users = users[:topN]
	}
	sb.WriteString("\n🔥 Больше всех потратили:\n")
	for i, u := range users {
		fmt.Fprintf(&sb, "%2d. %-24s %s\n", i+1, truncate(u, 24), HumanBytes(byUser[u]))
	}
	if hidden > 0 {
		fmt.Fprintf(&sb, "…и ещё %d — /full\n", hidden)
	}
	return sb.String()
}

// vsPrevWeek — « (+18% к прошлой неделе)». Пусто, если сравнивать не с чем:
// в первую неделю работы сервиса предыдущего окна просто нет.
func vsPrevWeek(total uint64, prev []Row) string {
	var was uint64
	for _, r := range prev {
		was += r.Total
	}
	if was == 0 {
		return ""
	}
	diff := (float64(total) - float64(was)) * 100 / float64(was)
	return fmt.Sprintf(" (%+.0f%% к прошлой неделе)", diff)
}

// FullUserReport — детальный отчёт (для файла): пользователь × сервис.
func FullUserReport(rows []Row, period string) string {
	// Группируем по пользователю, внутри — по сервисам.
	type agg struct {
		total    uint64
		services map[string]uint64
	}
	byUser := make(map[string]*agg)
	for _, r := range rows {
		label := r.Label()
		a := byUser[label]
		if a == nil {
			a = &agg{services: make(map[string]uint64)}
			byUser[label] = a
		}
		a.total += r.Total
		a.services[r.Service] += r.Total
	}

	labels := make([]string, 0, len(byUser))
	for l := range byUser {
		labels = append(labels, l)
	}
	sort.Slice(labels, func(i, j int) bool {
		return byUser[labels[i]].total > byUser[labels[j]].total
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "ДЕТАЛЬНАЯ СТАТИСТИКА ЗА %s\n", strings.ToUpper(period))
	sb.WriteString(strings.Repeat("=", 60) + "\n\n")

	for _, l := range labels {
		a := byUser[l]
		fmt.Fprintf(&sb, "%s — Σ %s\n", l, HumanBytes(a.total))
		for _, svc := range sortedByValueDesc(a.services) {
			fmt.Fprintf(&sb, "    %-12s %s\n", svc, HumanBytes(a.services[svc]))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func sortedByValueDesc(m map[string]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// truncate режет по рунам, а не по байтам: имена русские, обрезка по байтам
// разрубила бы кириллицу пополам и выдала «мусорный» символ.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

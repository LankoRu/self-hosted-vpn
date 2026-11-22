package web

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/LankoRu/vps-admin/internal/store"
)

// bucketing описывает, как нарезать период на временные корзины: SQL-выражение
// strftime для группировки, полный упорядоченный список корзин с подписями (нужен,
// чтобы заполнить нулями промежутки без трафика — иначе линия рвётся) и WHERE,
// ограничивающий выборку окном периода.
type bucketing struct {
	sqlExpr string
	keys    []string // ключи в том же формате, что даёт sqlExpr
	labels  []string // человекочитаемые подписи оси X
	where   string   // ограничение по td.timestamp (UTC-порог)
}

// bucketsFor строит корзины под период в ЧАСОВОМ ПОЯСЕ БРАУЗЕРА (tzMin — минуты
// к востоку от UTC). Храним всё в UTC, но корзины и подписи считаем в местном
// времени зрителя: границы дня/часа берём по локальным «настенным часам», а в SQL
// сдвигаем сохранённый UTC-timestamp модификатором strftime. «Сегодня» — по часам,
// недели/месяц/всё — по дням.
func bucketsFor(period string, tzMin int) bucketing {
	loc := time.FixedZone("browser", tzMin*60)
	now := time.Now().In(loc)
	// Сдвиг UTC→local прямо в strftime: '+180 minutes' для UTC+3 и т.п.
	mod := fmt.Sprintf(", '%+d minutes'", tzMin)
	var b bucketing

	switch period {
	case "today":
		b.sqlExpr = "strftime('%Y-%m-%dT%H', td.timestamp" + mod + ")"
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		for t := start; !t.After(now); t = t.Add(time.Hour) {
			b.keys = append(b.keys, t.Format("2006-01-02T15"))
			b.labels = append(b.labels, t.Format("15:04"))
		}
		b.where = whereFromLocal(start)
	default:
		b.sqlExpr = "strftime('%Y-%m-%d', td.timestamp" + mod + ")"
		start := periodStartDay(period, now, loc)
		for t := start; !t.After(now); t = t.AddDate(0, 0, 1) {
			b.keys = append(b.keys, t.Format("2006-01-02"))
			b.labels = append(b.labels, t.Format("02.01"))
		}
		b.where = whereFromLocal(start)
	}
	return b
}

// whereFromLocal переводит локальную границу периода обратно в UTC и формирует
// строковый порог по td.timestamp (SQLite хранит время как 'YYYY-MM-DD HH:MM:SS'
// в UTC, лексикографическое сравнение совпадает с временным). start.UTC() — это
// не пользовательский ввод, а форматированное время, инъекции нет.
func whereFromLocal(start time.Time) string {
	return "td.timestamp >= '" + start.UTC().Format("2006-01-02 15:04:05") + "'"
}

func periodStartDay(period string, now time.Time, loc *time.Location) time.Time {
	day := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	}
	switch period {
	case "week":
		return day(now.AddDate(0, 0, -6))
	case "month":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	default: // full — последние 90 дней (окно хранения traffic_deltas)
		return day(now.AddDate(0, 0, -89))
	}
}

// parseTZ читает смещение браузера (минуты к востоку от UTC) и ограничивает его
// разумным диапазоном [-14ч, +14ч].
func parseTZ(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	if n < -14*60 {
		return -14 * 60
	}
	if n > 14*60 {
		return 14 * 60
	}
	return n
}

// maxSeries — сколько серий показывать явно; остальные сворачиваются в «Прочие».
const maxSeries = 8

// fillSeries раскладывает точки по ключу серии в выровненные по корзинам массивы
// (нули там, где трафика не было). Возвращает карту key→[]uint64.
func fillSeries(keys []string, pts []store.SeriesPoint) map[string][]uint64 {
	idx := make(map[string]int, len(keys))
	for i, k := range keys {
		idx[k] = i
	}
	out := map[string][]uint64{}
	for _, p := range pts {
		i, ok := idx[p.Bucket]
		if !ok {
			continue // корзина вне окна периода
		}
		row, ok := out[p.Key]
		if !ok {
			row = make([]uint64, len(keys))
			out[p.Key] = row
		}
		row[i] += p.Total
	}
	return out
}

// namedSeries — серия с именем (для стабильного порядка и сворачивания хвоста).
type namedSeries struct {
	Name  string   `json:"name"`
	Data  []uint64 `json:"data"`
	Total uint64   `json:"total"`
}

// topSeries сортирует серии по сумме убыв., оставляет maxSeries, а остальные
// суммирует в «Прочие».
func topSeries(keys []string, m map[string][]uint64) []namedSeries {
	list := make([]namedSeries, 0, len(m))
	for name, data := range m {
		var total uint64
		for _, v := range data {
			total += v
		}
		list = append(list, namedSeries{Name: name, Data: data, Total: total})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Total != list[j].Total {
			return list[i].Total > list[j].Total
		}
		return list[i].Name < list[j].Name
	})
	if len(list) <= maxSeries {
		return list
	}
	rest := make([]uint64, len(keys))
	var restTotal uint64
	for _, s := range list[maxSeries:] {
		for i, v := range s.Data {
			rest[i] += v
		}
		restTotal += s.Total
	}
	head := list[:maxSeries]
	if restTotal > 0 {
		head = append(head, namedSeries{Name: "Прочие", Data: rest, Total: restTotal})
	}
	return head
}

// series возвращает выровненный по корзинам ряд по ключу keyExpr с
// доп.ограничением extra (может быть пустым).
func (s *Server) series(b bucketing, keyExpr, extra string) map[string][]uint64 {
	where := b.where
	if extra != "" {
		where += " AND (" + extra + ")"
	}
	pts, _ := s.store.TrafficSeries(b.sqlExpr, keyExpr, where)
	return fillSeries(b.keys, pts)
}

// overallSeries — общий ряд (одна серия) с доп.ограничением extra.
func (s *Server) overallSeries(b bucketing, extra string) []uint64 {
	row := s.series(b, "'Σ'", extra)["Σ"]
	if row == nil {
		row = make([]uint64, len(b.keys))
	}
	return row
}

// handleCharts — графики главной страницы: общий, по сервисам, по пользователям.
func (s *Server) handleCharts(w http.ResponseWriter, r *http.Request) {
	period, _ := whereFor(r.URL.Query().Get("period"))
	b := bucketsFor(period, parseTZ(r.URL.Query().Get("tz")))

	// Ключ серии для пользователя: имя, а для непривязанных — identity с пометкой.
	const userKey = "COALESCE(u.display_name, td.identity || ' (не привязан)')"

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"period":   period,
		"labels":   b.labels,
		"overall":  s.overallSeries(b, ""),
		"services": topSeries(b.keys, s.series(b, "td.service", "")),
		"users":    topSeries(b.keys, s.series(b, userKey, "")),
	})
}

// handleUserCharts — графики в карточке пользователя: его суммарный ряд и
// разбивка по сервисам за выбранный период.
func (s *Server) handleUserCharts(w http.ResponseWriter, r *http.Request) {
	user, err := s.store.GetUserByName(r.PathValue("username"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "пользователь не найден")
		return
	}
	period, _ := whereFor(r.URL.Query().Get("period"))
	b := bucketsFor(period, parseTZ(r.URL.Query().Get("tz")))
	extra := fmt.Sprintf("i.user_id = %d", user.ID) // user.ID — int из БД, не ввод

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"period":   period,
		"labels":   b.labels,
		"overall":  s.overallSeries(b, extra),
		"services": topSeries(b.keys, s.series(b, "td.service", extra)),
	})
}

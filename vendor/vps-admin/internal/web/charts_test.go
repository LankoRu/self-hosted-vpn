package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/manager"
	"github.com/LankoRu/vps-admin/internal/registry"
	"github.com/LankoRu/vps-admin/internal/store"
)

func TestFillSeriesAlignsBucketsAndZeroFills(t *testing.T) {
	keys := []string{"a", "b", "c"}
	pts := []store.SeriesPoint{
		{Bucket: "a", Key: "xray", Total: 10},
		{Bucket: "c", Key: "xray", Total: 5},
		{Bucket: "b", Key: "telemt", Total: 7},
		{Bucket: "zzz", Key: "xray", Total: 99}, // вне окна — отбрасывается
	}
	got := fillSeries(keys, pts)
	if want := []uint64{10, 0, 5}; !equalU64(got["xray"], want) {
		t.Errorf("xray = %v, ожидалось %v", got["xray"], want)
	}
	if want := []uint64{0, 7, 0}; !equalU64(got["telemt"], want) {
		t.Errorf("telemt = %v, ожидалось %v", got["telemt"], want)
	}
}

func TestTopSeriesCollapsesTail(t *testing.T) {
	keys := make([]string, 1)
	m := map[string][]uint64{}
	// 10 серий: топ-8 остаются, 2 сворачиваются в «Прочие».
	for i := 0; i < 10; i++ {
		m[string(rune('a'+i))] = []uint64{uint64(100 - i)}
	}
	got := topSeries(keys, m)
	if len(got) != maxSeries+1 {
		t.Fatalf("серий = %d, ожидалось %d (топ + Прочие)", len(got), maxSeries+1)
	}
	if got[len(got)-1].Name != "Прочие" {
		t.Errorf("последняя серия = %q, ожидалась «Прочие»", got[len(got)-1].Name)
	}
	// Отсортировано по убыванию суммы.
	if got[0].Total < got[1].Total {
		t.Errorf("серии не отсортированы по убыванию: %d < %d", got[0].Total, got[1].Total)
	}
}

func TestListUsersRespectsPeriod(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetDefaults()
	st, err := store.New(filepath.Join(t.TempDir(), "uperiod.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	uid, _ := st.CreateUser("alice", "Алиса", 0)
	_ = st.AddIdentity(uid, "xray", "alice-x", "", "[]")
	// Сегодняшний трафик + старый (10 дней назад).
	st.Exec(`INSERT INTO traffic_deltas(service,identity,total_bytes) VALUES('xray','alice-x',100)`)
	st.Exec(`INSERT INTO traffic_deltas(service,identity,timestamp,total_bytes)
	         VALUES('xray','alice-x',datetime('now','-10 days'),900)`)

	ts := httptest.NewServer(New(cfg, st, manager.New(st, registry.New(cfg, st))).Handler())
	t.Cleanup(ts.Close)

	total := func(period string) uint64 {
		r, err := http.Get(ts.URL + "/api/users?period=" + period)
		if err != nil {
			t.Fatalf("GET users: %v", err)
		}
		defer r.Body.Close()
		var out []struct {
			TotalBytes uint64 `json:"total_bytes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&out)
		if len(out) != 1 {
			t.Fatalf("ожидался 1 пользователь, получено %d", len(out))
		}
		return out[0].TotalBytes
	}

	if got := total("today"); got != 100 {
		t.Errorf("за сегодня = %d, ожидалось 100", got)
	}
	if got := total("full"); got != 1000 {
		t.Errorf("за всё время = %d, ожидалось 1000", got)
	}
	// Без параметра — прежнее поведение (всё время).
	if got := total(""); got != 1000 {
		t.Errorf("без периода = %d, ожидалось 1000", got)
	}
}

func TestChartsEndpoint(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetDefaults()
	st, err := store.New(filepath.Join(t.TempDir(), "charts.db"), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	uid, _ := st.CreateUser("alice", "Алиса", 0)
	_ = st.AddIdentity(uid, "xray", "alice-x", "", "[]")
	_ = st.AddIdentity(uid, "telemt", "alice-t", "", "[]")
	// Трафик сегодня (в окне любого периода).
	st.Exec(`INSERT INTO traffic_deltas(service,identity,total_bytes) VALUES('xray','alice-x',1000)`)
	st.Exec(`INSERT INTO traffic_deltas(service,identity,total_bytes) VALUES('telemt','alice-t',500)`)
	// Непривязанная идентичность — попадает в «(не привязан)».
	st.Exec(`INSERT INTO traffic_deltas(service,identity,total_bytes) VALUES('xray','ghost',200)`)

	ts := httptest.NewServer(New(cfg, st, manager.New(st, registry.New(cfg, st))).Handler())
	t.Cleanup(ts.Close)

	r, err := http.Get(ts.URL + "/api/charts?period=today")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("GET charts: %v / status %d", err, r.StatusCode)
	}
	var d struct {
		Labels   []string      `json:"labels"`
		Overall  []uint64      `json:"overall"`
		Services []namedSeries `json:"services"`
		Users    []namedSeries `json:"users"`
	}
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.Body.Close()

	if len(d.Labels) == 0 || len(d.Overall) != len(d.Labels) {
		t.Fatalf("overall не выровнен по labels: %d vs %d", len(d.Overall), len(d.Labels))
	}
	var overallSum uint64
	for _, v := range d.Overall {
		overallSum += v
	}
	if overallSum != 1700 {
		t.Errorf("сумма overall = %d, ожидалось 1700", overallSum)
	}
	// По сервисам: xray=1200 (alice+ghost), telemt=500.
	svc := map[string]uint64{}
	for _, s := range d.Services {
		svc[s.Name] = s.Total
	}
	if svc["xray"] != 1200 || svc["telemt"] != 500 {
		t.Errorf("по сервисам неверно: %+v", svc)
	}
	// По пользователям: есть Алиса и непривязанный ghost.
	users := map[string]uint64{}
	for _, u := range d.Users {
		users[u.Name] = u.Total
	}
	if users["Алиса"] != 1500 {
		t.Errorf("трафик Алисы = %d, ожидалось 1500", users["Алиса"])
	}
	if users["ghost (не привязан)"] != 200 {
		t.Errorf("непривязанный ghost = %d, ожидалось 200 (%+v)", users["ghost (не привязан)"], users)
	}

	// Графики карточки пользователя: только её трафик (1500), без ghost.
	r2, err := http.Get(ts.URL + "/api/users/alice/charts?period=today")
	if err != nil || r2.StatusCode != http.StatusOK {
		t.Fatalf("GET user charts: %v / status %d", err, r2.StatusCode)
	}
	var ud struct {
		Overall  []uint64      `json:"overall"`
		Services []namedSeries `json:"services"`
	}
	_ = json.NewDecoder(r2.Body).Decode(&ud)
	r2.Body.Close()
	var uSum uint64
	for _, v := range ud.Overall {
		uSum += v
	}
	if uSum != 1500 {
		t.Errorf("суммарный график Алисы = %d, ожидалось 1500 (без ghost)", uSum)
	}
	usvc := map[string]uint64{}
	for _, s := range ud.Services {
		usvc[s.Name] = s.Total
	}
	if usvc["xray"] != 1000 || usvc["telemt"] != 500 {
		t.Errorf("разбивка по сервисам Алисы неверна: %+v", usvc)
	}
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

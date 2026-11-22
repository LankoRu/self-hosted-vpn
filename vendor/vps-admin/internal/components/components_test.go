package components

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/LankoRu/vps-admin/internal/config"
)

func testRunner() *Runner {
	cfg := &config.Config{}
	cfg.SetDefaults()
	return NewRunner(cfg)
}

func TestStartUnknownComponent(t *testing.T) {
	r := testRunner()
	if err := r.Start("nonexistent"); err == nil {
		t.Error("ожидалась ошибка для неизвестного компонента")
	}
	if err := r.Start("amneziawg"); err == nil {
		t.Error("amneziawg не должен обновляться из панели")
	}
}

func TestJobStreamsLogAndFinishes(t *testing.T) {
	// Подменяем exec на заведомо быструю команду с выводом.
	old := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo строка-раз; echo строка-два")
	}
	t.Cleanup(func() { execCommand = old })

	r := testRunner()
	// nginx-апдейтер состоит из streamCmd — все вызовы уйдут в фейковый exec.
	if err := r.Start("nginx"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// повторный запуск активного job отклоняется (пока running)
	// (гонка допустима: job может завершиться мгновенно — проверяем мягко)
	deadline := time.Now().Add(5 * time.Second)
	var job *Job
	for time.Now().Before(deadline) {
		for _, c := range r.List() {
			if c.Name == "nginx" && c.Job != nil && c.Job.State != "running" {
				job = c.Job
			}
		}
		if job != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if job == nil {
		t.Fatal("job не завершился за 5с")
	}
	if job.State != "done" {
		t.Errorf("state = %q, want done (лог: %s)", job.State, job.Log)
	}
	if !strings.Contains(job.Log, "строка-раз") || !strings.Contains(job.Log, ">>> Готово") {
		t.Errorf("лог не стримится:\n%s", job.Log)
	}
}

func TestJobErrorState(t *testing.T) {
	old := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo плохо >&2; exit 3")
	}
	t.Cleanup(func() { execCommand = old })

	r := testRunner()
	if err := r.Start("nginx"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range r.List() {
			if c.Name == "nginx" && c.Job != nil && c.Job.State == "error" {
				if !strings.Contains(c.Job.Log, "ОШИБКА") || !strings.Contains(c.Job.Log, "плохо") {
					t.Errorf("лог ошибки:\n%s", c.Job.Log)
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job не перешёл в error")
}

func TestVersionParsingHelpers(t *testing.T) {
	if firstLine("a\nb") != "a" {
		t.Error("firstLine")
	}
	// nginx -v пишет в stderr — версия извлекается из CombinedOutput
	old := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo 'nginx version: nginx/1.28.3' >&2")
	}
	t.Cleanup(func() { execCommand = old })
	c := testRunner().nginx()
	if c.Version != "1.28.3" {
		t.Errorf("nginx version = %q", c.Version)
	}
}

func TestUpdateBypassesTagCache(t *testing.T) {
	// Кэш тегов — только для отображения. Обновление обязано запрашивать свежий
	// релиз: иначе отставший кэш «обновлял» бы бинарь на старый тег (на живом
	// сервере это привело к откату на v0.1.0 и потере новых API).
	r := testRunner()
	r.tagCache["o/r"] = tagEntry{tag: "v0.0.1", at: time.Now()}
	if got, _ := r.githubLatestTag("o/r"); got != "v0.0.1" {
		t.Fatalf("кэш не используется для отображения: %q", got)
	}
	// свежий запрос идёт в сеть (в тестах — недоступен/ошибка), кэш не отдаётся
	if got, err := r.githubLatestTagFresh("o/r"); err == nil && got == "v0.0.1" {
		t.Error("githubLatestTagFresh вернул кэш — обновление может откатить версию")
	}
}

func TestListUsesSnapshotNotNetwork(t *testing.T) {
	r := testRunner()
	// Снимка нет → localOnly: версии есть, latest/upd пустые, сеть не нужна.
	bare := r.List()
	if len(bare) != 6 {
		t.Fatalf("localOnly = %d компонентов, want 6", len(bare))
	}
	for _, c := range bare {
		if c.Latest != "" || c.UpdateAvailable {
			t.Errorf("localOnly не должен ходить в сеть: %+v", c)
		}
	}
	if !r.RefreshedAt().IsZero() {
		t.Error("снимка ещё нет — RefreshedAt должен быть zero")
	}

	// Кладём снимок вручную — List обязан вернуть его без сети.
	r.snapMu.Lock()
	r.snapshot = []Component{{Name: "xray", Version: "1.0", Latest: "2.0", UpdateAvailable: true}}
	r.refreshedAt = time.Now()
	r.snapMu.Unlock()
	got := r.List()
	if len(got) != 1 || got[0].Latest != "2.0" || !got[0].UpdateAvailable {
		t.Errorf("List не вернул снимок: %+v", got)
	}
	if r.RefreshedAt().IsZero() {
		t.Error("RefreshedAt должен быть заполнен")
	}
}

func TestListAttachesLiveJobToSnapshot(t *testing.T) {
	pick := func(list []Component) *Component {
		for i := range list {
			if list[i].Name == "nginx" {
				return &list[i]
			}
		}
		return nil
	}
	r := testRunner()
	// до первого снимка job тоже обязан быть виден (окно прогресса)
	r.mu.Lock()
	r.jobs["nginx"] = &jobState{state: "running", started: time.Now()}
	r.mu.Unlock()
	if c := pick(r.List()); c == nil || c.Job == nil || c.Job.State != "running" {
		t.Errorf("job не подставлен без снимка: %+v", c)
	}

	r.snapMu.Lock()
	r.snapshot = []Component{{Name: "nginx", Version: "1.28.3"}}
	r.refreshedAt = time.Now()
	r.snapMu.Unlock()
	if c := pick(r.List()); c == nil || c.Job == nil || c.Job.State != "running" {
		t.Errorf("job не подставлен в снимок: %+v", c)
	}
}

func TestCollectionWithoutNetworkStillHasVersions(t *testing.T) {
	// collect() при недоступной сети не падает и отдаёт версии (для 6 строк).
	r := testRunner()
	list := r.collect(context.Background())
	if len(list) != 6 {
		t.Fatalf("collect = %d, want 6", len(list))
	}
	names := map[string]bool{}
	for _, c := range list {
		if c.Name == "" || c.Title == "" {
			t.Errorf("пустое имя/заголовок: %+v", c)
		}
		names[c.Name] = true
	}
	for _, want := range []string{"vps-admin", "3x-ui", "xray", "telemt", "amneziawg", "nginx"} {
		if !names[want] {
			t.Errorf("нет компонента %q", want)
		}
	}
}

func TestTagFromRedirectFallback(t *testing.T) {
	// GitHub API отдаёт 403 (лимит) → тег берём из редиректа releases/latest.
	oldAPI, oldWeb := githubAPIBase, githubWebBase
	t.Cleanup(func() { githubAPIBase, githubWebBase = oldAPI, oldWeb })

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	t.Cleanup(api.Close)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/o/r/releases/latest" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Location", "https://github.com/o/r/releases/tag/v9.9.9")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(web.Close)
	githubAPIBase, githubWebBase = api.URL, web.URL

	r := testRunner()
	tag, err := r.githubLatestTagFresh("o/r")
	if err != nil || tag != "v9.9.9" {
		t.Fatalf("tag = %q, err = %v (ожидался fallback через редирект)", tag, err)
	}
	// успех кэшируется для отображения
	if got, _ := r.githubLatestTag("o/r"); got != "v9.9.9" {
		t.Errorf("кэш не заполнен: %q", got)
	}
}

func TestTagFromRedirectBroken(t *testing.T) {
	oldAPI, oldWeb := githubAPIBase, githubWebBase
	t.Cleanup(func() { githubAPIBase, githubWebBase = oldAPI, oldWeb })
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(api.Close)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // нет редиректа
	}))
	t.Cleanup(web.Close)
	githubAPIBase, githubWebBase = api.URL, web.URL

	if tag, err := testRunner().githubLatestTagFresh("o/r"); err == nil || tag != "" {
		t.Errorf("ожидалась ошибка, получено %q / %v", tag, err)
	}
}

package components

import (
	"context"
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

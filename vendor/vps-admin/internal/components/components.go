// Package components — версии компонентов стека и их обновление из панели
// («Система» → «Компоненты»). Обновления асинхронные:job пишет лог в буфер,
// UI опрашивает GET /api/components и показывает прогресс в окне.
package components

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/version"
)

// execCommand подменяется в тестах (сигнатура exec.CommandContext).
var execCommand = exec.CommandContext

const logCap = 8000 // хвост лога job, который видит UI

// Component — компонент стека: версия, доступность обновления, состояние job.
type Component struct {
	Name            string `json:"name"`
	Title           string `json:"title"`
	Version         string `json:"version"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	Updatable       bool   `json:"updatable"`
	Note            string `json:"note,omitempty"`
	Job             *Job   `json:"job,omitempty"`
}

// Job — состояние обновления (лог виден в окне прогресса).
type Job struct {
	State    string `json:"state"` // running | done | error
	Log      string `json:"log"`
	Started  int64  `json:"started"`
	Finished int64  `json:"finished,omitempty"`
}

type jobState struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	state    string
	started  time.Time
	finished time.Time
}

func (j *jobState) write(p string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.buf.WriteString(p)
	if j.buf.Len() > logCap*2 {
		tail := j.buf.Bytes()[j.buf.Len()-logCap:]
		j.buf.Reset()
		j.buf.Write(tail)
	}
}

func (j *jobState) snapshot() *Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	job := &Job{State: j.state, Log: j.buf.String(), Started: j.started.Unix()}
	if !j.finished.IsZero() {
		job.Finished = j.finished.Unix()
	}
	return job
}

func (j *jobState) finish(state string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.state = state
	j.finished = time.Now()
}

// Runner — сбор версий и запуск обновлений.
type Runner struct {
	cfg      *config.Config
	mu       sync.Mutex
	jobs     map[string]*jobState
	client   *http.Client
	tagMu    sync.Mutex
	tagCache map[string]tagEntry // кэш GitHub-тегов (rate limit 60/ч без токена)
}

type tagEntry struct {
	tag string
	at  time.Time
}

func NewRunner(cfg *config.Config) *Runner {
	return &Runner{
		cfg:      cfg,
		jobs:     map[string]*jobState{},
		client:   &http.Client{Timeout: 60 * time.Second},
		tagCache: map[string]tagEntry{},
	}
}

func (r *Runner) job(name string) *jobState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[name]
}

// Start запускает обновление компонента (async). Повторный запуск активного
// job отклоняется.
func (r *Runner) Start(name string) error {
	def, ok := updaters[name]
	if !ok || !def.updatable {
		return fmt.Errorf("компонент %q не обновляется из панели", name)
	}
	r.mu.Lock()
	if j := r.jobs[name]; j != nil && j.snapshot().State == "running" {
		r.mu.Unlock()
		return fmt.Errorf("обновление %s уже выполняется", name)
	}
	j := &jobState{state: "running", started: time.Now()}
	r.jobs[name] = j
	r.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		j.write(fmt.Sprintf(">>> Обновление %s\n", name))
		err := def.run(ctx, r, j)
		if err != nil {
			j.write("ОШИБКА: " + err.Error() + "\n")
			j.finish("error")
			return
		}
		j.write(">>> Готово\n")
		j.finish("done")
	}()
	return nil
}

// List собирает версии всех компонентов (best-effort: недоступные — «—»).
func (r *Runner) List() []Component {
	out := []Component{
		r.vpsAdmin(),
		r.panel3xui(),
		r.xray(),
		r.telemt(),
		r.amneziawg(),
		r.nginx(),
	}
	for i := range out {
		if j := r.job(out[i].Name); j != nil {
			out[i].Job = j.snapshot()
		}
	}
	return out
}

func runOut(ctx context.Context, name string, args ...string) (string, error) {
	c := execCommand(ctx, name, args...)
	b, err := c.Output()
	return strings.TrimSpace(string(b)), err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// --- версии -----------------------------------------------------------------

func (r *Runner) vpsAdmin() Component {
	c := Component{Name: "vps-admin", Title: "vps-admin (панель/бот)", Version: version.Version, Updatable: true}
	if tag, err := r.githubLatestTag(r.cfg.Update.GithubRepo); err == nil {
		c.Latest = tag
		// Бейдж «доступно обновление» — только для релизных сборок (vX.Y.Z):
		// dev-сборка новее релиза, и «обновление» было бы даунгрейдом.
		isRelease := strings.HasPrefix(version.Version, "v") && strings.Count(version.Version, ".") == 2
		c.UpdateAvailable = isRelease && tag != "" && tag != version.Version
		if !isRelease {
			c.Note = "локальная/dev-сборка; «Обновить» поставит стабильный релиз"
		}
	}
	return c
}

func (r *Runner) panel3xui() Component {
	c := Component{Name: "3x-ui", Title: "3x-ui (панель)", Updatable: true}
	var info struct {
		Success bool `json:"success"`
		Obj     struct {
			CurrentVersion  string `json:"currentVersion"`
			LatestVersion   string `json:"latestVersion"`
			UpdateAvailable bool   `json:"updateAvailable"`
		} `json:"obj"`
	}
	if err := r.panelGet("/panel/api/server/getPanelUpdateInfo", &info); err == nil && info.Success {
		c.Version = info.Obj.CurrentVersion
		c.Latest = strings.TrimPrefix(info.Obj.LatestVersion, "v")
		c.UpdateAvailable = info.Obj.UpdateAvailable
		return c
	}
	if v, err := runOut(context.Background(), "/usr/local/x-ui/x-ui", "-v"); err == nil {
		c.Version = firstLine(v)
	} else {
		c.Version = "—"
	}
	return c
}

func (r *Runner) xray() Component {
	c := Component{Name: "xray", Title: "Xray-core", Updatable: true}
	bin := "/usr/local/x-ui/bin/xray-linux-" + runtime.GOARCH
	if v, err := runOut(context.Background(), bin, "-version"); err == nil {
		// "Xray 26.10.21 (Xray, ...) ..." → берём второе поле
		f := strings.Fields(firstLine(v))
		if len(f) >= 2 {
			c.Version = f[1]
		}
	} else {
		c.Version = "—"
	}
	var vers struct {
		Success bool     `json:"success"`
		Obj     []string `json:"obj"`
	}
	if err := r.panelGet("/panel/api/server/getXrayVersion", &vers); err == nil && vers.Success && len(vers.Obj) > 0 {
		c.Latest = strings.TrimPrefix(vers.Obj[0], "v")
		c.UpdateAvailable = c.Version != "—" && c.Latest != c.Version
	}
	return c
}

func (r *Runner) telemt() Component {
	c := Component{Name: "telemt", Title: "telemt (MTProto)", Updatable: true}
	if v, err := runOut(context.Background(), "telemt", "--version"); err == nil {
		f := strings.Fields(firstLine(v))
		if len(f) >= 2 {
			c.Version = f[len(f)-1]
		}
	} else {
		c.Version = "—"
	}
	if tag, err := r.githubLatestTag("telemt/telemt"); err == nil {
		c.Latest = strings.TrimPrefix(tag, "v")
		c.UpdateAvailable = c.Version != "—" && c.Latest != "" && c.Latest != c.Version
	}
	return c
}

func (r *Runner) amneziawg() Component {
	c := Component{Name: "amneziawg", Title: "AmneziaWG (модуль + tools)", Updatable: false,
		Note: "Обновление модуля ядра — через установщик (DKMS + патч Ubuntu 7.0)"}
	if v, err := runOut(context.Background(), "awg", "--version"); err == nil {
		f := strings.Fields(firstLine(v))
		if len(f) >= 2 {
			c.Version = strings.TrimPrefix(f[1], "v")
		}
	} else {
		c.Version = "—"
	}
	if v, err := runOut(context.Background(), "modinfo", "-F", "version", "amneziawg"); err == nil && v != "" {
		c.Version += " / ядро: " + v
	}
	return c
}

func (r *Runner) nginx() Component {
	c := Component{Name: "nginx", Title: "nginx (SNI-роутер)", Updatable: true, Note: "Обновление из apt-репозитория дистрибутива"}
	c.Version = "—"
	c2 := execCommand(context.Background(), "nginx", "-v")
	b, _ := c2.CombinedOutput() // nginx -v пишет в stderr
	if i := strings.Index(string(b), "nginx/"); i >= 0 {
		c.Version = strings.Fields(string(b)[i+6:])[0]
	}
	return c
}

// --- GitHub -----------------------------------------------------------------

func (r *Runner) githubLatestTag(repo string) (string, error) {
	r.tagMu.Lock()
	if e, ok := r.tagCache[repo]; ok && time.Since(e.at) < 10*time.Minute {
		r.tagMu.Unlock()
		return e.tag, nil
	}
	r.tagMu.Unlock()
	return r.githubLatestTagFresh(repo)
}

// githubLatestTagFresh — всегда сетезапрос, без кэша. Используется ПРИ
// ОБНОВЛЕНИИ: кэш (10 мин) годится для отображения, но не для выбора версии —
// иначе свежий релиз пропускался бы, а при отставшем кэше можно было бы
// «обновиться» на старый тег (откат, случившийся на живом сервере).
func (r *Runner) githubLatestTagFresh(repo string) (string, error) {
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	if r.cfg.Update.GithubToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.Update.GithubToken)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github status %d", resp.StatusCode)
	}
	var d struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return "", err
	}
	r.tagMu.Lock()
	r.tagCache[repo] = tagEntry{tag: d.TagName, at: time.Now()}
	r.tagMu.Unlock()
	return d.TagName, nil
}

func (r *Runner) githubDownload(url string, w io.Writer) error {
	req, _ := http.NewRequest("GET", url, nil)
	if r.cfg.Update.GithubToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.Update.GithubToken)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download status %d", resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// --- 3x-ui panel API ----------------------------------------------------------

func (r *Runner) panelURL(endpoint string) string {
	base := strings.TrimRight(r.cfg.Xray3xUI.BaseURL, "/")
	path := strings.Trim(r.cfg.Xray3xUI.WebBasePath, "/")
	if path == "" {
		return base + endpoint
	}
	return base + "/" + path + "/" + strings.TrimLeft(endpoint, "/")
}

func (r *Runner) panelGet(endpoint string, out interface{}) error {
	req, _ := http.NewRequest("GET", r.panelURL(endpoint), nil)
	req.Header.Set("Authorization", "Bearer "+r.cfg.Xray3xUI.APIToken)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) vps-admin")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("panel status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (r *Runner) panelPost(endpoint string) error {
	req, _ := http.NewRequest("POST", r.panelURL(endpoint), nil)
	req.Header.Set("Authorization", "Bearer "+r.cfg.Xray3xUI.APIToken)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) vps-admin")
	resp, err := r.client.Do(req)
	if err != nil {
		return err // панель могла перезапуститься во время запроса — ожидаемо
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("panel status %d", resp.StatusCode)
	}
	return nil
}

// --- обновления ---------------------------------------------------------------

type updaterDef struct {
	updatable bool
	run       func(ctx context.Context, r *Runner, j *jobState) error
}

var updaters = map[string]updaterDef{
	"vps-admin": {true, updateVpsAdmin},
	"3x-ui":     {true, update3xui},
	"xray":      {true, updateXray},
	"telemt":    {true, updateTelemt},
	"nginx":     {true, updateNginx},
}

// streamCmd выполняет команду, стримя вывод в лог job (видно в окне прогресса).
func streamCmd(ctx context.Context, j *jobState, name string, args ...string) error {
	j.write("$ " + name + " " + strings.Join(args, " ") + "\n")
	c := execCommand(ctx, name, args...)
	w := &jobWriter{j}
	c.Stdout = w
	c.Stderr = w
	return c.Run()
}

type jobWriter struct{ j *jobState }

func (w *jobWriter) Write(p []byte) (int, error) {
	w.j.write(string(p))
	return len(p), nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// updateVpsAdmin — self-update: бинарь из Releases своего репо + sha256 +
// атомарная замена + отложенный перезапуск (ответ UI успевает уйти).
func updateVpsAdmin(ctx context.Context, r *Runner, j *jobState) error {
	tag, err := r.githubLatestTagFresh(r.cfg.Update.GithubRepo)
	if err != nil || tag == "" {
		return fmt.Errorf("не удалось получить последний релиз: %v", err)
	}
	j.write("Последний релиз: " + tag + " (текущий: " + version.Version + ")\n")
	asset := "vps-admin-linux-" + runtime.GOARCH
	base := "https://github.com/" + r.cfg.Update.GithubRepo + "/releases/download/" + tag + "/"
	tmp, err := os.MkdirTemp("", "vps-admin-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	j.write("Скачивание " + asset + "...\n")
	binPath := tmp + "/" + asset
	f, err := os.Create(binPath)
	if err != nil {
		return err
	}
	if err := r.githubDownload(base+asset, f); err != nil {
		f.Close()
		return err
	}
	f.Close()
	sumsPath := tmp + "/sha256sums.txt"
	sf, err := os.Create(sumsPath)
	if err != nil {
		return err
	}
	if err := r.githubDownload(base+"sha256sums.txt", sf); err != nil {
		sf.Close()
		return err
	}
	sf.Close()
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return err
	}
	var want string
	for _, line := range strings.Split(string(sums), "\n") {
		if strings.HasSuffix(line, " "+asset) {
			want = strings.Fields(line)[0]
		}
	}
	got, err := sha256File(binPath)
	if err != nil {
		return err
	}
	if want == "" || got != want {
		return fmt.Errorf("sha256 не совпала (получено %s, ожидалось %s)", got, want)
	}
	j.write("sha256: OK\n")

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := streamCmd(ctx, j, "install", "-m", "755", binPath, self+".new"); err != nil {
		return err
	}
	if err := os.Rename(self+".new", self); err != nil {
		return err
	}
	j.write("Бинарь заменён: " + self + "\nПерезапуск сервиса через 2 с (страница потеряет связь на пару секунд)...\n")
	go func() {
		time.Sleep(2 * time.Second)
		_ = exec.Command("systemctl", "restart", "vps-admin").Run()
	}()
	return nil
}

// update3xui — официальное обновление панели через API (данные сохраняются).
// Панель перезапускается во время запроса — обрыв соединения ожидаем.
func update3xui(ctx context.Context, r *Runner, j *jobState) error {
	j.write("Запрос обновления панели (POST /server/updatePanel)...\n")
	err := r.panelPost("/panel/api/server/updatePanel")
	if err != nil {
		j.write("Соединение прервано (панель перезапускается — ожидаемо): " + err.Error() + "\n")
	}
	j.write("Ожидание возврата панели...\n")
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
		var info struct {
			Success bool `json:"success"`
			Obj     struct {
				CurrentVersion string `json:"currentVersion"`
			} `json:"obj"`
		}
		if err := r.panelGet("/panel/api/server/getPanelUpdateInfo", &info); err == nil && info.Success {
			j.write("Панель вернулась, версия: " + info.Obj.CurrentVersion + "\n")
			return nil
		}
		j.write(".")
	}
	return fmt.Errorf("панель не вернулась за 4 минуты (проверьте: systemctl status x-ui)")
}

// updateXray — установка последней версии Xray через API панели.
func updateXray(ctx context.Context, r *Runner, j *jobState) error {
	var vers struct {
		Success bool     `json:"success"`
		Obj     []string `json:"obj"`
	}
	if err := r.panelGet("/panel/api/server/getXrayVersion", &vers); err != nil || !vers.Success || len(vers.Obj) == 0 {
		return fmt.Errorf("не удалось получить список версий Xray: %v", err)
	}
	latest := vers.Obj[0]
	j.write("Установка Xray " + latest + " (POST /server/installXray)...\n")
	if err := r.panelPost("/panel/api/server/installXray/" + latest); err != nil {
		j.write("Запрос завершился с ошибкой соединения (xray перезапускается — ожидаемо): " + err.Error() + "\n")
	}
	j.write("Ожидание запуска xray...\n")
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
		bin := "/usr/local/x-ui/bin/xray-linux-" + runtime.GOARCH
		if v, err := runOut(ctx, bin, "-version"); err == nil && strings.Contains(v, strings.TrimPrefix(latest, "v")) {
			j.write("Xray обновлён: " + firstLine(v) + "\n")
			return nil
		}
		j.write(".")
	}
	return fmt.Errorf("xray не вернулся с версией %s за 3 минуты", latest)
}

// updateTelemt — бинарь из GitHub Releases telemt (tar.gz + sha256), конфиг
// и пользователи не трогаются.
func updateTelemt(ctx context.Context, r *Runner, j *jobState) error {
	tag, err := r.githubLatestTagFresh("telemt/telemt")
	if err != nil || tag == "" {
		return fmt.Errorf("не удалось получить релиз telemt: %v", err)
	}
	j.write("Последний релиз telemt: " + tag + "\n")
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64"
	}
	asset := "telemt-" + arch + "-linux-gnu.tar.gz"
	base := "https://github.com/telemt/telemt/releases/download/" + tag + "/"
	tmp, err := os.MkdirTemp("", "telemt-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	j.write("Скачивание " + asset + "...\n")
	for _, name := range []string{asset, asset + ".sha256"} {
		f, err := os.Create(tmp + "/" + name)
		if err != nil {
			return err
		}
		if err := r.githubDownload(base+name, f); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	sums, err := os.ReadFile(tmp + "/" + asset + ".sha256")
	if err != nil {
		return err
	}
	want := strings.Fields(string(sums))[0]
	got, err := sha256File(tmp + "/" + asset)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("sha256 не совпала")
	}
	j.write("sha256: OK\nРаспаковка...\n")
	if err := streamCmd(ctx, j, "tar", "-xzf", tmp+"/"+asset, "-C", tmp); err != nil {
		return err
	}
	bin, err := exec.LookPath("telemt")
	if err != nil {
		return fmt.Errorf("telemt не найден в PATH: %w", err)
	}
	if err := streamCmd(ctx, j, "install", "-m", "755", tmp+"/telemt", bin+".new"); err != nil {
		return err
	}
	if err := os.Rename(bin+".new", bin); err != nil {
		return err
	}
	j.write("Бинарь заменён: " + bin + "\n")
	if err := streamCmd(ctx, j, "systemctl", "restart", "telemt"); err != nil {
		return err
	}
	time.Sleep(2 * time.Second)
	if v, err := runOut(ctx, "telemt", "--version"); err == nil {
		j.write("telemt: " + firstLine(v) + "\n")
	}
	return nil
}

// updateNginx — обновление пакета из apt + reload.
func updateNginx(ctx context.Context, r *Runner, j *jobState) error {
	if err := streamCmd(ctx, j, "apt-get", "update"); err != nil {
		return err
	}
	if err := streamCmd(ctx, j, "apt-get", "install", "-y", "--only-upgrade", "nginx"); err != nil {
		return err
	}
	if err := streamCmd(ctx, j, "nginx", "-t"); err != nil {
		return err
	}
	return streamCmd(ctx, j, "systemctl", "reload", "nginx")
}

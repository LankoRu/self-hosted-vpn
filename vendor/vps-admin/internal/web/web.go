// Package web — HTTP-панель управления: создание пользователей, просмотр их
// статистики и настроек по каждому сервису.
package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html"
	"io/fs"
	"log"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LankoRu/vps-admin/internal/components"
	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/manager"
	"github.com/LankoRu/vps-admin/internal/service"
	"github.com/LankoRu/vps-admin/internal/store"
	"github.com/LankoRu/vps-admin/internal/sysinfo"
	"github.com/LankoRu/vps-admin/internal/util"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed static
var staticFS embed.FS

func init() {
	// Go не знает это расширение, а без правильного MIME браузер не примет
	// манифест и не предложит установку PWA.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// periods — сопоставление имени периода и SQL-условия по timestamp.
var periods = map[string]string{
	"today": "timestamp >= datetime('now','start of day')",
	"week":  "timestamp >= datetime('now','-7 days')",
	"month": "timestamp >= datetime('now','start of month')",
	"full":  "1=1",
}

func whereFor(period string) (string, string) {
	if w, ok := periods[period]; ok {
		return period, w
	}
	return "full", periods["full"]
}

// Server — веб-панель.
type Server struct {
	cfg          *config.Config
	store        *store.Store
	mgr          *manager.Manager
	comp         *components.Runner // версии/обновления компонентов стека
	sessionToken string             // валидное значение cookie после входа
	limiter      *loginLimiter      // защита входа от перебора пароля
}

func New(cfg *config.Config, st *store.Store, mgr *manager.Manager) *Server {
	// Токен сессии детерминирован от пароля: стабилен между рестартами (деплой
	// не разлогинивает), но автоматически инвалидируется при смене пароля.
	sum := sha256.Sum256([]byte("vps-admin-session\x00" + cfg.Web.Password))
	return &Server{cfg: cfg, store: st, mgr: mgr, comp: components.NewRunner(cfg),
		sessionToken: hex.EncodeToString(sum[:]), limiter: newLoginLimiter()}
}

// Start запускает HTTP-сервер (блокирующий вызов; вызывать в горутине).
func (s *Server) Start() {
	// Снимок версий компонентов обновляется раз в час: страница читает кэш,
	// а не дёргает GitHub на каждый показ (лимит 60 запросов/час на IP).
	go s.comp.StartRefresher(context.Background())
	if s.requiresAuth() && s.cfg.Web.Password == "" {
		log.Printf("[web] ОТКЛЮЧЕНО: listen=%s не loopback, но пароль не задан (web.password)", s.cfg.Web.Listen)
		return
	}
	if s.cfg.Web.Password == "" {
		log.Printf("[web] ВНИМАНИЕ: пароль не задан — панель доступна без авторизации (только на %s)", s.cfg.Web.Listen)
	}

	addr := s.cfg.Web.Listen
	handler := s.Handler()

	if !s.cfg.Web.TLS.Enabled {
		log.Printf("[web] панель на http://%s", addr)
		if err := http.ListenAndServe(addr, handler); err != nil {
			log.Printf("[web] сервер остановлен: %v", err)
		}
		return
	}

	srv := &http.Server{Addr: addr, Handler: handler}
	// Свой сертификат, если задан. Читаем его через certReloader: acme.sh
	// перевыпускает Let's Encrypt (особенно shortlived-профиль на IP — ~6 дней)
	// прямо под работающим сервисом, и без перечитывания мы бы отдавали
	// просроченный сертификат до ручного рестарта.
	if c, k := s.cfg.Web.TLS.CertFile, s.cfg.Web.TLS.KeyFile; c != "" && k != "" {
		rl, err := newCertReloader(c, k)
		if err != nil {
			log.Printf("[web] не удалось загрузить сертификат %s: %v", c, err)
			return
		}
		srv.TLSConfig = &tls.Config{GetCertificate: rl.GetCertificate}
		log.Printf("[web] HTTPS на https://%s (сертификат %s, перечитывается при обновлении)", addr, c)
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			log.Printf("[web] HTTPS остановлен: %v", err)
		}
		return
	}
	// Иначе — самоподписанный (мгновенный HTTPS; браузер покажет предупреждение).
	// Кэшируем на диск, чтобы не перевыпускать каждый рестарт (иначе браузер
	// показывал бы новое предупреждение и сбрасывал бы «доверять» при каждом деплое).
	hosts := s.certHosts()
	cert, err := s.loadOrCreateSelfSigned(hosts)
	if err != nil {
		log.Printf("[web] не удалось получить самоподписанный сертификат: %v", err)
		return
	}
	srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	log.Printf("[web] HTTPS (самоподписанный сертификат) на https://%s | SAN: %s", addr, strings.Join(hosts, ", "))
	if err := srv.ListenAndServeTLS("", ""); err != nil {
		log.Printf("[web] HTTPS остановлен: %v", err)
	}
}

// listenHost возвращает host-часть из web.listen.
func (s *Server) listenHost() string {
	host := s.cfg.Web.Listen
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

// certHosts — какие имена/IP положить в SAN самоподписанного сертификата:
// host из web.listen (или, если слушаем все интерфейсы, все их IP) плюс явные
// из web.tls.host. Без этого браузер отвергает подключение по IP, которого нет
// в сертификате.
func (s *Server) certHosts() []string {
	var hosts []string
	switch h := s.listenHost(); h {
	case "", "0.0.0.0", "::":
		hosts = append(hosts, localIPs()...) // слушаем всё → все IP интерфейсов
	default:
		hosts = append(hosts, h)
	}
	for _, x := range strings.Split(s.cfg.Web.TLS.Host, ",") {
		if x = strings.TrimSpace(x); x != "" {
			hosts = append(hosts, x)
		}
	}
	return hosts
}

// localIPs — все не-loopback IP интерфейсов хоста.
func localIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			out = append(out, ipnet.IP.String())
		}
	}
	return out
}

// certReloader держит пару cert/key из файлов и перечитывает её, когда файлы
// на диске изменились. Нужен для сертификатов, которые обновляет внешний агент
// (acme.sh/certbot) без перезапуска сервиса.
type certReloader struct {
	certFile, keyFile string

	mu      sync.RWMutex
	cert    *tls.Certificate
	modCert time.Time
	modKey  time.Time
}

func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *certReloader) reload() error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}
	mc, mk := modTime(r.certFile), modTime(r.keyFile)
	r.mu.Lock()
	r.cert, r.modCert, r.modKey = &cert, mc, mk
	r.mu.Unlock()
	return nil
}

func modTime(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// GetCertificate — колбэк tls.Config: перед выдачей сертификата дёшево сверяет
// mtime файлов и перечитывает пару, если её обновили. Если перечитать не
// удалось (агент пишет файл прямо сейчас) — отдаём прежний рабочий сертификат.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	mc, mk := modTime(r.certFile), modTime(r.keyFile)
	r.mu.RLock()
	stale := !mc.Equal(r.modCert) || !mk.Equal(r.modKey)
	r.mu.RUnlock()
	if stale {
		if err := r.reload(); err != nil {
			log.Printf("[web] сертификат обновился, но перечитать не вышло: %v", err)
		} else {
			log.Printf("[web] сертификат перечитан после обновления (%s)", r.certFile)
		}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}

// certCachePaths — куда кладём кэш самоподписанного сертификата (рядом с БД,
// каталог уже принадлежит сервису и доступен на запись).
func (s *Server) certCachePaths() (crt, key string) {
	dir := filepath.Dir(s.cfg.DBPath)
	return filepath.Join(dir, "web-selfsigned.crt"), filepath.Join(dir, "web-selfsigned.key")
}

// loadOrCreateSelfSigned возвращает самоподписанный сертификат из кэша на диске;
// если его нет, он истекает или не покрывает нужные SAN-хосты — генерирует новый
// и сохраняет. За счёт этого сертификат стабилен между рестартами/деплоями.
func (s *Server) loadOrCreateSelfSigned(hosts []string) (tls.Certificate, error) {
	crtPath, keyPath := s.certCachePaths()
	if cert, ok := loadCachedCert(crtPath, keyPath, hosts); ok {
		log.Printf("[web] самоподписанный сертификат из кэша: %s", crtPath)
		return cert, nil
	}
	cert, der, priv, err := generateSelfSigned(hosts)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := saveCert(crtPath, keyPath, der, priv); err != nil {
		log.Printf("[web] не удалось сохранить сертификат в кэш (%v) — работаю без кэша", err)
	} else {
		log.Printf("[web] сгенерирован и сохранён самоподписанный сертификат: %s", crtPath)
	}
	return cert, nil
}

// loadCachedCert читает кэшированный сертификат и проверяет, что он ещё годен:
// не истекает в ближайший месяц и покрывает все требуемые хосты (SAN).
func loadCachedCert(crtPath, keyPath string, hosts []string) (tls.Certificate, bool) {
	cert, err := tls.LoadX509KeyPair(crtPath, keyPath)
	if err != nil {
		return tls.Certificate{}, false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, false
	}
	if time.Now().Add(30 * 24 * time.Hour).After(leaf.NotAfter) {
		return tls.Certificate{}, false // скоро истечёт — перевыпустим
	}
	if !certCoversHosts(leaf, hosts) {
		return tls.Certificate{}, false // изменился набор IP/доменов
	}
	cert.Leaf = leaf
	return cert, true
}

// certCoversHosts проверяет, что все хосты уже есть в SAN сертификата.
func certCoversHosts(leaf *x509.Certificate, hosts []string) bool {
	dns := map[string]bool{}
	for _, d := range leaf.DNSNames {
		dns[d] = true
	}
	ips := map[string]bool{}
	for _, ip := range leaf.IPAddresses {
		ips[ip.String()] = true
	}
	for _, h := range hosts {
		switch h {
		case "", "localhost", "127.0.0.1", "::1":
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			if !ips[ip.String()] {
				return false
			}
		} else if !dns[h] {
			return false
		}
	}
	return true
}

// saveCert сохраняет сертификат и ключ в PEM (ключ — с правами 0600).
func saveCert(crtPath, keyPath string, der []byte, priv *ecdsa.PrivateKey) error {
	crtPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(crtPath, crtPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, keyPEM, 0o600)
}

// selfSignedCert генерирует самоподписанный TLS-сертификат (обёртка над
// generateSelfSigned без кэша; используется в тестах).
func selfSignedCert(hosts []string) (tls.Certificate, error) {
	cert, _, _, err := generateSelfSigned(hosts)
	return cert, err
}

// generateSelfSigned создаёт самоподписанный TLS-сертификат (ECDSA P-256, 10 лет)
// с SAN на localhost и на все переданные host'ы (IP → IPAddresses, иначе DNSNames).
func generateSelfSigned(hosts []string) (tls.Certificate, []byte, *ecdsa.PrivateKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "vps-admin"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	for _, h := range hosts {
		switch h {
		case "", "localhost", "127.0.0.1", "::1":
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, der, priv, nil
}

// Handler собирает роутер с middleware авторизации.
func (s *Server) Handler() http.Handler {
	staticSub, _ := fs.Sub(staticFS, "static")

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.FS(staticSub)))
	mux.HandleFunc("GET /api/qr", s.handleQR)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/system", s.handleSystem)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/charts", s.handleCharts)
	mux.HandleFunc("GET /api/users", s.handleListUsers)
	mux.HandleFunc("POST /api/users", s.handleCreateUser)
	mux.HandleFunc("POST /api/regenerate-all", s.handleRegenerateAll)
	mux.HandleFunc("GET /api/users/{username}", s.handleUserDetail)
	mux.HandleFunc("GET /api/users/{username}/charts", s.handleUserCharts)
	mux.HandleFunc("PATCH /api/users/{username}", s.handleRenameUser)
	mux.HandleFunc("DELETE /api/users/{username}", s.handleDeleteUser)
	mux.HandleFunc("POST /api/users/{username}/regenerate", s.handleRegenerate)
	mux.HandleFunc("GET /api/connections", s.handleConnectionsState)
	mux.HandleFunc("GET /api/components", s.handleComponents)
	mux.HandleFunc("POST /api/components/refresh", s.handleComponentsRefresh)
	mux.HandleFunc("POST /api/components/{name}/update", s.handleComponentUpdate)
	mux.HandleFunc("POST /api/connections/reality", s.handleConnectionsReality)
	mux.HandleFunc("POST /api/connections/telemt", s.handleConnectionsTelemt)
	mux.HandleFunc("POST /api/connections/backup", s.handleConnectionsBackup)
	mux.HandleFunc("POST /api/users/{username}/devices", s.handleAddDevice)
	mux.HandleFunc("DELETE /api/users/{username}/devices/{device}", s.handleRemoveDevice)
	mux.HandleFunc("POST /api/users/{username}/share", s.handleShareCreate)
	mux.HandleFunc("DELETE /api/users/{username}/share", s.handleShareRevoke)
	// Публичные (без авторизации) — см. isPublicPath.
	mux.HandleFunc("GET /p/{token}", s.handlePublicPage)
	mux.HandleFunc("GET /p/{token}/data", s.handlePublicData)
	mux.HandleFunc("GET /p/{token}/file", s.handlePublicFile)
	return s.auth(mux)
}

// requiresAuth — true, если сервис слушает не на loopback.
func (s *Server) requiresAuth() bool {
	switch s.listenHost() {
	case "", "127.0.0.1", "::1", "localhost":
		return false
	default:
		return true
	}
}

// auth — middleware авторизации по паролю через session-cookie (без логина).
// Если пароль не задан — доступ открыт (актуально только для loopback).
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if s.cfg.Web.Password == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/login" {
			s.handleLogin(w, r)
			return
		}
		if s.authed(r) {
			next.ServeHTTP(w, r)
			return
		}
		// Не авторизован: API → 401, страницы → форма входа.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusUnauthorized, "требуется вход")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(s.loginPageHTML("")))
	})
}

// isPublicPath — пути, доступные без авторизации: публичная страница с ключами
// (защищена самим неугадываемым токеном), генератор QR (чистая функция
// text→PNG, ничего из БД не отдаёт) и статика PWA.
//
// Манифест и иконки обязаны быть публичными: по спецификации браузер тянет
// manifest БЕЗ cookie, и за авторизацией он получил бы HTML формы входа —
// установка приложения не предлагалась бы. Секретов в этих файлах нет.
func isPublicPath(p string) bool {
	switch p {
	case "/api/qr", "/manifest.webmanifest", "/sw.js", "/icon-192.png", "/icon-512.png":
		return true
	}
	return p == "/p" || strings.HasPrefix(p, "/p/")
}

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie("vps_session")
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.sessionToken)) == 1
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	form := strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded")
	if d := s.limiter.blockedFor(ip); d > 0 {
		mins := int(d.Minutes()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		msg := fmt.Sprintf("слишком много попыток входа — подождите %d мин", mins)
		if form {
			s.writeLoginPage(w, msg)
			return
		}
		writeErr(w, http.StatusTooManyRequests, msg)
		return
	}
	var username, password string
	if form {
		if err := r.ParseForm(); err != nil {
			writeErr(w, http.StatusBadRequest, "неверный запрос")
			return
		}
		username, password = r.PostFormValue("username"), r.PostFormValue("password")
	} else {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "неверный запрос")
			return
		}
		username, password = req.Username, req.Password
	}
	// Логин фиксированный и единственный (нужен менеджерам паролей). В JSON
	// допускается отсутствие username (старые клиенты/тесты), но присланный
	// обязан совпадать.
	if username != "" && subtle.ConstantTimeCompare([]byte(username), []byte(s.cfg.Web.Login)) != 1 {
		s.limiter.fail(ip)
		log.Printf("[web] неудачный вход (логин) с %s", ip)
		if form {
			s.writeLoginPage(w, "Неверный логин")
			return
		}
		writeErr(w, http.StatusUnauthorized, "неверный логин")
		return
	}
	if subtle.ConstantTimeCompare([]byte(password), []byte(s.cfg.Web.Password)) != 1 {
		s.limiter.fail(ip)
		log.Printf("[web] неудачный вход с %s", ip)
		if form {
			s.writeLoginPage(w, "Неверный пароль")
			return
		}
		writeErr(w, http.StatusUnauthorized, "неверный пароль")
		return
	}
	s.limiter.reset(ip)
	http.SetCookie(w, &http.Cookie{
		Name:     "vps_session",
		Value:    s.sessionToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil, // над HTTPS помечаем cookie как Secure
		SameSite: http.SameSiteStrictMode,
		MaxAge:   7 * 24 * 3600,
	})
	if form {
		// Нативная отправка формы: редирект обратно, откуда пришли (менеджеры
		// паролей, в т.ч. Safari, сохраняют пароль именно на реальной отправке
		// формы). Из Referer берём только путь — редирект остаётся на этом же хосте.
		dest := "/"
		if ref, err := url.Parse(r.Referer()); err == nil && ref.Path != "" {
			dest = ref.Path
			if ref.RawQuery != "" {
				dest += "?" + ref.RawQuery
			}
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) writeLoginPage(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(s.loginPageHTML(errMsg)))
}

// handleLogout сбрасывает session-cookie — пользователь снова увидит форму входа.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "vps_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1, // немедленно удалить cookie
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// loginPageHTML — форма входа: фиксированный логин + пароль. Нативный <form>
// POST (без fetch): менеджеры паролей (Safari в первую очередь) предлагают
// сохранить пароль только на реальной отправке формы; autocomplete-атрибуты
// подсказывают им поля. Логин подставляется из конфига (web.login) и readonly —
// он единственный.
func (s *Server) loginPageHTML(errMsg string) string {
	errHTML := ""
	if errMsg != "" {
		errHTML = `<div class="err">` + html.EscapeString(errMsg) + `</div>`
	}
	return `<!doctype html><html lang="ru"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>vps-admin — вход</title>
<style>
body{margin:0;height:100vh;display:flex;align-items:center;justify-content:center;
  background:#0f1216;color:#e6eaef;font:14px -apple-system,Segoe UI,Roboto,sans-serif}
.box{background:#171b21;border:1px solid #2a323c;border-radius:14px;padding:26px;width:300px;text-align:center}
h1{font-size:18px;margin:0 0 16px}
input{width:100%;box-sizing:border-box;background:#1e242c;border:1px solid #2a323c;color:#e6eaef;
  border-radius:8px;padding:10px 12px;margin-bottom:10px;font:inherit}
input:focus{outline:none;border-color:#4f9cff}
input[readonly]{color:#8b949e}
button{width:100%;background:#2b6fd6;border:none;color:#fff;border-radius:8px;padding:10px;
  font-weight:600;cursor:pointer}
button:hover{background:#4f9cff}
.err{color:#f85149;min-height:18px;margin-top:8px;font-size:13px}
</style></head><body>
<div class="box"><h1>🛡️ vps-admin</h1>
<form method="post" action="/api/login">
<input id="u" name="username" type="text" value="` + html.EscapeString(s.cfg.Web.Login) + `" autocomplete="username" readonly>
<input id="p" name="password" type="password" placeholder="Пароль" autocomplete="current-password" autofocus>
<button type="submit">Войти</button>
</form>
` + errHTML + `</div></body></html>`
}

// handleQR отдаёт PNG QR-кода для ?text=... (генерируется на сервере, без CDN).
func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("text")
	if text == "" {
		http.Error(w, "text required", http.StatusBadRequest)
		return
	}
	png, err := qrcode.Encode(text, qrcode.Medium, 512)
	if err != nil {
		http.Error(w, "qr error: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type svcBytes struct {
	Service string `json:"service"`
	Bytes   uint64 `json:"bytes"`
}

type userBrief struct {
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	Services    []string `json:"services"`
	TotalBytes  uint64   `json:"total_bytes"`
	LastSeen    string   `json:"last_seen,omitempty"` // RFC3339 UTC; пусто — трафика не было
}

// rfc3339UTC переводит время из БД ('YYYY-MM-DD HH:MM:SS' в UTC) в RFC3339 —
// иначе браузер разберёт строку как ЛОКАЛЬНОЕ время и «последняя активность»
// уедет на величину часового пояса.
func rfc3339UTC(ts string) string {
	t, err := time.Parse("2006-01-02 15:04:05", ts)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// handleSystem отдаёт метрики ресурсов сервера (CPU/RAM/диск/uptime).
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	info := sysinfo.Collect()
	dbSize, _ := s.store.GetSize()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"system":  info,
		"db_size": dbSize,
	})
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	period, where := whereFor(r.URL.Query().Get("period"))

	rows, err := s.store.TrafficByServiceIdentity(where)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	byService := map[string]uint64{}
	byUser := map[string]uint64{}
	var total uint64
	for _, row := range rows {
		byService[row.Service] += row.Total
		byUser[row.Label()] += row.Total
		total += row.Total
	}

	services := make([]svcBytes, 0, len(byService))
	for svc, b := range byService {
		services = append(services, svcBytes{svc, b})
	}
	users := make([]svcBytes, 0, len(byUser))
	for u, b := range byUser {
		users = append(users, svcBytes{u, b})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"period":       period,
		"total":        total,
		"services":     services,
		"users":        users,
		"auth":         s.cfg.Web.Password != "",  // есть ли что «разлогинивать»
		"provisioners": s.mgr.ProvisionServices(), // сервисы с регистрацией/перегенерацией
	})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	// Итог по каждому пользователю — за выбранный период (по умолчанию всё время).
	_, where := whereFor(r.URL.Query().Get("period"))
	users, err := s.store.ListUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	totals, _ := s.store.TrafficByUser(where)
	// Активность считаем за всё время, а не за выбранный период: иначе при
	// периоде «сегодня» у вчерашних пользователей она пропадала бы.
	lastSeen, _ := s.store.LastSeenByUser()

	out := make([]userBrief, 0, len(users))
	for _, u := range users {
		ids, _ := s.store.GetIdentities(u.ID)
		svcs := make([]string, 0, len(ids))
		for _, id := range ids {
			svcs = append(svcs, id.Service)
		}
		out = append(out, userBrief{
			Username:    u.Username,
			DisplayName: u.DisplayName,
			Services:    svcs,
			TotalBytes:  totals[u.ID],
			LastSeen:    rfc3339UTC(lastSeen[u.ID]),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "неверный JSON")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.DisplayName = strings.TrimSpace(req.DisplayName)

	var user *store.User
	var results []manager.ServiceResult
	var err error
	if req.Username != "" {
		// legacy-путь API: явный логин (тесты/внешние интеграции)
		user, results, err = s.mgr.Register(req.Username, req.DisplayName)
	} else {
		// основной путь: только отображаемое имя, логин генерируется
		user, results, err = s.mgr.RegisterByName(req.DisplayName)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   err.Error(),
			"results": results,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user":    user,
		"results": results,
	})
}

func (s *Server) handleUserDetail(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	user, err := s.store.GetUserByName(username)
	if err != nil {
		writeErr(w, http.StatusNotFound, "пользователь не найден")
		return
	}

	ids, _ := s.store.GetIdentities(user.ID)

	// Трафик по периодам.
	traffic := map[string]interface{}{}
	for _, p := range []string{"today", "week", "month", "full"} {
		byService, _ := s.store.TrafficForUser(user.ID, periods[p])
		var total uint64
		svcs := make([]svcBytes, 0, len(byService))
		for svc, b := range byService {
			svcs = append(svcs, svcBytes{svc, b})
			total += b
		}
		traffic[p] = map[string]interface{}{"services": svcs, "total": total}
	}

	// Активность: суммарная по пользователю и по каждому сервису.
	lastSeenBySvc, _ := s.store.LastSeenForUser(user.ID)
	activity := make(map[string]string, len(lastSeenBySvc))
	overallSeen := ""
	for svc, ts := range lastSeenBySvc {
		activity[svc] = rfc3339UTC(ts)
		if ts > overallSeen { // строки UTC сравнимы лексикографически
			overallSeen = ts
		}
	}

	shareToken, _ := s.store.GetShareToken(user.ID)
	sharePath := ""
	if shareToken != "" {
		sharePath = "/p/" + shareToken
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user":       user,
		"identities": ids,
		"traffic":    traffic,
		"last_seen":  rfc3339UTC(overallSeen), // суммарно по пользователю
		"activity":   activity,                // сервис → время последней активности
		"share_path": sharePath,               // "" — публичной ссылки нет
		// Самоподписанный сертификат: встроенные браузеры (Telegram) откроют
		// такую ссылку молча отказавшись — панель предупредит об этом админа.
		"self_signed": s.cfg.Web.TLS.Enabled && s.cfg.Web.TLS.CertFile == "",
	})
}

// ---------------------------------------------------------------------------
// Публичная ссылка на страницу с ключами
// ---------------------------------------------------------------------------

// handleAddDevice — POST /api/users/{username}/devices {device}: доп. AWG-конфиг
// для отдельного устройства пользователя (multi-device). Идемпотентно: повтор
// с тем же именем возвращает существующий конфиг.
func (s *Server) handleAddDevice(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	var req struct {
		Device string `json:"device"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "неверный запрос")
		return
	}
	acc, err := s.mgr.AddDevice(username, req.Device)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account": acc})
}

// handleRemoveDevice — DELETE /api/users/{username}/devices/{device}: удаляет
// устройство (учётку в панели + identity + историю трафика устройства).
func (s *Server) handleRemoveDevice(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.RemoveDevice(r.PathValue("username"), r.PathValue("device")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleShareCreate создаёт (или пересоздаёт) публичную ссылку пользователя.
func (s *Server) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	user, err := s.store.GetUserByName(r.PathValue("username"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "пользователь не найден")
		return
	}
	token, err := util.RandHex(16) // 32 hex-символа — не подобрать перебором
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.SetShareToken(user.ID, token); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("[web] создана публичная ссылка для %s", user.Username)
	writeJSON(w, http.StatusOK, map[string]string{"token": token, "path": "/p/" + token})
}

// handleShareRevoke отзывает публичную ссылку пользователя.
func (s *Server) handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	user, err := s.store.GetUserByName(r.PathValue("username"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "пользователь не найден")
		return
	}
	if err := s.store.DeleteShareToken(user.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("[web] отозвана публичная ссылка для %s", user.Username)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// publicHeaders — публичную страницу не кэшируем и прячем от поисковиков.
func publicHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// handlePublicPage отдаёт оболочку публичной страницы (данные тянет из /data).
func (s *Server) handlePublicPage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.GetUserByShareToken(r.PathValue("token")); err != nil {
		publicHeaders(w)
		http.Error(w, "Ссылка недействительна или отозвана", http.StatusNotFound)
		return
	}
	page, err := staticFS.ReadFile("static/public.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	publicHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

// handlePublicData отдаёт данные для публичной страницы: ключи по сервисам и
// трафик ЭТОГО пользователя. Ничего лишнего (ни id, ни других пользователей).
func (s *Server) handlePublicData(w http.ResponseWriter, r *http.Request) {
	publicHeaders(w)
	user, err := s.store.GetUserByShareToken(r.PathValue("token"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "ссылка недействительна или отозвана")
		return
	}
	ids, _ := s.store.GetIdentities(user.ID)

	traffic := map[string]interface{}{}
	for _, p := range []string{"today", "week", "month", "full"} {
		byService, _ := s.store.TrafficForUser(user.ID, periods[p])
		var total uint64
		svcs := make([]svcBytes, 0, len(byService))
		for svc, b := range byService {
			svcs = append(svcs, svcBytes{svc, b})
			total += b
		}
		traffic[p] = map[string]interface{}{"services": svcs, "total": total}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"display_name": user.DisplayName,
		"identities":   ids,
		"traffic":      traffic,
	})
}

// safeFilename оставляет только безопасные символы: имя уходит в заголовок
// Content-Disposition, где кавычки и переводы строк недопустимы.
func safeFilename(s string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, s)
	if out == "" {
		return "config.conf"
	}
	return out
}

// handlePublicFile отдаёт конфиг-файл настоящим HTTP-ответом с
// Content-Disposition: attachment. Через data:-URI это не работало — встроенные
// браузеры (Telegram) и iOS такие «скачивания» просто игнорируют, а обычную
// ссылку на файл ОС отдаёт приложению (AmneziaWG).
func (s *Server) handlePublicFile(w http.ResponseWriter, r *http.Request) {
	publicHeaders(w)
	user, err := s.store.GetUserByShareToken(r.PathValue("token"))
	if err != nil {
		http.Error(w, "ссылка недействительна или отозвана", http.StatusNotFound)
		return
	}
	svc := r.URL.Query().Get("svc")
	idx, err := strconv.Atoi(r.URL.Query().Get("i"))
	if err != nil || idx < 0 {
		http.Error(w, "неверный номер файла", http.StatusBadRequest)
		return
	}

	ids, _ := s.store.GetIdentities(user.ID)
	for _, id := range ids {
		if id.Service != svc {
			continue
		}
		var items []service.Item
		if json.Unmarshal([]byte(id.Config), &items) != nil || idx >= len(items) {
			break
		}
		it := items[idx]
		if it.File == "" { // отдаём только то, что задумано как файл
			break
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+safeFilename(it.File)+`"`)
		_, _ = w.Write([]byte(it.Value))
		return
	}
	http.Error(w, "файл не найден", http.StatusNotFound)
}

// handleRegenerate пересоздаёт учётку пользователя в одном сервисе (новые
// ключи/ссылки). Тело: {"service":"xray"}.
func (s *Server) handleRegenerate(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	var req struct {
		Service string `json:"service"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "неверный JSON")
		return
	}
	req.Service = strings.TrimSpace(req.Service)
	if req.Service == "" {
		writeErr(w, http.StatusBadRequest, "не указан сервис")
		return
	}
	acc, err := s.mgr.Regenerate(username, req.Service)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account": acc})
}

// handleRegenerateAll пересоздаёт учётки всех пользователей в одном сервисе.
// Тело: {"service":"amneziawg"}.
func (s *Server) handleRegenerateAll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Service string `json:"service"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "неверный JSON")
		return
	}
	req.Service = strings.TrimSpace(req.Service)
	if req.Service == "" {
		writeErr(w, http.StatusBadRequest, "не указан сервис")
		return
	}
	results, err := s.mgr.RegenerateAll(req.Service)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ok := 0
	for _, rr := range results {
		if rr.OK {
			ok++
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": results, "ok": ok, "total": len(results),
	})
}

// handleRenameUser меняет отображаемое имя пользователя. Логин остаётся прежним —
// он же identity в сервисах, менять его нельзя без перевыпуска конфигов.
func (s *Server) handleRenameUser(w http.ResponseWriter, r *http.Request) {
	user, err := s.store.GetUserByName(r.PathValue("username"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "пользователь не найден")
		return
	}
	var req struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "неверный JSON")
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" {
		name = user.Username // пустое имя = вернуть подпись к логину
	}
	if err := s.store.SetDisplayName(user.ID, name); err != nil {
		writeErr(w, http.StatusInternalServerError, "не удалось переименовать: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"display_name": name})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	results, err := s.mgr.Delete(username)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   err.Error(),
			"results": results,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

package manager

// connections.go — оркестрация «Подключений» (антиблокировка): ротация
// Reality-SNI/dest/fingerprint, смена домена-маски telemt, запасной транспорт
// XHTTP/gRPC. После каждого изменения перегенерируется nginx SNI-карта
// (пакет nginxmap) и обновляются сохранённые ссылки клиентов (RefreshLinks).

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/LankoRu/vps-admin/internal/nginxmap"
	"github.com/LankoRu/vps-admin/internal/service"
)

// RealityService — узкий контракт xray-адаптера для управления подключениями.
type RealityService interface {
	MainInboundID() int
	BackupPort() int
	Reality(inboundID int) (*service.RealitySettings, error)
	UpdateReality(inboundID int, rs *service.RealitySettings) error
	XrayInbounds() ([]service.InboundNet, error)
	EnsureBackup(transport string, port int, rs *service.RealitySettings) (int, error)
	FindBackup(transport string) (int, bool, error)
	SetEnabled(inboundID int, enabled bool) error
	Attach(email string, inboundID int) error
	Detach(email string, inboundID int) error
	BackupServerNames(transport string) ([]string, string, error)
}

// TelemtDomainService — контракт telemt-адаптера для смены домена-маски.
type TelemtDomainService interface {
	Domain() (string, error)
	SetDomain(domain string) error
}

// RealityPreset — проверенный набор Reality-доменов (пресет для ротации).
type RealityPreset struct {
	Name        string   `json:"name"`
	Dest        string   `json:"dest"`
	ServerNames []string `json:"server_names"`
}

// RealityPresets — популярные Reality-цели (TLS 1.3 + H2). Доступность из RU
// не гарантирована — перед применением	dest проверяется probe'ом с сервера.
var RealityPresets = []RealityPreset{
	{"Google", "www.google.com", []string{"www.google.com", "google.com", "mail.google.com", "drive.google.com", "docs.google.com", "play.google.com", "maps.google.com", "accounts.google.com", "dl.google.com", "www.googletagmanager.com"}},
	{"Microsoft", "www.microsoft.com", []string{"www.microsoft.com", "login.microsoftonline.com", "www.office.com", "outlook.office.com", "www.bing.com"}},
	{"Apple", "www.apple.com", []string{"www.apple.com", "itunes.apple.com", "swcdn.apple.com", "www.icloud.com", "support.apple.com"}},
	{"Amazon", "www.amazon.com", []string{"www.amazon.com", "aws.amazon.com", "www.media-amazon.com", "images-na.ssl-images-amazon.com"}},
	{"Samsung", "www.samsung.com", []string{"www.samsung.com", "semiconductor.samsung.com", "www.samsungsemicon.com"}},
}

// FingerprintOptions — допустимые uTLS-фингерпринты.
var FingerprintOptions = []string{"chrome", "firefox", "safari", "ios", "android", "edge", "random", "randomized"}

// BackupTransports — поддерживаемые запасные транспорты.
var BackupTransports = []string{"xhttp", "grpc"}

// Тестовые хуки (в тестах подменяются).
var (
	nginxMapPath = nginxmap.DefaultPath
	nginxReload  = nginxmap.DefaultReload
	probeReality = probeRealityDest
	probeMask    = probeMaskDomain
)

// BackupInfo — состояние запасного инбаунда.
type BackupInfo struct {
	Transport   string   `json:"transport"`
	ID          int      `json:"id"`
	Enabled     bool     `json:"enabled"`
	ServerNames []string `json:"server_names"`
	Dest        string   `json:"dest"`
}

// ConnectionsInfo — полное состояние раздела «Подключения».
type ConnectionsInfo struct {
	Reality      *service.RealitySettings `json:"reality"`
	InboundID    int                      `json:"inbound_id"`
	BackupPort   int                      `json:"backup_port"`
	TelemtDomain string                   `json:"telemt_domain"`
	Backups      []BackupInfo             `json:"backups"`
	Presets      []RealityPreset          `json:"presets"`
	Fingerprints []string                 `json:"fingerprints"`
	Transports   []string                 `json:"transports"`
	MapPath      string                   `json:"map_path"`
}

func (m *Manager) realitySvc() (RealityService, error) {
	p, ok := m.reg.Provisioners()["xray"]
	if !ok {
		return nil, fmt.Errorf("сервис xray не включён")
	}
	rs, ok := p.(RealityService)
	if !ok {
		return nil, fmt.Errorf("xray-адаптер не поддерживает управление подключениями")
	}
	return rs, nil
}

func (m *Manager) telemtSvc() (TelemtDomainService, error) {
	p, ok := m.reg.Provisioners()["telemt"]
	if !ok {
		return nil, fmt.Errorf("сервис telemt не включён")
	}
	ts, ok := p.(TelemtDomainService)
	if !ok {
		return nil, fmt.Errorf("telemt-адаптер не поддерживает смену домена")
	}
	return ts, nil
}

// ConnectionsState читает текущее состояние из сервисов (не из БД).
func (m *Manager) ConnectionsState() (*ConnectionsInfo, error) {
	info := &ConnectionsInfo{
		Presets: RealityPresets, Fingerprints: FingerprintOptions,
		Transports: BackupTransports, MapPath: nginxMapPath,
	}
	if rs, err := m.realitySvc(); err == nil {
		info.InboundID = rs.MainInboundID()
		info.BackupPort = rs.BackupPort()
		if r, err := rs.Reality(info.InboundID); err == nil {
			info.Reality = r
		} else {
			log.Printf("[connections] Reality не прочитан: %v", err)
		}
		for _, t := range BackupTransports {
			if id, en, err := rs.FindBackup(t); err == nil && id != 0 {
				names, dest, _ := rs.BackupServerNames(t)
				info.Backups = append(info.Backups, BackupInfo{Transport: t, ID: id, Enabled: en, ServerNames: names, Dest: dest})
			}
		}
	}
	if ts, err := m.telemtSvc(); err == nil {
		if d, err := ts.Domain(); err == nil {
			info.TelemtDomain = d
		} else {
			log.Printf("[connections] домен telemt не прочитан: %v", err)
		}
	}
	return info, nil
}

// rebuildMap перегенерирует nginx SNI-карту из ТЕКУЩЕГО состояния сервисов.
func (m *Manager) rebuildMap() error {
	rs, err := m.realitySvc()
	if err != nil {
		return err
	}
	inbounds, err := rs.XrayInbounds()
	if err != nil {
		return err
	}
	var nets []nginxmap.Inbound
	for _, ib := range inbounds {
		if !ib.Enabled || len(ib.ServerNames) == 0 {
			continue
		}
		nets = append(nets, nginxmap.Inbound{Port: ib.Port, ServerNames: ib.ServerNames})
	}
	domain := ""
	if ts, err := m.telemtSvc(); err == nil {
		domain, _ = ts.Domain()
	}
	if domain == "" {
		return fmt.Errorf("не удалось определить домен telemt для SNI-карты")
	}
	return nginxmap.Apply(nginxMapPath, nginxmap.State{TelemtDomain: domain, Inbounds: nets}, nginxReload)
}

var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func cleanNames(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range in {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || seen[n] {
			continue
		}
		if !hostnameRe.MatchString(n) {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// otherNames — SNI, занятые telemt-доменом и запасными инбаундами.
func (m *Manager) otherNames(rs RealityService, excludeTransport string) map[string]bool {
	used := map[string]bool{}
	if ts, err := m.telemtSvc(); err == nil {
		if d, err := ts.Domain(); err == nil {
			used[d] = true
		}
	}
	for _, t := range BackupTransports {
		if t == excludeTransport {
			continue
		}
		if names, _, err := rs.BackupServerNames(t); err == nil {
			for _, n := range names {
				used[n] = true
			}
		}
	}
	return used
}

func checkCollision(names []string, used map[string]bool) error {
	for _, n := range names {
		if used[n] {
			return fmt.Errorf("SNI %q уже занят (telemt-домен или другой инбаунд) — маршруты столкнутся", n)
		}
	}
	return nil
}

// UpdateReality — ротация SNI/dest/fingerprint основного инбаунда. Непустые
// аргументы заменяют текущие значения. Порядок: валидация → probe dest →
// панель → nginx-карта → обновление ссылок клиентов.
func (m *Manager) UpdateReality(serverNames []string, dest, fingerprint string) error {
	rs, err := m.realitySvc()
	if err != nil {
		return err
	}
	cur, err := rs.Reality(rs.MainInboundID())
	if err != nil {
		return err
	}
	next := *cur
	if len(serverNames) > 0 {
		next.ServerNames = cleanNames(serverNames)
	}
	if dest != "" {
		next.Dest = strings.ToLower(strings.TrimSpace(dest))
	}
	if fingerprint != "" {
		if !contains(FingerprintOptions, fingerprint) {
			return fmt.Errorf("неизвестный fingerprint %q (доступны: %s)", fingerprint, strings.Join(FingerprintOptions, ", "))
		}
		next.Fingerprint = fingerprint
	}
	if len(next.ServerNames) == 0 {
		return fmt.Errorf("пустой список SNI")
	}
	if !hostnameRe.MatchString(hostOf(next.Dest)) {
		return fmt.Errorf("некорректный dest %q", next.Dest)
	}
	if err := checkCollision(next.ServerNames, m.otherNames(rs, "")); err != nil {
		return err
	}
	if err := probeReality(hostOf(next.Dest)); err != nil {
		return fmt.Errorf("dest %s не прошёл проверку (нужен TLS 1.3 + H2): %w", next.Dest, err)
	}
	if err := rs.UpdateReality(rs.MainInboundID(), &next); err != nil {
		return err
	}
	if err := m.rebuildMap(); err != nil {
		return fmt.Errorf("Reality обновлён, но nginx-карта не применена: %w", err)
	}
	if u, f := m.RefreshLinks(); f > 0 {
		log.Printf("[connections] ссылки обновлены: %d, ошибок: %d", u, f)
	}
	log.Printf("[connections] Reality: dest=%s sni=%d fp=%s", next.Dest, len(next.ServerNames), next.Fingerprint)
	return nil
}

// SetTelemtDomain — смена домена-маски FakeTLS. ВНИМАНИЕ: все tg://-ссылки
// меняются (домен зашит в secret) — клиентам нужно раздать новые.
func (m *Manager) SetTelemtDomain(domain string) error {
	ts, err := m.telemtSvc()
	if err != nil {
		return err
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	if !hostnameRe.MatchString(domain) {
		return fmt.Errorf("некорректный домен %q", domain)
	}
	if rs, err := m.realitySvc(); err == nil {
		used := map[string]bool{}
		if cur, err := rs.Reality(rs.MainInboundID()); err == nil {
			for _, n := range cur.ServerNames {
				used[n] = true
			}
		}
		for _, t := range BackupTransports {
			if names, _, err := rs.BackupServerNames(t); err == nil {
				for _, n := range names {
					used[n] = true
				}
			}
		}
		if used[domain] {
			return fmt.Errorf("домен %q уже используется как Reality-SNI — маршруты столкнутся", domain)
		}
	}
	if err := probeMask(domain); err != nil {
		return fmt.Errorf("домен %s не прошёл проверку (нужен реальный HTTPS-сайт с валидным сертификатом): %w", domain, err)
	}
	if err := ts.SetDomain(domain); err != nil {
		return err
	}
	if err := m.rebuildMap(); err != nil {
		return fmt.Errorf("домен сменён, но nginx-карта не применена: %w", err)
	}
	if u, f := m.RefreshLinks(); f > 0 {
		log.Printf("[connections] ссылки обновлены: %d, ошибок: %d", u, f)
	}
	log.Printf("[connections] домен-маска telemt: %s", domain)
	return nil
}

// SetBackup — запасной транспорт (XHTTP/gRPC) на отдельном localhost-порту со
// СВОИМ набором SNI (один SNI не может вести в два инбаунда). Включение:
// создаёт инбаунд, прикрепляет всех xray-клиентов (ссылки пополнятся вторым
// транспортом), обновляет карту. Выключение: гасит инбаунд и открепляет.
func (m *Manager) SetBackup(enabled bool, transport string, serverNames []string, dest string) error {
	rs, err := m.realitySvc()
	if err != nil {
		return err
	}
	if transport == "" {
		transport = "xhttp"
	}
	if !contains(BackupTransports, transport) {
		return fmt.Errorf("неподдерживаемый транспорт %q (xhttp|grpc)", transport)
	}
	if enabled {
		names := cleanNames(serverNames)
		if len(names) == 0 {
			return fmt.Errorf("для запасного транспорта нужен свой список SNI (отличный от основного)")
		}
		dest = strings.ToLower(strings.TrimSpace(dest))
		if dest == "" {
			dest = names[0]
		}
		if !hostnameRe.MatchString(hostOf(dest)) {
			return fmt.Errorf("некорректный dest %q", dest)
		}
		used := m.otherNames(rs, transport)
		if cur, err := rs.Reality(rs.MainInboundID()); err == nil {
			for _, n := range cur.ServerNames {
				used[n] = true
			}
		}
		if err := checkCollision(names, used); err != nil {
			return err
		}
		if err := probeReality(hostOf(dest)); err != nil {
			return fmt.Errorf("dest %s не прошёл проверку (TLS 1.3 + H2): %w", dest, err)
		}
		fp := "chrome"
		if cur, err := rs.Reality(rs.MainInboundID()); err == nil && cur.Fingerprint != "" {
			fp = cur.Fingerprint
		}
		id, err := rs.EnsureBackup(transport, rs.BackupPort(), &service.RealitySettings{
			Dest: dest, ServerNames: names, Fingerprint: fp,
		})
		if err != nil {
			return err
		}
		ids, _ := m.store.IdentitiesByService("xray")
		for _, idn := range ids {
			if err := rs.Attach(idn.Identity, id); err != nil {
				log.Printf("[connections] attach %s к запасному инбаунду: %v", idn.Identity, err)
			}
		}
		log.Printf("[connections] запасной транспорт %s включён (инбаунд %d, SNI: %d)", transport, id, len(names))
	} else {
		id, _, err := rs.FindBackup(transport)
		if err != nil {
			return err
		}
		if id != 0 {
			if err := rs.SetEnabled(id, false); err != nil {
				return err
			}
			ids, _ := m.store.IdentitiesByService("xray")
			for _, idn := range ids {
				if err := rs.Detach(idn.Identity, id); err != nil {
					log.Printf("[connections] detach %s: %v", idn.Identity, err)
				}
			}
		}
		log.Printf("[connections] запасной транспорт %s выключен", transport)
	}
	if err := m.rebuildMap(); err != nil {
		return fmt.Errorf("транспорт изменён, но nginx-карта не применена: %w", err)
	}
	if u, f := m.RefreshLinks(); f > 0 {
		log.Printf("[connections] ссылки обновлены: %d, ошибок: %d", u, f)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// hostOf — хост из dest (панель хранит host:port, probe/валидация — по хосту).
func hostOf(dest string) string {
	if h, _, err := net.SplitHostPort(dest); err == nil {
		return h
	}
	return dest
}

// probeRetry — проба с повторами: на свежих VPS DNS/сеть первые секунды
// флапают, и одиночный dial даёт ложное «dest не прошёл проверку».
func probeRetry(fn func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return err
}

// probeRealityDest — Reality-цель обязана поддерживать TLS 1.3 + ALPN h2.
func probeRealityDest(host string) error {
	return probeRetry(func() error { return probeRealityOnce(host) })
}

func probeRealityOnce(host string) error {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp",
		net.JoinHostPort(host, "443"),
		&tls.Config{ServerName: host, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS13})
	if err != nil {
		return err
	}
	defer conn.Close()
	if conn.ConnectionState().NegotiatedProtocol != "h2" {
		return fmt.Errorf("нет ALPN h2")
	}
	return nil
}

// probeMaskDomain — домен-маска telemt: реальный HTTPS-сайт с валидным
// сертификатом (telemt релейит на него сканеры и отдаёт его сертификат).
func probeMaskDomain(host string) error {
	return probeRetry(func() error { return probeMaskOnce(host) })
}

func probeMaskOnce(host string) error {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp",
		net.JoinHostPort(host, "443"), &tls.Config{ServerName: host})
	if err != nil {
		return err
	}
	defer conn.Close()
	return nil
}

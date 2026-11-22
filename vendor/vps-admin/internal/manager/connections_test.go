package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LankoRu/vps-admin/internal/service"
)

type fakeReality struct {
	rs       service.RealitySettings
	updated  []service.RealitySettings
	inbounds []service.InboundNet
	backups  map[string]int
	enabled  map[string]bool
	attached []string
	detached []string
	ensured  []string
}

func (f *fakeReality) Provision(u string) (*service.Account, error) {
	return &service.Account{Service: "xray", Identity: u}, nil
}
func (f *fakeReality) Deprovision(string) error                      { return nil }
func (f *fakeReality) MainInboundID() int                            { return 1 }
func (f *fakeReality) BackupPort() int                               { return 10092 }
func (f *fakeReality) Reality(int) (*service.RealitySettings, error) { c := f.rs; return &c, nil }
func (f *fakeReality) UpdateReality(_ int, rs *service.RealitySettings) error {
	f.updated = append(f.updated, *rs)
	f.rs = *rs
	for i := range f.inbounds {
		if f.inbounds[i].ID == 1 {
			f.inbounds[i].ServerNames = rs.ServerNames
		}
	}
	return nil
}
func (f *fakeReality) XrayInbounds() ([]service.InboundNet, error) { return f.inbounds, nil }
func (f *fakeReality) EnsureBackup(t string, port int, rs *service.RealitySettings) (int, error) {
	f.ensured = append(f.ensured, t)
	if f.backups == nil {
		f.backups = map[string]int{}
		f.enabled = map[string]bool{}
	}
	f.backups[t] = 7
	f.enabled[t] = true
	f.inbounds = append(f.inbounds, service.InboundNet{ID: 7, Port: port, Protocol: "vless",
		Remark: "shvpn-backup-" + t, Enabled: true, ServerNames: rs.ServerNames})
	return 7, nil
}
func (f *fakeReality) FindBackup(t string) (int, bool, error) { return f.backups[t], f.enabled[t], nil }
func (f *fakeReality) SetEnabled(id int, e bool) error {
	for t, i := range f.backups {
		if i == id {
			f.enabled[t] = e
		}
	}
	for i := range f.inbounds {
		if f.inbounds[i].ID == id {
			f.inbounds[i].Enabled = e
		}
	}
	return nil
}
func (f *fakeReality) Attach(email string, _ int) error {
	f.attached = append(f.attached, email)
	return nil
}
func (f *fakeReality) Detach(email string, _ int) error {
	f.detached = append(f.detached, email)
	return nil
}
func (f *fakeReality) BackupServerNames(t string) ([]string, string, error) {
	if t == "grpc" {
		return []string{"www.samsung.com"}, "www.samsung.com", nil
	}
	return nil, "", nil
}

type fakeTelemt struct{ domain string }

func (f *fakeTelemt) Provision(u string) (*service.Account, error) {
	return &service.Account{Service: "telemt", Identity: u}, nil
}
func (f *fakeTelemt) Deprovision(string) error { return nil }
func (f *fakeTelemt) Domain() (string, error)  { return f.domain, nil }
func (f *fakeTelemt) SetDomain(d string) error { f.domain = d; return nil }

func newConnManager(t *testing.T, fr *fakeReality, ft *fakeTelemt) (*Manager, string) {
	t.Helper()
	m, _ := newTestManager(t, map[string]service.Provisioner{"xray": fr, "telemt": ft})
	mapPath := filepath.Join(t.TempDir(), "map.conf")
	op, or, opr, opm := nginxMapPath, nginxReload, probeReality, probeMask
	nginxMapPath = mapPath
	nginxReload = func() error { return nil }
	probeReality = func(string) error { return nil }
	probeMask = func(string) error { return nil }
	t.Cleanup(func() { nginxMapPath, nginxReload, probeReality, probeMask = op, or, opr, opm })
	return m, mapPath
}

func googleReality() *fakeReality {
	return &fakeReality{
		rs: service.RealitySettings{Dest: "www.google.com:443", ServerNames: []string{"www.google.com", "google.com"}, Fingerprint: "chrome"},
		inbounds: []service.InboundNet{
			{ID: 1, Port: 10090, Protocol: "vless", Enabled: true, ServerNames: []string{"www.google.com", "google.com"}},
		},
	}
}

func TestConnectionsState(t *testing.T) {
	fr, ft := googleReality(), &fakeTelemt{domain: "petrovich.ru"}
	m, _ := newConnManager(t, fr, ft)
	info, err := m.ConnectionsState()
	if err != nil {
		t.Fatalf("ConnectionsState: %v", err)
	}
	if info.Reality == nil || info.Reality.Dest != "www.google.com:443" || info.TelemtDomain != "petrovich.ru" {
		t.Errorf("info = %+v", info)
	}
	if len(info.Presets) == 0 || len(info.Fingerprints) == 0 {
		t.Error("пресеты/фингерпринты пусты")
	}
}

func TestUpdateRealityCollisionAndValidation(t *testing.T) {
	fr, ft := googleReality(), &fakeTelemt{domain: "petrovich.ru"}
	m, _ := newConnManager(t, fr, ft)
	// коллизия с доменом telemt
	if err := m.UpdateReality([]string{"petrovich.ru"}, "", ""); err == nil {
		t.Error("ожидалась ошибка коллизии с telemt-доменом")
	}
	// коллизия с grpc-backup
	fr.backups = map[string]int{"grpc": 7}
	if err := m.UpdateReality([]string{"www.samsung.com"}, "", ""); err == nil {
		t.Error("ожидалась ошибка коллизии с backup SNI")
	}
	// плохой fingerprint
	if err := m.UpdateReality(nil, "", "netscape"); err == nil {
		t.Error("ожидалась ошибка fingerprint")
	}
	if len(fr.updated) != 0 {
		t.Errorf("панель не должна была обновиться: %v", fr.updated)
	}
}

func TestUpdateRealitySuccessWritesMap(t *testing.T) {
	fr, ft := googleReality(), &fakeTelemt{domain: "petrovich.ru"}
	m, mapPath := newConnManager(t, fr, ft)
	if err := m.UpdateReality([]string{"www.apple.com", "itunes.apple.com"}, "www.apple.com", "firefox"); err != nil {
		t.Fatalf("UpdateReality: %v", err)
	}
	if fr.rs.Fingerprint != "firefox" || len(fr.updated) != 1 {
		t.Errorf("rs = %+v", fr.rs)
	}
	b, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatalf("карта не записана: %v", err)
	}
	if !strings.Contains(string(b), "www.apple.com xray_10090;") || !strings.Contains(string(b), "petrovich.ru telemt;") {
		t.Errorf("карта:\n%s", b)
	}
}

func TestUpdateRealityProbeFailure(t *testing.T) {
	fr, ft := googleReality(), &fakeTelemt{domain: "petrovich.ru"}
	m, _ := newConnManager(t, fr, ft)
	probeReality = func(string) error { return os.ErrPermission }
	if err := m.UpdateReality([]string{"www.apple.com"}, "www.apple.com", ""); err == nil {
		t.Error("ожидалась ошибка probe")
	}
	if len(fr.updated) != 0 {
		t.Error("обновление не должно было пройти при плохом probe")
	}
}

func TestSetTelemtDomain(t *testing.T) {
	fr, ft := googleReality(), &fakeTelemt{domain: "petrovich.ru"}
	m, mapPath := newConnManager(t, fr, ft)
	// коллизия с Reality-SNI
	if err := m.SetTelemtDomain("www.google.com"); err == nil {
		t.Error("ожидалась ошибка коллизии")
	}
	// некорректный домен
	if err := m.SetTelemtDomain("not a domain"); err == nil {
		t.Error("ожидалась ошибка формата")
	}
	if err := m.SetTelemtDomain("example.com"); err != nil {
		t.Fatalf("SetTelemtDomain: %v", err)
	}
	if ft.domain != "example.com" {
		t.Errorf("домен = %q", ft.domain)
	}
	b, _ := os.ReadFile(mapPath)
	if !strings.Contains(string(b), "example.com telemt;") {
		t.Errorf("карта:\n%s", b)
	}
}

func TestSetBackupEnableDisable(t *testing.T) {
	fr, ft := googleReality(), &fakeTelemt{domain: "petrovich.ru"}
	m, mapPath := newConnManager(t, fr, ft)
	// юзер с xray-учёткой
	uid, _ := m.store.CreateUser("alice", "Alice", 0)
	_ = m.store.AddIdentity(uid, "xray", "alice", "", "[]")

	if err := m.SetBackup(true, "xhttp", []string{"www.microsoft.com"}, ""); err != nil {
		t.Fatalf("SetBackup enable: %v", err)
	}
	if len(fr.ensured) != 1 || fr.ensured[0] != "xhttp" {
		t.Errorf("ensured = %v", fr.ensured)
	}
	if len(fr.attached) != 1 || fr.attached[0] != "alice" {
		t.Errorf("attached = %v", fr.attached)
	}
	b, _ := os.ReadFile(mapPath)
	if !strings.Contains(string(b), "www.microsoft.com xray_10092;") {
		t.Errorf("карта без backup:\n%s", b)
	}
	// коллизия с основным SNI
	if err := m.SetBackup(true, "grpc", []string{"www.google.com"}, ""); err == nil {
		t.Error("ожидалась ошибка коллизии backup с основным SNI")
	}
	// выключение
	if err := m.SetBackup(false, "xhttp", nil, ""); err != nil {
		t.Fatalf("SetBackup disable: %v", err)
	}
	if fr.enabled["xhttp"] {
		t.Error("backup не выключен")
	}
	if len(fr.detached) != 1 || fr.detached[0] != "alice" {
		t.Errorf("detached = %v", fr.detached)
	}
}

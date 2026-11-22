// Package awg3xui — AmneziaWG через инбаунд панели 3x-ui (userspace netstack
// или kernel mode). Клиенты создаются через API панели, она сама генерирует
// ключи, выделяет IP и хранит параметры обфускации (AWG 2.0/3.1, включая
// HeaderProtectionKey).
//
// Конфиги подключения забираем у панели готовыми: GET /panel/api/clients/links
// для AmneziaWG отдаёт vpn://<base64url текста .conf> (схема самого приложения
// Amnezia VPN). Декодируем, правим localhost в Endpoint на публичный хост и
// отдаём два варианта: vpn:// (Amnezia VPN) и .conf (AmneziaWG).
//
// Имя сервиса каноническое — "amneziawg": бот, веб-панель, manager и отчёты
// не отличают этот бэкенд от нативного.
package awg3xui

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/service"
	"github.com/LankoRu/vps-admin/internal/util"
)

const serviceName = "amneziawg"

// browserUA — панели за nginx/Cloudflare отвечают 403 на не-браузерный
// User-Agent; прикидываемся браузером (как в xray-адаптере).
const browserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

type Service struct {
	cfg    *config.AWG3xUIConfig
	client *http.Client
}

// uaTransport проставляет браузерный User-Agent и токен API (Bearer).
type uaTransport struct {
	base  http.RoundTripper
	token string
}

func (t uaTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("User-Agent") == "" {
		r.Header.Set("User-Agent", browserUA)
	}
	if r.Header.Get("Accept") == "" {
		r.Header.Set("Accept", "application/json, text/plain, */*")
	}
	if t.token != "" && r.Header.Get("Authorization") == "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(r)
}

func New(cfg *config.Config) *Service {
	jar, _ := cookiejar.New(nil)
	return &Service{
		cfg: &cfg.AmneziaWG3xUI,
		client: &http.Client{
			Transport: uaTransport{
				base:  &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
				token: cfg.AmneziaWG3xUI.APIToken,
			},
			Jar:     jar,
			Timeout: 30 * time.Second,
		},
	}
}

func (s *Service) Name() string { return serviceName }

func (s *Service) auth() error {
	if s.cfg.APIToken == "" {
		return fmt.Errorf("не задан api_token (3x-ui: Settings → Security → API Token)")
	}
	return nil
}

// buildURL собирает URL с учётом кастомного web_base_path.
func (s *Service) buildURL(endpoint string) string {
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	path := strings.Trim(s.cfg.WebBasePath, "/")
	if path == "" {
		return base + endpoint
	}
	return base + "/" + path + "/" + strings.TrimLeft(endpoint, "/")
}

func statusErr(what string, status int, body []byte) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("%s: status %d — проверьте api_token (Settings → Security) "+
			"и base_url/web_base_path", what, status)
	}
	return fmt.Errorf("%s: status %d: %s", what, status, strings.TrimSpace(string(body)))
}

// GetTraffic — трафик по email клиентов НАШЕГО AmneziaWG-инбаунда.
// Берём только inbound_id из конфига: в панели могут быть и xray-инбаунды,
// их трафик считает xray-коллектор (иначе задвоение на пользователя).
func (s *Service) GetTraffic() (map[string]uint64, error) {
	if err := s.auth(); err != nil {
		return nil, fmt.Errorf("amneziawg: %w", err)
	}
	resp, err := s.client.Get(s.buildURL("/panel/api/inbounds/list"))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("inbounds/list", resp.StatusCode, rb)
	}
	var r struct {
		Obj []struct {
			ID          int `json:"id"`
			ClientStats []struct {
				Email string `json:"email"`
				Up    uint64 `json:"up"`
				Down  uint64 `json:"down"`
			} `json:"clientStats"`
		} `json:"obj"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return nil, fmt.Errorf("decode inbounds: %w", err)
	}
	traffic := make(map[string]uint64)
	for _, in := range r.Obj {
		if in.ID != s.cfg.InboundID {
			continue
		}
		for _, st := range in.ClientStats {
			if st.Email != "" {
				traffic[st.Email] = st.Up + st.Down
			}
		}
	}
	log.Printf("[amneziawg] собрана статистика по %d клиентам (3x-ui inbound %d)", len(traffic), s.cfg.InboundID)
	return traffic, nil
}

// postAPI отправляет POST к API панели и разбирает конверт {success,msg}.
func (s *Service) postAPI(endpoint string, body []byte) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodPost, s.buildURL(endpoint), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return rb, statusErr(endpoint, resp.StatusCode, rb)
	}
	var r struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
	}
	_ = json.Unmarshal(rb, &r)
	if !r.Success {
		return rb, fmt.Errorf("%s отклонён панелью: %s", endpoint, strings.TrimSpace(r.Msg))
	}
	return rb, nil
}

// emailSuffix — AWG-клиент создаётся ОТДЕЛЬНОЙ учёткой от xray-клиента:
// в 3x-ui трафик хранится одной строкой на email (client_traffics), и общая
// учётка на двух инбаундах давала бы задвоение трафика в обоих коллекторах.
// Разные email → разные строки → честный per-service учёт. Identity в
// vps-admin = этот email (username+"-awg"), канонический юзер тот же.
const emailSuffix = "-awg"

func awgEmail(username string) string { return username + emailSuffix }

// Provision создаёт отдельного клиента <username>-awg в AmneziaWG-инбаунде.
// Ключи, IP и обфускацию генерирует сама панель. Повторный вызов идемпотентен:
// если клиент уже есть и прикреплён к нашему инбаунду — возвращаем его конфиги.
func (s *Service) Provision(username string) (*service.Account, error) {
	return s.provisionEmail(awgEmail(username))
}

// ProvisionDevice — доп. AWG-клиент для отдельного устройства пользователя
// (multi-device): email `<username>-awg-<device>`. Один WireGuard-ключ нельзя
// использовать параллельно с двух устройств (roaming + один туннельный IP),
// поэтому на каждое устройство — свой пир. Трафик всех устройств пользователя
// агрегируется в vps-admin по user_id (несколько identities на юзера).
func (s *Service) ProvisionDevice(username, device string) (*service.Account, error) {
	return s.provisionEmail(username + "-awg-" + sanitize(device))
}

func (s *Service) provisionEmail(email string) (*service.Account, error) {
	if s.cfg.InboundID == 0 {
		return nil, fmt.Errorf("amneziawg: не задан inbound_id в секции amneziawg_3xui")
	}
	if err := s.auth(); err != nil {
		return nil, fmt.Errorf("amneziawg: %w", err)
	}

	exists, err := s.clientExists(email)
	if err != nil {
		return nil, err
	}
	if exists {
		own, err := s.attachedToOwnInbound(email)
		if err != nil {
			return nil, err
		}
		if !own {
			return nil, fmt.Errorf("amneziawg: email %s занят другой учёткой (не в inbound %d)", email, s.cfg.InboundID)
		}
		log.Printf("[amneziawg] клиент уже существует: %s (inbound %d)", email, s.cfg.InboundID)
	} else {
		subID, err := util.RandHex(8)
		if err != nil {
			return nil, err
		}
		body, _ := json.Marshal(map[string]interface{}{
			"client": map[string]interface{}{
				"email": email, "enable": true, "subId": subID,
				"tgId": 0, "limitIp": 0, "totalGB": 0,
				"expiryTime": 0, "reset": 0, "comment": "",
			},
			"inboundIds": []int{s.cfg.InboundID},
		})
		if _, err := s.postAPI("/panel/api/clients/add", body); err != nil {
			return nil, err
		}
		log.Printf("[amneziawg] клиент создан: %s (3x-ui inbound %d)", email, s.cfg.InboundID)
	}

	addr, _ := s.clientAddress(email)
	items, err := s.itemsFromLinks(email)
	if err != nil {
		// Клиент создан — конфиги подтянет RefreshItems (как в xray-адаптере).
		log.Printf("[amneziawg] конфиги для %s не получены: %v", email, err)
	}
	return &service.Account{
		Service:  serviceName,
		Identity: email,
		Address:  addr,
		Items:    items,
	}, nil
}

// attachedToOwnInbound — состоит ли клиент в нашем AWG-инбаунде (по settings
// инбаундов из inbounds/list).
func (s *Service) attachedToOwnInbound(email string) (bool, error) {
	resp, err := s.client.Get(s.buildURL("/panel/api/inbounds/list"))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false, statusErr("inbounds/list", resp.StatusCode, rb)
	}
	var r struct {
		Obj []struct {
			ID       int             `json:"id"`
			Settings json.RawMessage `json:"settings"`
		} `json:"obj"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return false, fmt.Errorf("decode inbounds: %w", err)
	}
	for _, in := range r.Obj {
		if in.ID != s.cfg.InboundID {
			continue
		}
		var st struct {
			Clients []struct {
				Email string `json:"email"`
			} `json:"clients"`
		}
		raw := bytes.TrimSpace(in.Settings)
		if len(raw) > 0 && raw[0] == '"' {
			var str string
			if json.Unmarshal(raw, &str) == nil {
				raw = []byte(str)
			}
		}
		if json.Unmarshal(raw, &st) != nil {
			continue
		}
		for _, c := range st.Clients {
			if c.Email == email {
				return true, nil
			}
		}
	}
	return false, nil
}

// RefreshItems перечитывает конфиги существующего клиента (ключи/IP прежние).
// Нужно при смене public_host/порта или параметров обфускации в панели.
func (s *Service) RefreshItems(identity string) ([]service.Item, error) {
	if err := s.auth(); err != nil {
		return nil, fmt.Errorf("amneziawg: %w", err)
	}
	if _, err := s.getClient(identity); err != nil {
		return nil, err
	}
	items, err := s.itemsFromLinks(identity)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("панель не вернула конфигов для %s", identity)
	}
	return items, nil
}

// Deprovision удаляет AWG-клиента (<username>-awg) целиком: это отдельная
// учётка, не связанная с xray-клиентом, поэтому del безопасен. «Не найден»
// считаем успехом (идемпотентность).
func (s *Service) Deprovision(identity string) error {
	if err := s.auth(); err != nil {
		return fmt.Errorf("amneziawg: %w", err)
	}
	if _, err := s.postAPI("/panel/api/clients/del/"+url.PathEscape(identity), nil); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			log.Printf("[amneziawg] клиент %s не найден (уже удалён)", identity)
			return nil
		}
		return err
	}
	log.Printf("[amneziawg] клиент удалён: %s", identity)
	return nil
}

// clientExists проверяет, есть ли уже учётка с таким email в панели
// (глобально, по всем инбаундам). «not found» — не ошибка, а отсутствие.
func (s *Service) clientExists(email string) (bool, error) {
	resp, err := s.client.Get(s.buildURL("/panel/api/clients/get/" + url.PathEscape(email)))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false, statusErr("clients/get", resp.StatusCode, rb)
	}
	var r struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return false, fmt.Errorf("clients/get: %w", err)
	}
	if r.Success {
		return true, nil
	}
	if strings.Contains(strings.ToLower(r.Msg), "not found") {
		return false, nil
	}
	return false, fmt.Errorf("clients/get %s: %s", email, strings.TrimSpace(r.Msg))
}

// panelClient — поля клиента из GET /panel/api/clients/get/:email.
// allowedIPs панель отдаёт то строкой ("10.8.1.2/32"), то массивом —
// принимаем оба варианта.
type panelClient struct {
	AllowedIPs flexStrings `json:"allowedIPs"`
}

type flexStrings []string

func (f *flexStrings) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if strings.TrimSpace(s) == "" {
			*f = nil
			return nil
		}
		var out []string
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		*f = out
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*f = arr
	return nil
}

func (s *Service) getClient(email string) (*panelClient, error) {
	resp, err := s.client.Get(s.buildURL("/panel/api/clients/get/" + url.PathEscape(email)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("clients/get", resp.StatusCode, rb)
	}
	var r struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
		Obj     struct {
			Client panelClient `json:"client"`
		} `json:"obj"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return nil, fmt.Errorf("clients/get: %w", err)
	}
	if !r.Success {
		return nil, fmt.Errorf("клиент %s не найден в панели: %s", email, strings.TrimSpace(r.Msg))
	}
	return &r.Obj.Client, nil
}

// clientAddress — выданный туннельный IP (без маски) для Account.Address.
func (s *Service) clientAddress(email string) (string, error) {
	c, err := s.getClient(email)
	if err != nil {
		return "", err
	}
	if len(c.AllowedIPs) == 0 {
		return "", nil
	}
	ip := c.AllowedIPs[0]
	if i := strings.IndexByte(ip, '/'); i >= 0 {
		ip = ip[:i]
	}
	return ip, nil
}

// clientLinks запрашивает готовые vpn://-ссылки у панели.
func (s *Service) clientLinks(email string) ([]string, error) {
	resp, err := s.client.Get(s.buildURL("/panel/api/clients/links/" + url.PathEscape(email)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("clients/links", resp.StatusCode, rb)
	}
	var r struct {
		Obj []string `json:"obj"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return nil, fmt.Errorf("clients/links: %w", err)
	}
	return r.Obj, nil
}

// itemsFromLinks превращает vpn://-ссылки панели в варианты подключения:
// vpn:// (Amnezia VPN) + полный .conf (AmneziaWG). Endpoint правим на
// публичный хост (панель подставляет localhost из Host-заголовка запроса).
func (s *Service) itemsFromLinks(username string) ([]service.Item, error) {
	links, err := s.clientLinks(username)
	if err != nil {
		return nil, err
	}
	host := strings.TrimSpace(s.cfg.PublicHost)
	// панель отдаёт ссылки ВСЕХ инбаундов клиента (vless + vpn://) — берём только vpn://
	var vpnLinks []string
	for _, l := range links {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "vpn://") {
			vpnLinks = append(vpnLinks, l)
		}
	}
	var items []service.Item
	for n, l := range vpnLinks {
		conf, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(l, "vpn://"))
		if err != nil {
			log.Printf("[amneziawg] не декодировалась vpn://-ссылка %s: %v", username, err)
			continue
		}
		text := patchEndpointHost(string(conf), host)
		label := "vpn:// (Amnezia VPN)"
		confLabel := "Конфиг (AmneziaWG)"
		if len(vpnLinks) > 1 {
			label += fmt.Sprintf(" #%d", n+1)
			confLabel += fmt.Sprintf(" #%d", n+1)
		}
		items = append(items,
			service.Item{
				Label: label,
				Value: "vpn://" + base64.RawURLEncoding.EncodeToString([]byte(text)),
				QR:    true,
				File:  sanitize(username) + ".vpnuri",
			},
			service.Item{
				Label: confLabel, Value: text, QR: true,
				File: sanitize(username) + "-amneziawg.conf",
			})
	}
	return items, nil
}

// patchEndpointHost заменяет localhost в строке "Endpoint = host:port" на
// публичный хост. Порт и все остальные байты конфига не трогаем (параметры
// обфускации должны остаться байт-в-байт как сгенерила панель).
func patchEndpointHost(conf, host string) string {
	if host == "" {
		return conf
	}
	lines := strings.Split(conf, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Endpoint") {
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq < 0 {
			continue
		}
		value := strings.TrimSpace(trimmed[eq+1:])
		// отделяем :port (IPv6 — в квадратных скобках)
		hostPart := value
		if strings.HasPrefix(value, "[") {
			if c := strings.Index(value, "]"); c >= 0 {
				hostPart = value[:c+1]
			}
		} else if c := strings.LastIndex(value, ":"); c >= 0 {
			hostPart = value[:c]
		}
		switch strings.Trim(hostPart, "[]") {
		case "localhost", "127.0.0.1", "0.0.0.0", "::1":
		default:
			continue // адрес уже нормальный
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		lines[i] = indent + "Endpoint = " + host + value[len(hostPart):]
	}
	return strings.Join(lines, "\n")
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

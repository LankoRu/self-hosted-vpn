// Package xray — сбор трафика и создание клиентов через панель 3x-ui.
package xray

import (
	"bytes"
	"crypto/tls"
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

const serviceName = "xray"

// browserUA — многие панели за nginx/Cloudflare отвечают 403 на не-браузерный
// User-Agent (по умолчанию Go шлёт "Go-http-client/1.1"). Прикидываемся браузером.
const browserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

type Service struct {
	cfg    *config.Xray3xUIConfig
	client *http.Client
}

// uaTransport проставляет браузерный User-Agent и (если задан) токен API.
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
		cfg: &cfg.Xray3xUI,
		client: &http.Client{
			Transport: uaTransport{
				base:  &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
				token: cfg.Xray3xUI.APIToken,
			},
			Jar:     jar,
			Timeout: 30 * time.Second,
		},
	}
}

// auth проверяет, что задан API-токен (единственный способ авторизации).
// Сам токен подставляется в заголовок Authorization на каждый запрос (uaTransport).
func (s *Service) auth() error {
	if s.cfg.APIToken == "" {
		return fmt.Errorf("не задан api_token (3x-ui: Settings → Security → API Token)")
	}
	return nil
}

func (s *Service) Name() string { return serviceName }

// buildURL собирает URL с учётом кастомного web_base_path.
func (s *Service) buildURL(endpoint string) string {
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	path := strings.Trim(s.cfg.WebBasePath, "/")
	if path == "" {
		return base + endpoint
	}
	return base + "/" + path + "/" + strings.TrimLeft(endpoint, "/")
}

// transportHint поясняет частую ошибку с неверным портом (HTTP/2 gRPC-API).
func transportHint(err error) error {
	if err != nil && strings.Contains(err.Error(), "malformed HTTP response") {
		return fmt.Errorf("%w — base_url указывает на HTTP/2-порт (gRPC-API xray), "+
			"а не на веб-панель 3x-ui; укажите адрес панели из браузера", err)
	}
	return err
}

// statusErr поясняет ответы, типичные для проблем с токеном/адресом.
func statusErr(what string, status int, body []byte) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("%s: status %d — проверьте api_token (Settings → Security) "+
			"и base_url/web_base_path", what, status)
	}
	return fmt.Errorf("%s: status %d: %s", what, status, strings.TrimSpace(string(body)))
}

// GetTraffic возвращает трафик по email каждого клиента.
func (s *Service) GetTraffic() (map[string]uint64, error) {
	if err := s.auth(); err != nil {
		return nil, fmt.Errorf("xray: %w", err)
	}

	resp, err := s.client.Get(s.buildURL("/panel/api/inbounds/list"))
	if err != nil {
		return nil, transportHint(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, statusErr("inbounds/list", resp.StatusCode, body)
	}

	var r struct {
		Obj []struct {
			Protocol    string `json:"protocol"`
			ClientStats []struct {
				Email string `json:"email"`
				Up    uint64 `json:"up"`
				Down  uint64 `json:"down"`
			} `json:"clientStats"`
		} `json:"obj"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode inbounds: %w", err)
	}

	traffic := make(map[string]uint64)
	for _, inbound := range r.Obj {
		// Туннельные инбаунды (AmneziaWG/WireGuard) не считаем: их трафик
		// собирает отдельный коллектор amneziawg (иначе задвоение на юзера).
		if inbound.Protocol == "amneziawg" || inbound.Protocol == "wireguard" {
			continue
		}
		for _, st := range inbound.ClientStats {
			if st.Email != "" {
				traffic[st.Email] = st.Up + st.Down
			}
		}
	}
	log.Printf("[xray] собрана статистика по %d клиентам", len(traffic))
	return traffic, nil
}

// postAPI отправляет POST к API-панели и разбирает конверт {success,msg}.
func (s *Service) postAPI(endpoint string, body []byte) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodPost, s.buildURL(endpoint), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, transportHint(err)
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

// Provision добавляет клиента с email=username в настроенный inbound.
// 3x-ui v3: клиент создаётся через POST /panel/api/clients/add.
func (s *Service) Provision(username string) (*service.Account, error) {
	if s.cfg.InboundID == 0 {
		return nil, fmt.Errorf("xray: не задан inbound_id в конфиге")
	}
	if err := s.auth(); err != nil {
		return nil, fmt.Errorf("xray: %w", err)
	}

	// flow не задаём в конфиге — определяем по самому inbound.
	flow, err := s.inboundFlow(s.cfg.InboundID)
	if err != nil {
		return nil, err
	}

	uuid, err := util.UUIDv4()
	if err != nil {
		return nil, err
	}
	subID, err := util.RandHex(8)
	if err != nil {
		return nil, err
	}

	body, _ := json.Marshal(map[string]interface{}{
		"client": map[string]interface{}{
			"id": uuid, "email": username, "flow": flow, "enable": true,
			"subId": subID, "tgId": 0, "limitIp": 0, "totalGB": 0,
			"expiryTime": 0, "reset": 0, "comment": "",
		},
		"inboundIds": []int{s.cfg.InboundID},
	})

	if _, err := s.postAPI("/panel/api/clients/add", body); err != nil {
		return nil, err
	}
	log.Printf("[xray] клиент создан: %s (inbound %d)", username, s.cfg.InboundID)

	// Ссылки не получены — не повод откатывать созданного клиента: останутся
	// подписка/UUID, а RefreshItems подтянет ссылки позже.
	links, err := s.clientLinks(username)
	if err != nil {
		log.Printf("[xray] ссылки для %s не получены: %v", username, err)
	}
	return &service.Account{
		Service:  serviceName,
		Identity: username,
		Items:    s.clientItems(links, username, uuid, subID),
	}, nil
}

// RefreshItems перечитывает ссылки существующего клиента у панели — клиента не
// трогает (UUID и subId прежние). Нужно, когда в 3x-ui поменяли хосты/порты:
// сохранённые при создании ссылки иначе так и указывали бы на старый адрес.
func (s *Service) RefreshItems(email string) ([]service.Item, error) {
	if err := s.auth(); err != nil {
		return nil, fmt.Errorf("xray: %w", err)
	}
	c, err := s.getClient(email)
	if err != nil {
		return nil, err
	}
	links, err := s.clientLinks(email)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("панель не вернула ссылок для %s", email)
	}
	return s.clientItems(links, email, c.UUID, c.SubID), nil
}

// panelClient — нужные нам поля клиента из GET /panel/api/clients/get/:email.
type panelClient struct {
	UUID  string `json:"uuid"`
	SubID string `json:"subId"`
}

func (s *Service) getClient(email string) (*panelClient, error) {
	resp, err := s.client.Get(s.buildURL("/panel/api/clients/get/" + url.PathEscape(email)))
	if err != nil {
		return nil, transportHint(err)
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

// clientItems собирает варианты подключения: готовые ссылки панели (vless),
// ссылку подписки, иначе — UUID/email.
func (s *Service) clientItems(links []string, email, uuid, subID string) []service.Item {
	var items []service.Item
	for _, l := range links {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		items = append(items, service.Item{Label: "Ссылка (vless)", Value: l, QR: true})
	}
	if s.cfg.SubURL != "" {
		items = append(items, service.Item{
			Label: "Подписка",
			Value: strings.TrimRight(s.cfg.SubURL, "/") + "/" + subID,
			QR:    true,
		})
	}
	if len(items) == 0 {
		items = append(items, service.Item{Label: "UUID/email", Value: fmt.Sprintf("UUID: %s\nemail: %s", uuid, email)})
	}
	return items
}

// clientLinks запрашивает готовые ссылки подключения у панели (v3).
//
// 3x-ui строит адрес в ссылке из Host-заголовка запроса (resolveHost). Панель мы
// дёргаем по localhost, поэтому в ссылке оказывается localhost. Сам запрос НЕ
// трогаем (подмена Host ломает маршрутизацию за nginx и панель отдаёт non-200) —
// вместо этого правим localhost в уже полученных ссылках.
func (s *Service) clientLinks(email string) ([]string, error) {
	resp, err := s.client.Get(s.buildURL("/panel/api/clients/links/" + url.PathEscape(email)))
	if err != nil {
		return nil, transportHint(err)
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
	if host := s.publicHost(); host != "" {
		for i, l := range r.Obj {
			r.Obj[i] = rewriteLinkHost(l, host)
		}
	}
	return r.Obj, nil
}

// publicHost — публичный адрес для vless-ссылок: из public_host, иначе хост из
// sub_url. Пусто → не подменяем (поведение как раньше).
func (s *Service) publicHost() string {
	if h := strings.TrimSpace(s.cfg.PublicHost); h != "" {
		return h
	}
	if s.cfg.SubURL != "" {
		if u, err := url.Parse(s.cfg.SubURL); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return ""
}

// rewriteLinkHost заменяет localhost-адрес в ссылке на публичный хост.
//
// Меняем ТОЛЬКО байты хоста, а не пересобираем URL через url.Parse+String():
// тот перекодирует `#remark` (кириллица и пробелы превращаются в %D0%98%20…) и
// вообще нормализует ссылку, а клиентам нужен байт-в-байт тот вид, что выдала
// панель. Порт, query и remark остаются нетронутыми.
func rewriteLinkHost(link, host string) string {
	const sep = "://"
	i := strings.Index(link, sep)
	if i < 0 {
		return link
	}
	start := i + len(sep)

	// authority — до первого '/', '?' или '#'.
	end := len(link)
	for _, c := range []string{"/", "?", "#"} {
		if j := strings.Index(link[start:], c); j >= 0 && start+j < end {
			end = start + j
		}
	}
	// Отбрасываем userinfo (uuid@…).
	hostStart := start
	if at := strings.LastIndex(link[start:end], "@"); at >= 0 {
		hostStart = start + at + 1
	}

	// Отделяем :port (у IPv6 хост в квадратных скобках).
	hostPart := link[hostStart:end]
	hostOnly := hostPart
	if strings.HasPrefix(hostPart, "[") {
		if c := strings.Index(hostPart, "]"); c >= 0 {
			hostOnly = hostPart[:c+1]
		}
	} else if c := strings.LastIndex(hostPart, ":"); c >= 0 {
		hostOnly = hostPart[:c]
	}

	switch strings.Trim(hostOnly, "[]") {
	case "localhost", "127.0.0.1", "0.0.0.0", "::1":
	default:
		return link // адрес уже нормальный
	}
	return link[:hostStart] + host + link[hostStart+len(hostOnly):]
}

// inboundRaw — inbound в «сыром» виде. settings/streamSettings в разных
// версиях 3x-ui приходят то JSON-строкой, то уже объектом — поэтому RawMessage.
type inboundRaw struct {
	ID             int             `json:"id"`
	Settings       json.RawMessage `json:"settings"`
	StreamSettings json.RawMessage `json:"streamSettings"`
}

// jsonBytes возвращает JSON-содержимое поля, которое может быть либо строкой с
// вложенным JSON ("{...}"), либо объектом ({...}).
func jsonBytes(raw json.RawMessage) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return []byte(s)
		}
	}
	return raw
}

// listInbounds возвращает inbounds панели (требует предварительного login).
func (s *Service) listInbounds() ([]inboundRaw, error) {
	resp, err := s.client.Get(s.buildURL("/panel/api/inbounds/list"))
	if err != nil {
		return nil, transportHint(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, statusErr("inbounds/list", resp.StatusCode, body)
	}
	var r struct {
		Obj []inboundRaw `json:"obj"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode inbounds: %w", err)
	}
	return r.Obj, nil
}

// inboundFlow определяет flow для нового клиента по самому inbound:
// берёт flow существующего клиента, а если клиентов нет — выводит из
// security (reality/tls → xtls-rprx-vision).
func (s *Service) inboundFlow(inboundID int) (string, error) {
	inbounds, err := s.listInbounds()
	if err != nil {
		return "", err
	}
	for _, in := range inbounds {
		if in.ID != inboundID {
			continue
		}
		var st struct {
			Clients []struct {
				Flow string `json:"flow"`
			} `json:"clients"`
		}
		if json.Unmarshal(jsonBytes(in.Settings), &st) == nil {
			for _, c := range st.Clients {
				if c.Flow != "" {
					return c.Flow, nil
				}
			}
		}
		var ss struct {
			Security string `json:"security"`
		}
		if json.Unmarshal(jsonBytes(in.StreamSettings), &ss) == nil {
			if ss.Security == "reality" || ss.Security == "tls" {
				return "xtls-rprx-vision", nil
			}
		}
		return "", nil // inbound без vision-flow (напр. обычный vmess/vless)
	}
	return "", fmt.Errorf("inbound с id=%d не найден", inboundID)
}

// Deprovision удаляет клиента по email (3x-ui v3: POST /panel/api/clients/del/:email).
func (s *Service) Deprovision(identity string) error {
	if err := s.auth(); err != nil {
		return fmt.Errorf("xray: %w", err)
	}
	if _, err := s.postAPI("/panel/api/clients/del/"+identity, nil); err != nil {
		return err
	}
	log.Printf("[xray] клиент удалён: %s", identity)
	return nil
}

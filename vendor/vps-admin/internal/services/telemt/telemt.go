// Package telemt — сбор трафика и провижининг пользователей MTProto-прокси
// telemt (https://github.com/telemt/telemt) через его control-API.
//
// Пользователи telemt живут в его config.toml (секция [access.users]).
// Эндпоинт POST /v1/users создаёт пользователя и ПИШЕТ его в config.toml
// (persistent), а в ответе возвращает готовые tg://-ссылки (dd и ee),
// сгенерированные самим telemt из его tls_domain / public_host / port.
// Поэтому хост, порт и домен FakeTLS здесь не настраиваются — только адрес
// API и токен.
package telemt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LankoRu/vps-admin/internal/config"
	"github.com/LankoRu/vps-admin/internal/service"
	"github.com/LankoRu/vps-admin/internal/util"
)

const serviceName = "telemt"

type Service struct {
	cfg    *config.TeleMTConfig
	client *http.Client
}

func New(cfg *config.Config) *Service {
	return &Service{
		cfg:    &cfg.TeleMT,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *Service) Name() string { return serviceName }

func (s *Service) newRequest(method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	r, err := http.NewRequest(method, s.cfg.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if s.cfg.AdminKey != "" {
		// telemt ждёт точное совпадение значения заголовка Authorization.
		r.Header.Set("Authorization", s.cfg.AdminKey)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r, nil
}

// userInfo — фрагмент ответа telemt, который нам нужен.
type userInfo struct {
	Username    string    `json:"username"`
	TotalOctets uint64    `json:"total_octets"`
	Links       userLinks `json:"links"`
}

// userLinks — ссылки, сгенерированные telemt для всех включённых режимов.
type userLinks struct {
	Classic    []string `json:"classic"` // classic-режим
	Secure     []string `json:"secure"`  // dd (обфускация)
	TLS        []string `json:"tls"`     // ee (FakeTLS)
	TLSDomains []struct {
		Domain string `json:"domain"`
		Link   string `json:"link"`
	} `json:"tls_domains"`
}

func (l userLinks) empty() bool {
	return len(l.Classic) == 0 && len(l.Secure) == 0 && len(l.TLS) == 0 && len(l.TLSDomains) == 0
}

// fetchStatsUsers читает GET /v1/stats/users → пользователи с трафиком
// (total_octets) и ссылками (links).
func (s *Service) fetchStatsUsers() ([]userInfo, error) {
	req, err := s.newRequest(http.MethodGet, "/v1/stats/users", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telemt http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telemt stats status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	users, err := parseStatsUsers(raw)
	if err != nil {
		return nil, fmt.Errorf("decode telemt stats: %w", err)
	}
	return users, nil
}

// GetTraffic — трафик по каждому пользователю.
func (s *Service) GetTraffic() (map[string]uint64, error) {
	users, err := s.fetchStatsUsers()
	if err != nil {
		return nil, err
	}
	traffic := make(map[string]uint64)
	for _, u := range users {
		if u.Username != "" {
			traffic[u.Username] = u.TotalOctets
		}
	}
	log.Printf("[telemt] собрана статистика по %d пользователям", len(traffic))
	return traffic, nil
}

// userLinksFor находит ссылки конкретного пользователя в /v1/stats/users.
func (s *Service) userLinksFor(username string) userLinks {
	users, err := s.fetchStatsUsers()
	if err != nil {
		return userLinks{}
	}
	for _, u := range users {
		if u.Username == username {
			return u.Links
		}
	}
	return userLinks{}
}

// isIPv6Link — true, если server в tg://-ссылке является IPv6-адресом.
func isIPv6Link(link string) bool {
	u, err := url.Parse(link)
	if err != nil {
		return false
	}
	server := strings.Trim(u.Query().Get("server"), "[]")
	ip := net.ParseIP(server)
	return ip != nil && ip.To4() == nil
}

// ipv4Only отбрасывает ссылки с IPv6-сервером (оставляет IPv4 и доменные).
func ipv4Only(links []string) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		if !isIPv6Link(l) {
			out = append(out, l)
		}
	}
	return out
}

// buildItems собирает варианты подключения telemt (секрет + ссылки, только IPv4;
// IPv6 отбрасываются). Без QR (по требованию).
func buildItems(secret string, l userLinks) []service.Item {
	items := []service.Item{{Label: "Секрет", Value: secret}}
	add := func(label string, links []string) {
		for _, link := range ipv4Only(links) {
			items = append(items, service.Item{Label: label, Value: link})
		}
	}
	add("classic", l.Classic)
	add("dd (обфускация)", l.Secure)
	add("ee (FakeTLS)", l.TLS)
	for _, d := range l.TLSDomains {
		if !isIPv6Link(d.Link) {
			items = append(items, service.Item{Label: "ee (" + d.Domain + ")", Value: d.Link})
		}
	}
	return items
}

// parseStatsUsers терпимо разбирает ответ /v1/stats/users, форма которого отличается
// между версиями telemt: конверт {ok,data:[…]} (текущая), голый массив, другие
// обёртки (users|items|result) или объект-словарь username→UserInfo.
func parseStatsUsers(raw []byte) ([]userInfo, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}

	if raw[0] == '[' { // голый массив
		var users []userInfo
		if err := json.Unmarshal(raw, &users); err != nil {
			return nil, err
		}
		return users, nil
	}

	if raw[0] == '{' {
		// Обёртка с массивом под известным ключом.
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		for _, key := range []string{"users", "data", "items", "result"} {
			if inner, ok := obj[key]; ok {
				var users []userInfo
				if err := json.Unmarshal(inner, &users); err != nil {
					return nil, err
				}
				return users, nil
			}
		}
		// Иначе — словарь username→UserInfo.
		var m map[string]userInfo
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		users := make([]userInfo, 0, len(m))
		for name, u := range m {
			if u.Username == "" {
				u.Username = name
			}
			users = append(users, u)
		}
		return users, nil
	}

	return nil, fmt.Errorf("неизвестный формат ответа (%.20s…)", raw)
}

// Provision создаёт пользователя (POST /v1/users пишет его в config.toml
// telemt) и возвращает сгенерированные telemt ссылки dd и ee.
func (s *Service) Provision(username string) (*service.Account, error) {
	secret, err := util.RandHex(16) // 16 байт → 32 hex-символа (формат telemt)
	if err != nil {
		return nil, err
	}

	body, _ := json.Marshal(map[string]interface{}{
		"username": username,
		"secret":   secret,
		"enabled":  true,
	})
	req, err := s.newRequest(http.MethodPost, "/v1/users", body)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telemt create: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated &&
		resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("telemt create status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var cr struct {
		User   userInfo `json:"user"`
		Secret string   `json:"secret"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	if cr.Secret != "" {
		secret = cr.Secret
	}

	// Ссылки в ответе создания telemt заполняет не всегда (считаются по данным
	// рантайма). Если пусто — дочитываем из /v1/stats/users.
	links := cr.User.Links
	if links.empty() {
		links = s.userLinksFor(username)
	}

	items := buildItems(secret, links)
	log.Printf("[telemt] пользователь создан: %s (ссылок: %d)", username, len(items)-1)
	if len(items) <= 1 { // только секрет, ссылок нет
		items = append(items, service.Item{
			Label: "⚠️",
			Value: "telemt не вернул ссылки — в config.toml включите режимы в [general.modes] " +
				"и задайте [general.links] public_host/public_port (или дайте серверу определить внешний IP)",
		})
	}

	// WEB-прокси (best-effort): профиль + ссылка, если WEB включён в telemt.
	// Профиль добавляем ПОСЛЕ создания пользователя (telemt валидирует, что
	// profile.user есть в access.users).
	if host, werr := s.ensureWebProfile(username); werr != nil {
		log.Printf("[telemt] WEB-профиль для %s не создан: %v", username, werr)
	} else if host != "" {
		items = append(items, webItem(host, secret))
	}

	return &service.Account{
		Service:  serviceName,
		Identity: username,
		Items:    items,
	}, nil
}

// Deprovision удаляет пользователя telemt (и из config.toml).
func (s *Service) Deprovision(identity string) error {
	// WEB-профиль убираем ДО удаления пользователя: telemt не удалит access.user,
	// на который ссылается WEB-профиль (validate). Best-effort — если WEB выключен,
	// это no-op.
	s.removeWebProfile(identity)
	req, err := s.newRequest(http.MethodDelete, "/v1/users/"+identity, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// telemt отвечает 202 Accepted ({"ok":true,...}) — принимаем любой 2xx.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telemt delete status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	log.Printf("[telemt] пользователь удалён: %s", identity)
	return nil
}

package telemt

// web.go — поддержка WEB-прокси Telegram (новый тип прокси, Desktop 7.1+):
// MTProto внутри обычных HTTPS/WebSocket-запросов к собственному домену.
//
// В telemt WEB-режим требует ЯВНЫЙ профиль [[web.vhosts.profiles]] на каждого
// пользователя ([access.users] сам по себе WEB не включает). Поэтому:
//   - Provision: после POST /v1/users добавляем профиль через PATCH /v1/config;
//   - Deprovision: СНАЧАЛА убираем профиль, потом DELETE /v1/users — иначе
//     telemt не удалит пользователя (validate: профиль ссылается на access.user);
//   - ссылки tg://webproxy считаем сами: telemt 3.5.x не отдаёт их через API.
//
// WEB не поддерживает ee-секреты (FakeTLS) — используем dd с тем же 16-байтным
// секретом пользователя. base_path пуст (выделенный домен), поэтому ссылка вида
// tg://webproxy?server=HOST&secret=dd<32hex>.
//
// Всё WEB-взаимодействие — best-effort: если WEB в telemt не включён или API
// недоступен, провижининг MTProto не ломается (просто без WEB-ссылки).

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/LankoRu/vps-admin/internal/service"
)

// webTable читает GET /v1/config и возвращает секцию web (data.web).
// Возвращает (nil, nil), если секции web нет. Ошибка — только транспорт/статус.
func (s *Service) webTable() (map[string]interface{}, error) {
	req, err := s.newRequest(http.MethodGet, "/v1/config", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telemt config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telemt config status %d", resp.StatusCode)
	}
	var env struct {
		OK   bool                   `json:"ok"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode telemt config: %w", err)
	}
	web, _ := env.Data["web"].(map[string]interface{})
	return web, nil
}

// webVhosts извлекает []vhost из секции web и первый непустой host.
func webVhosts(web map[string]interface{}) ([]interface{}, string) {
	vhosts, _ := web["vhosts"].([]interface{})
	host := ""
	for _, v := range vhosts {
		if vh, ok := v.(map[string]interface{}); ok {
			if h, _ := vh["host"].(string); h != "" && host == "" {
				host = h
			}
		}
	}
	return vhosts, host
}

// ensureWebProfile гарантирует профиль (secret_mode=dd) для пользователя во всех
// WEB-vhost'ах. Возвращает host для ссылки. Если WEB выключен/нет vhost'ов —
// ("", nil) без ошибки. Массив vhosts в PATCH заменяется целиком, поэтому
// читаем текущий (GET /v1/config), правим profiles и отправляем обратно.
func (s *Service) ensureWebProfile(username string) (string, error) {
	web, err := s.webTable()
	if err != nil {
		return "", err
	}
	if web == nil {
		return "", nil
	}
	if enabled, _ := web["enabled"].(bool); !enabled {
		return "", nil
	}
	vhosts, host := webVhosts(web)
	if len(vhosts) == 0 || host == "" {
		return "", nil
	}
	changed := false
	for _, v := range vhosts {
		vh, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		profiles, _ := vh["profiles"].([]interface{})
		found := false
		for _, p := range profiles {
			if pm, _ := p.(map[string]interface{}); pm != nil && pm["user"] == username {
				found = true
				break
			}
		}
		if !found {
			vh["profiles"] = append(profiles, map[string]interface{}{
				"user":        username,
				"secret_mode": "dd",
			})
			changed = true
		}
	}
	if changed {
		if err := s.patchWebVhosts(vhosts); err != nil {
			return host, err
		}
	}
	return host, nil
}

// removeWebProfile убирает профиль пользователя из WEB-vhost'ов (best-effort).
// Вызывается ДО удаления пользователя: telemt не удалит access.user, на который
// ссылается WEB-профиль. Пустой profiles при enabled=true telemt не примет, но
// последнего пользователя (hello) telemt и так не даёт удалить — профиль всегда
// остаётся непустым.
func (s *Service) removeWebProfile(username string) {
	web, err := s.webTable()
	if err != nil || web == nil {
		return
	}
	if enabled, _ := web["enabled"].(bool); !enabled {
		return
	}
	vhosts, _ := webVhosts(web)
	changed := false
	for _, v := range vhosts {
		vh, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		profiles, _ := vh["profiles"].([]interface{})
		kept := make([]interface{}, 0, len(profiles))
		removed := false
		for _, p := range profiles {
			if pm, _ := p.(map[string]interface{}); pm != nil && pm["user"] == username {
				removed = true
				continue
			}
			kept = append(kept, p)
		}
		if removed {
			vh["profiles"] = kept
			changed = true
		}
	}
	if changed {
		if err := s.patchWebVhosts(vhosts); err != nil {
			log.Printf("[telemt] WEB-профиль %s не удалён: %v", username, err)
		}
	}
}

// patchWebVhosts отправляет PATCH /v1/config?reload=instant с массивом vhosts.
// reload=instant применяет профили в рантайме без рестарта (они hot-reloadable).
func (s *Service) patchWebVhosts(vhosts []interface{}) error {
	body, err := json.Marshal(map[string]interface{}{
		"web": map[string]interface{}{"vhosts": vhosts},
	})
	if err != nil {
		return err
	}
	req, err := s.newRequest(http.MethodPatch, "/v1/config?reload=instant", body)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("telemt patch config: %w", err)
	}
	defer resp.Body.Close()
	// 200 OK (без reload) или 202 Accepted (reload запрошен) — оба успех.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telemt patch config status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// webLink собирает ссылку WEB-прокси для выделенного домена (base_path пуст).
// Формат совпадает с telemt format_web_proxy_link: dd-режим → префикс dd.
func webLink(host, secret string) string {
	return "tg://webproxy?server=" + host + "&secret=dd" + secret
}

// webItem — пункт WEB-ссылки для выдачи пользователю (панель/бот).
func webItem(host, secret string) service.Item {
	return service.Item{Label: "WEB (HTTPS, Desktop 7.1+)", Value: webLink(host, secret)}
}

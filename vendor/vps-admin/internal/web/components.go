package web

// components.go — API версий/обновлений компонентов стека («Система»).
// Обновление асинхронное: POST запускает job, UI опрашивает GET и показывает
// живой лог в окне прогресса.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LankoRu/vps-admin/internal/components"
)

func (s *Server) handleComponents(w http.ResponseWriter, r *http.Request) {
	// Версии — из снимка (обновляется раз в час), не «актуальные» из сети.
	checked := ""
	if t := s.comp.RefreshedAt(); !t.IsZero() {
		checked = t.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"components": s.comp.List(),
		"links":      s.componentLinks(), // ссылки на все веб-страницы стека
		"checked_at": checked,            // когда снимок обновлялся ("" — ещё нет)
		"interval":   int(components.RefreshInterval.Seconds()),
	})
}

// handleComponentsRefresh — принудительная проверка обновлений (кнопка
// «Проверить»). Асинхронно: сеть может отвечать секунды, UI опрашивает снимок.
func (s *Server) handleComponentsRefresh(w http.ResponseWriter, r *http.Request) {
	go s.comp.Refresh(context.Background())
	writeJSON(w, http.StatusOK, map[string]bool{"started": true})
}

// componentLinks — ссылки на другие веб-страницы стека для раздела «Компоненты»
// (раньше 3x-ui висел в шапке; подписки и сайт-прикрытие вообще были не видны).
// Ссылки на саму эту панель нет — она уже открыта.
// Отдаётся только в авторизованном API.
func (s *Server) componentLinks() []map[string]string {
	var out []map[string]string
	host := strings.TrimSpace(s.cfg.Xray3xUI.PublicHost)
	if host == "" && s.cfg.Xray3xUI.SubURL != "" {
		if u, err := url.Parse(s.cfg.Xray3xUI.SubURL); err == nil {
			host = u.Hostname()
		}
	}
	if host == "" {
		host = strings.TrimSpace(s.cfg.AmneziaWG3xUI.PublicHost)
	}
	if host != "" {
		panel := "https://" + host + "/"
		if path := strings.Trim(s.cfg.Xray3xUI.WebBasePath, "/"); path != "" {
			panel += path + "/"
		}
		out = append(out, map[string]string{"label": "3x-ui (панель)", "url": panel})
		out = append(out, map[string]string{"label": "Сайт-прикрытие", "url": "https://" + host + "/"})
	}
	if sub := strings.TrimSpace(s.cfg.Xray3xUI.SubURL); sub != "" {
		out = append(out, map[string]string{"label": "Подписки", "url": sub})
	}
	return out
}

func (s *Server) handleComponentUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.comp.Start(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"started": true, "name": name})
}

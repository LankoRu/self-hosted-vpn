package web

// components.go — API версий/обновлений компонентов стека («Система»).
// Обновление асинхронное: POST запускает job, UI опрашивает GET и показывает
// живой лог в окне прогресса.

import (
	"net/http"
	"net/url"
	"strings"
)

func (s *Server) handleComponents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"components": s.comp.List(),
		"links":      s.componentLinks(), // ссылки на все веб-страницы стека
	})
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

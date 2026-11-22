package web

// connections.go — API раздела «Подключения» (антиблокировка): состояние,
// ротация Reality-SNI/dest/fingerprint, смена домена-маски telemt, запасной
// транспорт XHTTP/gRPC. Оркестрация — в manager (connections.go).

import (
	"encoding/json"
	"net/http"
)

func (s *Server) handleConnectionsState(w http.ResponseWriter, r *http.Request) {
	info, err := s.mgr.ConnectionsState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleConnectionsReality(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ServerNames []string `json:"server_names"`
		Dest        string   `json:"dest"`
		Fingerprint string   `json:"fingerprint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "неверный запрос")
		return
	}
	if err := s.mgr.UpdateReality(req.ServerNames, req.Dest, req.Fingerprint); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
		"warning": "Xray перезапущен (кратковременный разрыв). VLESS-подписка обновится сама; " +
			"статические ссылки перевыпущены в карточках пользователей — раздайте новые конфиги.",
	})
}

func (s *Server) handleConnectionsTelemt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Domain string `json:"domain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "неверный запрос")
		return
	}
	if err := s.mgr.SetTelemtDomain(req.Domain); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
		"warning": "Все Telegram-ссылки изменились (домен зашит в secret). Новые ссылки уже в карточках " +
			"пользователей и на /p/-страницах — раздайте их клиентам.",
	})
}

func (s *Server) handleConnectionsBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled     bool     `json:"enabled"`
		Transport   string   `json:"transport"`
		ServerNames []string `json:"server_names"`
		Dest        string   `json:"dest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "неверный запрос")
		return
	}
	if err := s.mgr.SetBackup(req.Enabled, req.Transport, req.ServerNames, req.Dest); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
		"warning": "Ссылки пользователей обновлены: при включении запасного транспорта в них " +
			"появляется второй вариант подключения (подписка обновится сама).",
	})
}

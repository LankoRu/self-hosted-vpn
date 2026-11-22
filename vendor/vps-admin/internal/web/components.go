package web

// components.go — API версий/обновлений компонентов стека («Система»).
// Обновление асинхронное: POST запускает job, UI опрашивает GET и показывает
// живой лог в окне прогресса.

import (
	"net/http"
)

func (s *Server) handleComponents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.comp.List())
}

func (s *Server) handleComponentUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.comp.Start(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"started": true, "name": name})
}

package server

import (
	"encoding/json"
	"net/http"
	"piratesbot/internal/bot"
)

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	state := s.b.State()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	logs := s.b.GetNewLogs()
	if logs == nil {
		logs = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(logs)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.b.Start()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.b.Stop()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleStrategy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Strategy bot.Strategy `json:"strategy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	s.b.SetStrategy(req.Strategy)
	w.WriteHeader(http.StatusOK)
}

package server

import (
	"net/http"
	"piratesbot/internal/bot"
)

type Server struct {
	addr string
	b    *bot.Bot
	mux  *http.ServeMux
}

func NewServer(addr string, b *bot.Bot) *Server {
	s := &Server{
		addr: addr,
		b:    b,
		mux:  http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/api/state", s.handleState)
	s.mux.HandleFunc("/api/logs", s.handleLogs)
	s.mux.HandleFunc("/api/start", s.handleStart)
	s.mux.HandleFunc("/api/stop", s.handleStop)

	// Since we execute from `backend/`, frontend is at `../frontend`
	fs := http.FileServer(http.Dir(`../frontend`))
	s.mux.Handle("/", fs)
}

func (s *Server) Start() error {
	return http.ListenAndServe(s.addr, corsOpts(s.mux))
}

func corsOpts(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		h.ServeHTTP(w, r)
	})
}

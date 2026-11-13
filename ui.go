package main

import (
	_ "embed"
	"net/http"
)

//go:embed ui.html
var uiHTML string

func (s *server) handleUI(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "/ui", "/index.html":
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(uiHTML))
}

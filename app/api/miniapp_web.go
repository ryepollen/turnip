package api

import (
	"embed"
	"io/fs"
	"net/http"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/go-pkgz/routegroup"
)

// miniappFS holds the Mini App frontend (shell + JS/CSS), baked into the binary
// so serving it never depends on a working directory or a mounted volume.
//
//go:embed miniapp/index.html miniapp/static
var miniappFS embed.FS

// miniappWebRoutes serves the public shell and static assets. The HTML carries
// no data — everything sensitive lives behind /wegweiser/api under miniAuth —
// so the shell itself needs no gate: Telegram injects initData into the page at
// runtime, and the JS attaches it to every API call.
func (s *Server) miniappWebRoutes(router *routegroup.Bundle) {
	static, err := fs.Sub(miniappFS, "miniapp/static")
	if err != nil {
		log.Printf("[WARN] miniapp static embed unavailable: %v", err)
		return
	}
	router.Group().Route(func(r *routegroup.Bundle) {
		r.Use(timeout(60 * time.Second))
		r.HandleFunc("GET /wegweiser", s.miniappShellCtrl)
		r.HandleFunc("GET /wegweiser/", s.miniappShellCtrl)
		fileServer := http.StripPrefix("/wegweiser/static/", http.FileServer(http.FS(static)))
		r.Handle("GET /wegweiser/static/{file...}", cacheControl(fileServer, "public, max-age=3600"))
	})
}

// GET /wegweiser — the app shell
func (s *Server) miniappShellCtrl(w http.ResponseWriter, _ *http.Request) {
	b, err := miniappFS.ReadFile("miniapp/index.html")
	if err != nil {
		http.Error(w, "miniapp not built", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

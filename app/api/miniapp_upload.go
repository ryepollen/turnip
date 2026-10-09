package api

import (
	"fmt"
	"net/http"

	log "github.com/go-pkgz/lgr"
	"github.com/go-pkgz/rest"

	"github.com/umputun/feed-master/app/publisher"
)

// Web ingest: the «➕ Занести» zone of the Library tab. The browser asks for
// presigned PUT URLs, uploads each part straight to R2 (bytes never pass
// through this server), then commits — the server verifies the objects landed
// and writes the 0-byte stub skeleton that makes the work a dormant catalog
// entry. Both endpoints sit behind miniAuth like every other Mini App action.

const maxUploadBatch = 200 // parts per work; the longest real work so far is 228 eps across seasons

// POST /wegweiser/api/library/upload/sign
// {"category":"books","work":"Название","files":[{"name":"01 - Часть.mp3"},…]}
// → {"files":[{"name":…,"url":…,"content_type":…},…]}
func (s *Server) appLibraryUploadSignCtrl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Category string `json:"category"`
		Work     string `json:"work"`
		Files    []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := decodeBody(r, &body); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "bad request")
		return
	}
	if s.Pub == nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable,
			fmt.Errorf("publishing not configured"), "library unavailable")
		return
	}
	if len(body.Files) == 0 || len(body.Files) > maxUploadBatch {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest,
			fmt.Errorf("need 1..%d files, got %d", maxUploadBatch, len(body.Files)), "bad request")
		return
	}
	type signed struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
	}
	out := make([]signed, 0, len(body.Files))
	for _, f := range body.Files {
		url, err := s.Pub.PresignUploadPart(r.Context(), body.Category, body.Work, f.Name)
		if err != nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "cannot sign upload")
			return
		}
		out = append(out, signed{Name: f.Name, URL: url, ContentType: publisher.UploadMime(f.Name)})
	}
	rest.RenderJSON(w, rest.JSON{"status": "ok", "files": out})
}

// POST /wegweiser/api/library/upload/commit
// {"category":"books","work":"Название","files":["01 - Часть.mp3",…]}
// Verifies every part actually landed in R2 (409 + the missing list otherwise),
// then writes the stub skeleton; the work shows up dormant in the catalog.
func (s *Server) appLibraryUploadCommitCtrl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Category string   `json:"category"`
		Work     string   `json:"work"`
		Files    []string `json:"files"`
	}
	if err := decodeBody(r, &body); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "bad request")
		return
	}
	if s.Pub == nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable,
			fmt.Errorf("publishing not configured"), "library unavailable")
		return
	}
	if len(body.Files) == 0 || len(body.Files) > maxUploadBatch {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest,
			fmt.Errorf("need 1..%d files, got %d", maxUploadBatch, len(body.Files)), "bad request")
		return
	}
	missing, err := s.Pub.VerifyUploaded(r.Context(), body.Category, body.Work, body.Files)
	if err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "cannot verify upload")
		return
	}
	if len(missing) > 0 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		rest.RenderJSON(w, rest.JSON{"status": "incomplete", "missing": missing})
		return
	}
	if err := s.Pub.RegisterWork(body.Category, body.Work, body.Files); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to register work")
		return
	}
	log.Printf("[INFO] mini app ingested «%s» (%s): %d parts now in library", body.Work, body.Category, len(body.Files))
	rest.RenderJSON(w, rest.JSON{"status": "ok", "work": body.Work, "category": body.Category, "parts": len(body.Files)})
}

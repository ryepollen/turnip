package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	log "github.com/go-pkgz/lgr"
	"github.com/go-pkgz/rest"

	ytfeed "github.com/umputun/feed-master/app/youtube/feed"
)

// decodeBody reads a small JSON body into dst, capped so a stuck client can't
// stream forever. Empty body is allowed (dst stays zero).
func decodeBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16))
	if err := dec.Decode(dst); err != nil && err.Error() != "EOF" {
		return err
	}
	return nil
}

// POST /wegweiser/api/feed/delete {"video_id":"..."} — same as /del: file + R2
// object + bucket + history mark, via the bot's one delete path.
func (s *Server) appFeedDeleteCtrl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VideoID string `json:"video_id"`
	}
	if err := decodeBody(r, &body); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "bad request")
		return
	}
	if body.VideoID == "" {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, fmt.Errorf("video_id required"), "bad request")
		return
	}
	if s.DeleteFeedEntry == nil || s.YoutubeStore == nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable,
			fmt.Errorf("delete not available"), "feed delete not wired")
		return
	}
	entries, err := s.YoutubeStore.Load(s.feedName(), 0)
	if err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to load feed")
		return
	}
	var found *ytfeed.Entry
	for i := range entries {
		if entries[i].VideoID == body.VideoID {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusNotFound,
			fmt.Errorf("entry %s not found", body.VideoID), "not in feed")
		return
	}
	if err := s.DeleteFeedEntry(*found); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to delete entry")
		return
	}
	rest.RenderJSON(w, rest.JSON{"status": "ok", "deleted": body.VideoID})
}

// POST /wegweiser/api/notes/enqueue {"url":"...","level":"notes"|"md"} — queue a
// transcript/notes job (the 📓 button). Priority is "user" so it jumps a bulk batch.
func (s *Server) appNotesEnqueueCtrl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL   string `json:"url"`
		Level string `json:"level"`
	}
	if err := decodeBody(r, &body); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "bad request")
		return
	}
	if body.URL == "" {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, fmt.Errorf("url required"), "bad request")
		return
	}
	if body.Level != "md" {
		body.Level = "notes"
	}
	if s.NotesSvc == nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable,
			fmt.Errorf("notes not configured"), "notes unavailable")
		return
	}
	if err := s.NotesSvc.EnqueueURL(body.URL, body.Level, miniappUserPriority); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusConflict, err, "failed to enqueue")
		return
	}
	rest.RenderJSON(w, rest.JSON{"status": "ok", "queued": body.URL})
}

// POST /wegweiser/api/notes/delete {"source_id":"..."} — delete an L1 transcript file
func (s *Server) appNotesDeleteCtrl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SourceID string `json:"source_id"`
	}
	if err := decodeBody(r, &body); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "bad request")
		return
	}
	if s.NotesSvc == nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable,
			fmt.Errorf("notes not configured"), "notes unavailable")
		return
	}
	if err := s.NotesSvc.Delete(body.SourceID); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "failed to delete transcript")
		return
	}
	rest.RenderJSON(w, rest.JSON{"status": "ok", "deleted": body.SourceID})
}

// POST /wegweiser/api/read/delete {"source_id":"..."} — delete a saved article
func (s *Server) appReadDeleteCtrl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SourceID string `json:"source_id"`
	}
	if err := decodeBody(r, &body); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "bad request")
		return
	}
	if s.ReadSvc == nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable,
			fmt.Errorf("reader not configured"), "reader unavailable")
		return
	}
	if err := s.ReadSvc.Delete(body.SourceID); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "failed to delete article")
		return
	}
	rest.RenderJSON(w, rest.JSON{"status": "ok", "deleted": body.SourceID})
}

// POST /wegweiser/api/queue/pause {"paused":true|false} — /pause and /resume
func (s *Server) appQueuePauseCtrl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused bool `json:"paused"`
	}
	if err := decodeBody(r, &body); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "bad request")
		return
	}
	if s.NotesSvc == nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable,
			fmt.Errorf("queue not configured"), "queue unavailable")
		return
	}
	if err := s.NotesSvc.SetPaused(body.Paused); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to set pause")
		return
	}
	rest.RenderJSON(w, rest.JSON{"status": "ok", "paused": body.Paused})
}

// miniappUserPriority marks Mini App enqueues as user-initiated so a tapped
// link jumps ahead of a bulk playlist batch, matching the bot's tap behaviour.
const miniappUserPriority = 1

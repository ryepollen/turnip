package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/go-pkgz/rest"

	"github.com/umputun/feed-master/app/publisher"
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

// POST /wegweiser/api/library/activate {"category":"books","work":"slug"} — make a
// work live in its category feed (the ▶ Listen button, mirrors the bot's /library).
func (s *Server) appLibraryActivateCtrl(w http.ResponseWriter, r *http.Request) {
	s.libraryAction(w, r, publisher.OpActivate)
}

// POST /wegweiser/api/library/deactivate {"category":"books","work":"slug"} — empty
// a category feed (the ⏹ Stop button).
func (s *Server) appLibraryDeactivateCtrl(w http.ResponseWriter, r *http.Request) {
	s.libraryAction(w, r, publisher.OpDeactivate)
}

// libraryAction validates the work against the live catalog, then either enqueues a
// request for the Mac agent (variant A, when ActQueue is set) or runs the op
// in-process. It mirrors the bot's runActivate/runDeactivate so the web and the bot
// drive the same single-active model: activating a work evicts the previous one from
// that category's feed. Long uploads never block the request — the caller polls the
// catalog to see the Active flag flip.
func (s *Server) libraryAction(w http.ResponseWriter, r *http.Request, op publisher.ActivationOp) {
	var body struct {
		Category string `json:"category"`
		Work     string `json:"work"`
	}
	if err := decodeBody(r, &body); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "bad request")
		return
	}
	if body.Category == "" || body.Work == "" {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest,
			fmt.Errorf("category and work required"), "bad request")
		return
	}
	if s.Pub == nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable,
			fmt.Errorf("publishing not configured"), "library unavailable")
		return
	}

	// validate against the catalog: the scan only ever returns real work folders, so
	// a match rules out both a stale UI and a path-traversal attempt in one step
	cw, ok, err := s.findCatWork(body.Category, body.Work)
	if err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to scan library")
		return
	}
	if !ok {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusNotFound,
			fmt.Errorf("work %q not in %s", body.Work, body.Category), "not found")
		return
	}
	switch op {
	case publisher.OpActivate:
		if cw.Active {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusConflict,
				fmt.Errorf("«%s» already live", cw.Title), "already active")
			return
		}
	case publisher.OpDeactivate:
		if !cw.Active {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusConflict,
				fmt.Errorf("«%s» not live", cw.Title), "not active")
			return
		}
	}

	// variant A: hand off to the Mac agent through the queue (the VM holds 0-byte stub
	// originals and can't read the audio). No Telegram status message — the web polls.
	if s.ActQueue != nil {
		if _, eqErr := s.ActQueue.Enqueue(publisher.ActivationRequest{
			Op: op, Category: cw.Category, Work: cw.Slug,
		}); eqErr != nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, eqErr, "failed to enqueue")
			return
		}
		rest.RenderJSON(w, rest.JSON{"status": "ok", "queued": true, "op": string(op), "work": cw.Slug})
		return
	}

	// single-machine mode: run detached, log the outcome; the web polls for the flip
	go s.runLibraryOp(op, cw)
	rest.RenderJSON(w, rest.JSON{"status": "ok", "started": true, "op": string(op), "work": cw.Slug})
}

// findCatWork loads the catalog and returns the work matching category+slug.
func (s *Server) findCatWork(category, slug string) (publisher.CatWork, bool, error) {
	works, err := s.Pub.Catalog()
	if err != nil {
		return publisher.CatWork{}, false, err
	}
	for _, w := range works {
		if w.Category == category && w.Slug == slug {
			return w, true, nil
		}
	}
	return publisher.CatWork{}, false, nil
}

// runLibraryOp performs an in-process activate/deactivate detached from the request
// (only reached when the queue is off), logging the outcome. Activation of a whole
// work can be gigabytes, so it gets a generous ceiling.
func (s *Server) runLibraryOp(op publisher.ActivationOp, cw publisher.CatWork) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	var err error
	switch op {
	case publisher.OpActivate:
		err = s.Pub.Activate(ctx, cw.Category, cw.Slug, nil)
	case publisher.OpDeactivate:
		err = s.Pub.Deactivate(ctx, cw.Category)
	}
	if err != nil {
		log.Printf("[WARN] mini app library %s «%s» (%s) failed: %v", op, cw.Title, cw.Category, err)
		return
	}
	log.Printf("[INFO] mini app library %s «%s» (%s) done", op, cw.Title, cw.Category)
}

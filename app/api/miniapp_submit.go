package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/go-pkgz/rest"
)

// POST /wegweiser/api/submit — the link drop for everything outside Telegram
// (#78): the userscript's «→ turnip» buttons, iOS Shortcuts, anything that can
// send one authenticated POST. Actions mirror the bot menu's headless halves:
//
//	audio — YouTube audio into the feed (detached; outcome lands in the chat)
//	notes — full Notion summary via the notes queue
//	md    — transcript only, same queue
//	read  — structured article into the reader
//
// Voiceover (/vo) is deliberately not here yet: its web path needs the status
// flow untangled from Telegram first.
func (s *Server) appSubmitCtrl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL    string `json:"url"`
		Action string `json:"action"`
	}
	if err := decodeBody(r, &body); err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "bad request")
		return
	}
	link := cleanTrackingParams(strings.TrimSpace(body.URL))
	if link == "" {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, fmt.Errorf("url required"), "bad request")
		return
	}

	switch body.Action {
	case "audio":
		if s.SubmitAudio == nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable,
				fmt.Errorf("bot not wired"), "audio unavailable")
			return
		}
		if err := s.SubmitAudio(link); err != nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, err, "cannot queue audio")
			return
		}
	case "notes", "md":
		if s.NotesSvc == nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable,
				fmt.Errorf("notes not configured"), "notes unavailable")
			return
		}
		if err := s.NotesSvc.EnqueueURL(link, body.Action, miniappUserPriority); err != nil {
			// the queue's own wording is user-facing ru («задача уже в очереди»)
			rest.SendErrorJSON(w, r, log.Default(), http.StatusConflict, err, err.Error())
			return
		}
	case "read":
		if s.ReadSvc == nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable,
				fmt.Errorf("reader not configured"), "reader unavailable")
			return
		}
		go func(u string) { // Save fetches the page — never block the button
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if _, err := s.ReadSvc.Save(ctx, u); err != nil {
				log.Printf("[WARN] web submit read failed for %s: %v", u, err)
				if s.Notify != nil {
					s.Notify(fmt.Sprintf("❌ в читалку из браузера: %s — %v", u, err))
				}
				return
			}
			if s.Notify != nil {
				s.Notify(fmt.Sprintf("📖 в читалке (из браузера): %s", u))
			}
		}(link)
	default:
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest,
			fmt.Errorf("unknown action %q", body.Action), "bad request")
		return
	}

	log.Printf("[INFO] web submit %s: %s", body.Action, link)
	rest.RenderJSON(w, rest.JSON{"status": "ok", "action": body.Action, "url": link})
}

// corsMini answers browser preflights and marks the Mini App API callable
// from foreign origins — the userscript lives on youtube.com and may have to
// fall back to plain fetch (#78). '*' is safe here: auth is the explicit
// token header, never cookies, so there is nothing ambient for CORS to guard.
func corsMini(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Wegweiser-Token")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// trackingParams are query keys that identify the sharer, not the content —
// they make every pasted link unique and poison dedup (#82's seed).
var trackingParams = map[string]bool{
	"si": true, "feature": true, "pp": true,
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true, "utm_id": true,
	"fbclid": true, "gclid": true, "igshid": true, "mc_cid": true, "mc_eid": true,
	"ref": true, "ref_src": true, "cmpid": true,
}

// cleanTrackingParams strips sharer-tracking query keys, keeping everything
// that addresses content (v, t, list, i, …). Unparseable input is returned
// as-is — the pipelines will complain about it in their own words.
func cleanTrackingParams(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		return rawURL
	}
	q := u.Query()
	changed := false
	for k := range q {
		if trackingParams[strings.ToLower(k)] {
			q.Del(k)
			changed = true
		}
	}
	if !changed {
		return rawURL
	}
	u.RawQuery = q.Encode()
	return u.String()
}

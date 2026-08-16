package api

import (
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/go-pkgz/rest"
	"github.com/go-pkgz/routegroup"
)

// miniappRoutes mounts the Mini App JSON API under /wegweiser/api, all of it
// behind the single-user initData gate. Each handler is a thin read wrapper
// over the same services the bot uses — no new store, just a web-shaped view.
func (s *Server) miniappRoutes(router *routegroup.Bundle) {
	router.Mount("/wegweiser/api").Route(func(r *routegroup.Bundle) {
		r.Use(timeout(60 * time.Second))
		r.Use(s.miniAuth)
		r.HandleFunc("GET /summary", s.appSummaryCtrl)
		r.HandleFunc("GET /feed", s.appFeedCtrl)
		r.HandleFunc("GET /history", s.appHistoryCtrl)
		r.HandleFunc("GET /notes", s.appNotesCtrl)
		r.HandleFunc("GET /read", s.appReadCtrl)
		r.HandleFunc("GET /refs", s.appRefsCtrl)
		r.HandleFunc("GET /status", s.appStatusCtrl)
		r.HandleFunc("GET /feeds", s.appFeedsCtrl)
		r.HandleFunc("GET /notes/file", s.appNotesFileCtrl)
		r.HandleFunc("GET /read/file", s.appReadFileCtrl)

		// actions — the same primitives the bot commands drive, over the visible list
		r.HandleFunc("POST /feed/delete", s.appFeedDeleteCtrl)
		r.HandleFunc("POST /notes/enqueue", s.appNotesEnqueueCtrl)
		r.HandleFunc("POST /notes/delete", s.appNotesDeleteCtrl)
		r.HandleFunc("POST /read/delete", s.appReadDeleteCtrl)
		r.HandleFunc("POST /queue/pause", s.appQueuePauseCtrl)
	})
}

// appPage is the envelope for every paginated list: the items plus enough to
// drive the frontend's ◀▶ pager without a second round-trip.
type appPage struct {
	Items  any `json:"items"`
	Total  int `json:"total"`
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

// pageParams parses ?offset=&limit= with a default and a hard cap so a hostile
// or fat-fingered limit can't ask us to marshal the whole store at once.
func pageParams(r *http.Request, defLimit int) (offset, limit int) {
	limit = defLimit
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > 100 {
		limit = 100
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	return offset, limit
}

// window clamps [offset, offset+limit) to a slice of length n
func window(n, offset, limit int) (start, end int) {
	if offset > n {
		offset = n
	}
	end = offset + limit
	if end > n {
		end = n
	}
	return offset, end
}

func (s *Server) feedName() string {
	if s.Conf.TelegramBot.FeedName != "" {
		return s.Conf.TelegramBot.FeedName
	}
	return "manual"
}

// GET /wegweiser/api/summary — the pulse line: counts across every tab so the
// header can show "12 в ленте · 4 в очереди · пауза" without opening each.
func (s *Server) appSummaryCtrl(w http.ResponseWriter, _ *http.Request) {
	var out struct {
		Feed       int  `json:"feed"`
		Queued     int  `json:"queued"`
		Processing int  `json:"processing"`
		Paused     bool `json:"paused"`
		Notes      int  `json:"notes"`
		Read       int  `json:"read"`
		Refs       int  `json:"refs"`
		Feeds      int  `json:"feeds"`
	}

	if s.YoutubeStore != nil {
		if entries, err := s.YoutubeStore.Load(s.feedName(), 0); err == nil {
			out.Feed = len(entries)
		}
	}
	if s.NotesSvc != nil {
		if q, p, _, err := s.NotesSvc.QueueStatus(); err == nil {
			out.Queued, out.Processing = q, p
		}
		out.Paused = s.NotesSvc.IsPaused()
		if items, err := s.NotesSvc.List(); err == nil {
			out.Notes = len(items)
		}
	}
	if s.ReadSvc != nil {
		if items, err := s.ReadSvc.List(); err == nil {
			out.Read = len(items)
		}
	}
	if s.AppStore != nil {
		if _, total, err := s.AppStore.ReferenceTypeCounts(); err == nil {
			out.Refs = total
		}
	}
	if s.Pub != nil {
		if cats, err := s.Pub.Categories(); err == nil {
			out.Feeds = len(cats)
		}
	}
	rest.RenderJSON(w, out)
}

// appFeedItem is one episode currently in the RSS feed (on disk + in the bucket)
type appFeedItem struct {
	VideoID   string `json:"video_id"`
	Title     string `json:"title"`
	URL       string `json:"url"`       // original source (YouTube/article)
	MediaURL  string `json:"media_url"` // public audio URL for the ⬇ button
	Duration  string `json:"duration"`
	Added     string `json:"added"` // pubDate == time added to the feed
	Thumbnail string `json:"thumbnail,omitempty"`
	HasNotion bool   `json:"has_notion"`
}

// mediaURL builds the public audio URL for an episode file, matching how the
// RSS enclosure is served (youtube.base_url alias, else base_url + /yt/media).
func (s *Server) mediaURL(file string) string {
	if file == "" {
		return ""
	}
	base := s.Conf.YouTube.BaseURL
	if base == "" {
		base = strings.TrimRight(s.Conf.System.BaseURL, "/") + "/yt/media"
	}
	return strings.TrimRight(base, "/") + "/" + filepath.Base(file)
}

// GET /wegweiser/api/feed — what's live in the feed right now (drives ⬇/📓/🗑)
func (s *Server) appFeedCtrl(w http.ResponseWriter, r *http.Request) {
	offset, limit := pageParams(r, 20)
	items := []appFeedItem{}
	total := 0
	if s.YoutubeStore != nil {
		entries, err := s.YoutubeStore.Load(s.feedName(), 0)
		if err != nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to load feed")
			return
		}
		total = len(entries)
		start, end := window(total, offset, limit)
		for _, e := range entries[start:end] {
			dur := e.DurationFmt
			if dur == "" && e.Duration > 0 {
				dur = fmtDuration(e.Duration)
			}
			items = append(items, appFeedItem{
				VideoID:   e.VideoID,
				Title:     e.Title,
				URL:       e.Link.Href,
				MediaURL:  s.mediaURL(e.File),
				Duration:  dur,
				Added:     e.Published.Format(time.RFC3339),
				Thumbnail: e.Media.Thumbnail.URL,
				HasNotion: strings.Contains(string(e.Media.Description), "📓 Конспект:"),
			})
		}
	}
	rest.RenderJSON(w, appPage{Items: items, Total: total, Offset: offset, Limit: limit})
}

// appHistoryItem is one line of the permanent, append-only send log
type appHistoryItem struct {
	Timestamp string `json:"ts"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	Action    string `json:"action"`
	Duration  string `json:"duration,omitempty"`
	Deleted   bool   `json:"deleted"`
	DeletedAt string `json:"deleted_at,omitempty"`
}

// GET /wegweiser/api/history — the never-pruned log (read-only tab)
func (s *Server) appHistoryCtrl(w http.ResponseWriter, r *http.Request) {
	offset, limit := pageParams(r, 20)
	items := []appHistoryItem{}
	total := 0
	if s.AppStore != nil {
		entries, cnt, err := s.AppStore.LoadHistory(s.feedName(), offset, limit)
		if err != nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to load history")
			return
		}
		total = cnt
		for _, e := range entries {
			it := appHistoryItem{
				Timestamp: e.Timestamp.Format(time.RFC3339),
				URL:       e.URL,
				Title:     e.Title,
				Action:    e.Action,
				Duration:  e.Duration,
				Deleted:   e.Deleted,
			}
			if e.Deleted && !e.DeletedAt.IsZero() {
				it.DeletedAt = e.DeletedAt.Format(time.RFC3339)
			}
			items = append(items, it)
		}
	}
	rest.RenderJSON(w, appPage{Items: items, Total: total, Offset: offset, Limit: limit})
}

// appNoteItem is one L1 transcript on disk
type appNoteItem struct {
	SourceID    string   `json:"source_id"`
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	Date        string   `json:"date"`
	DurationMin int      `json:"duration_min"`
	Tags        []string `json:"tags"`
	HasNotion   bool     `json:"has_notion"`
}

// GET /wegweiser/api/notes — the /md transcript catalog (drives ⬇/📓/🗑)
func (s *Server) appNotesCtrl(w http.ResponseWriter, r *http.Request) {
	offset, limit := pageParams(r, 20)
	items := []appNoteItem{}
	total := 0
	if s.NotesSvc != nil {
		list, err := s.NotesSvc.List()
		if err != nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to list notes")
			return
		}
		total = len(list)
		start, end := window(total, offset, limit)
		for _, it := range list[start:end] {
			tags := it.Meta.Tags
			if tags == nil {
				tags = []string{}
			}
			items = append(items, appNoteItem{
				SourceID:    it.SourceID,
				Title:       it.Meta.Title,
				URL:         it.Meta.URL,
				Date:        it.Meta.Date,
				DurationMin: it.Meta.DurationMin,
				Tags:        tags,
				HasNotion:   contains(it.Meta.Processed, "notes"),
			})
		}
	}
	rest.RenderJSON(w, appPage{Items: items, Total: total, Offset: offset, Limit: limit})
}

// appReadItem is one saved article from the reader
type appReadItem struct {
	SourceID   string   `json:"source_id"`
	Title      string   `json:"title"`
	SourceURL  string   `json:"source_url"`
	Site       string   `json:"site,omitempty"`
	Date       string   `json:"date"`
	ReadingMin int      `json:"reading_min"`
	Tags       []string `json:"tags"`
}

// GET /wegweiser/api/read — saved /read articles (drives ⬇/🔗/🗑)
func (s *Server) appReadCtrl(w http.ResponseWriter, r *http.Request) {
	offset, limit := pageParams(r, 20)
	items := []appReadItem{}
	total := 0
	if s.ReadSvc != nil {
		list, err := s.ReadSvc.List()
		if err != nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to list read")
			return
		}
		total = len(list)
		start, end := window(total, offset, limit)
		for _, it := range list[start:end] {
			tags := it.Meta.Tags
			if tags == nil {
				tags = []string{}
			}
			items = append(items, appReadItem{
				SourceID:   it.SourceID,
				Title:      it.Meta.Title,
				SourceURL:  it.Meta.SourceURL,
				Site:       it.Meta.Site,
				Date:       it.Meta.DateAdded,
				ReadingMin: it.Meta.ReadingMin,
				Tags:       tags,
			})
		}
	}
	rest.RenderJSON(w, appPage{Items: items, Total: total, Offset: offset, Limit: limit})
}

type appRefType struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

type appRefMention struct {
	EpisodeTitle string `json:"episode_title"`
	EpisodeURL   string `json:"episode_url,omitempty"`
	NotionURL    string `json:"notion_url,omitempty"`
	Timecode     string `json:"timecode,omitempty"`
	Quote        string `json:"quote,omitempty"`
}

type appRefGroup struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Mentions []appRefMention `json:"mentions"`
}

// GET /wegweiser/api/refs — without ?type= the type summary with counts; with
// ?type=книга the references of that type grouped by name (one book, N episodes).
func (s *Server) appRefsCtrl(w http.ResponseWriter, r *http.Request) {
	typeFilter := r.URL.Query().Get("type")
	if s.AppStore == nil {
		if typeFilter == "" {
			rest.RenderJSON(w, struct {
				Types []appRefType `json:"types"`
				Total int          `json:"total"`
			}{Types: []appRefType{}, Total: 0})
			return
		}
		rest.RenderJSON(w, struct {
			Groups []appRefGroup `json:"groups"`
		}{Groups: []appRefGroup{}})
		return
	}

	if typeFilter == "" {
		counts, total, err := s.AppStore.ReferenceTypeCounts()
		if err != nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to count references")
			return
		}
		types := make([]appRefType, 0, len(counts))
		for t, c := range counts {
			types = append(types, appRefType{Type: t, Count: c})
		}
		sort.Slice(types, func(i, j int) bool {
			if types[i].Count != types[j].Count {
				return types[i].Count > types[j].Count
			}
			return types[i].Type < types[j].Type
		})
		rest.RenderJSON(w, struct {
			Types []appRefType `json:"types"`
			Total int          `json:"total"`
		}{Types: types, Total: total})
		return
	}

	refs, err := s.AppStore.ListReferences(typeFilter)
	if err != nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to list references")
		return
	}
	// group by name, preserving first-seen order
	order := []string{}
	byName := map[string]*appRefGroup{}
	for _, ref := range refs {
		g, ok := byName[ref.Name]
		if !ok {
			g = &appRefGroup{Name: ref.Name, Type: ref.Type}
			byName[ref.Name] = g
			order = append(order, ref.Name)
		}
		g.Mentions = append(g.Mentions, appRefMention{
			EpisodeTitle: ref.EpisodeTitle,
			EpisodeURL:   ref.EpisodeURL,
			NotionURL:    ref.NotionURL,
			Timecode:     ref.Timecode,
			Quote:        ref.Quote,
		})
	}
	groups := make([]appRefGroup, 0, len(order))
	for _, name := range order {
		groups = append(groups, *byName[name])
	}
	rest.RenderJSON(w, struct {
		Groups []appRefGroup `json:"groups"`
	}{Groups: groups})
}

// appJob is one recent notes-queue job for the status tab
type appJob struct {
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	Level   string `json:"level"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	Updated string `json:"updated"`
}

// GET /wegweiser/api/status — notes queue state (drives ⏸/▶)
func (s *Server) appStatusCtrl(w http.ResponseWriter, r *http.Request) {
	var out struct {
		Queued     int      `json:"queued"`
		Processing int      `json:"processing"`
		Paused     bool     `json:"paused"`
		Recent     []appJob `json:"recent"`
	}
	out.Recent = []appJob{}
	if s.NotesSvc != nil {
		q, p, recent, err := s.NotesSvc.QueueStatus()
		if err != nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to read queue")
			return
		}
		out.Queued, out.Processing = q, p
		out.Paused = s.NotesSvc.IsPaused()
		for _, j := range recent {
			out.Recent = append(out.Recent, appJob{
				URL:     j.URL,
				Title:   j.Title,
				Level:   j.Level,
				Status:  j.Status,
				Error:   j.Error,
				Updated: j.UpdatedAt.Format(time.RFC3339),
			})
		}
	}
	rest.RenderJSON(w, out)
}

// appPubFeed is one publishing category (personal audio feed)
type appPubFeed struct {
	Category string `json:"category"`
	Episodes int    `json:"episodes"`
	FeedURL  string `json:"feed_url"`
}

// GET /wegweiser/api/feeds — publishing feeds (drives 🔗)
func (s *Server) appFeedsCtrl(w http.ResponseWriter, r *http.Request) {
	items := []appPubFeed{}
	if s.Pub != nil {
		cats, err := s.Pub.Categories()
		if err != nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusInternalServerError, err, "failed to list feeds")
			return
		}
		for _, c := range cats {
			n := 0
			if eps, lerr := s.Pub.EpisodeList(c); lerr == nil {
				n = len(eps)
			}
			items = append(items, appPubFeed{Category: c, Episodes: n, FeedURL: s.Pub.FeedURL(c)})
		}
	}
	rest.RenderJSON(w, struct {
		Feeds []appPubFeed `json:"feeds"`
	}{Feeds: items})
}

// safeID guards a source id used as a filename against traversal
func safeID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\`) && !strings.Contains(id, "..")
}

// serveMDFile serves a markdown artifact as a download, behind miniAuth. The
// frontend fetches it with the initData header and turns it into a blob, so a
// plain <a download> can't reach it — that's fine, only I ever hit this.
func (s *Server) serveMDFile(w http.ResponseWriter, r *http.Request, dir, id string) {
	if !safeID(id) {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusBadRequest, nil, "bad id")
		return
	}
	path := filepath.Join(dir, id+".md")
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.md"`)
	http.ServeFile(w, r, path)
}

// GET /wegweiser/api/notes/file?id=... — download an L1 transcript .md
func (s *Server) appNotesFileCtrl(w http.ResponseWriter, r *http.Request) {
	if s.NotesSvc == nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable, nil, "notes unavailable")
		return
	}
	s.serveMDFile(w, r, s.NotesSvc.MDLocation, r.URL.Query().Get("id"))
}

// GET /wegweiser/api/read/file?id=... — download a saved article .md
func (s *Server) appReadFileCtrl(w http.ResponseWriter, r *http.Request) {
	if s.ReadSvc == nil {
		rest.SendErrorJSON(w, r, log.Default(), http.StatusServiceUnavailable, nil, "reader unavailable")
		return
	}
	s.serveMDFile(w, r, s.ReadSvc.Location, r.URL.Query().Get("id"))
}

// fmtDuration renders seconds as MM:SS or H:MM:SS
func fmtDuration(sec int) string {
	h := sec / 3600
	m := (sec % 3600) / 60
	sc := sec % 60
	if h > 0 {
		return strconv.Itoa(h) + ":" + pad2(m) + ":" + pad2(sc)
	}
	return strconv.Itoa(m) + ":" + pad2(sc)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

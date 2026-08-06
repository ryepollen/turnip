package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/feed-master/app/config"
	"github.com/umputun/feed-master/app/proc"
	ytfeed "github.com/umputun/feed-master/app/youtube/feed"
	ytstore "github.com/umputun/feed-master/app/youtube/store"
)

// fakeAppStore is an in-memory MiniAppStore for endpoint tests
type fakeAppStore struct {
	history   []ytstore.HistoryEntry
	refCounts map[string]int
	refs      []ytstore.ReferenceEntry
}

func (f *fakeAppStore) LoadHistory(_ string, offset, limit int) ([]ytstore.HistoryEntry, int, error) {
	total := len(f.history)
	start, end := offset, offset+limit
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	return f.history[start:end], total, nil
}
func (f *fakeAppStore) ListReferences(string) ([]ytstore.ReferenceEntry, error) { return f.refs, nil }
func (f *fakeAppStore) ReferenceTypeCounts() (map[string]int, int, error) {
	total := 0
	for _, c := range f.refCounts {
		total += c
	}
	return f.refCounts, total, nil
}
func (f *fakeAppStore) LoadNotesJobs(string, int) ([]ytstore.NotesJobRecord, error) { return nil, nil }
func (f *fakeAppStore) CountNotesJobs(string) (int, error)                          { return 0, nil }
func (f *fakeAppStore) Remove(ytfeed.Entry) error                                   { return nil }
func (f *fakeAppStore) ResetProcessed(ytfeed.Entry) error                           { return nil }
func (f *fakeAppStore) MarkHistoryDeleted(string, string, string) error             { return nil }

// fakeYTStore is an in-memory YoutubeStore for endpoint tests
type fakeYTStore struct{ entries []ytfeed.Entry }

func (f *fakeYTStore) Load(string, int) ([]ytfeed.Entry, error) { return f.entries, nil }

func makeEntry(id, title string, durSec int, notion bool) ytfeed.Entry {
	var e ytfeed.Entry
	e.VideoID = id
	e.Title = title
	e.Link.Href = "https://youtu.be/" + id
	e.Duration = durSec
	e.Published = time.Unix(1_700_000_000, 0)
	e.Media.Thumbnail.URL = "https://i.ytimg.com/" + id + ".jpg"
	if notion {
		e.Media.Description = "desc\n\n📓 Конспект: https://notion.so/x"
	}
	return e
}

// appTestServer builds a Server wired with fakes and a signed-request helper
func appTestServer(t *testing.T) (*Server, func(method, path string, auth bool) *httptest.ResponseRecorder) {
	t.Helper()
	const token = "123456:test-bot-token"
	srv := &Server{
		BotToken:      token,
		AllowedUserID: 5504926420,
		Conf:          config.Conf{},
		YoutubeStore: &fakeYTStore{entries: []ytfeed.Entry{
			makeEntry("aaa", "First", 90, true),
			makeEntry("bbb", "Second", 3725, false),
		}},
		AppStore: &fakeAppStore{
			history: []ytstore.HistoryEntry{
				{Timestamp: time.Unix(1_700_000_100, 0), URL: "https://youtu.be/aaa", Title: "First", Action: "audio", Duration: "1:30"},
				{Timestamp: time.Unix(1_700_000_050, 0), URL: "https://ex.com/a", Title: "Article", Action: "article", Deleted: true, DeletedAt: time.Unix(1_700_000_200, 0)},
			},
			refCounts: map[string]int{"книга": 3, "человек": 1},
			refs: []ytstore.ReferenceEntry{
				{Type: "книга", Name: "Дюна", EpisodeTitle: "Ep1", EpisodeURL: "u1", Timecode: "12:00"},
				{Type: "книга", Name: "Дюна", EpisodeTitle: "Ep2", NotionURL: "n2"},
			},
		},
	}
	handler := srv.router()

	do := func(method, path string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, http.NoBody)
		if auth {
			req.Header.Set(initDataHeader, signInitData(token, map[string]string{
				"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
				"user":      `{"id":5504926420,"first_name":"Pol"}`,
			}))
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	return srv, do
}

func TestMiniAppEndpoints_Unauthorized(t *testing.T) {
	_, do := appTestServer(t)
	for _, p := range []string{"/summary", "/feed", "/history", "/notes", "/read", "/refs", "/status", "/feeds"} {
		t.Run(p, func(t *testing.T) {
			rec := do(http.MethodGet, "/wegweiser/api"+p, false)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

func TestMiniAppSummary(t *testing.T) {
	_, do := appTestServer(t)
	rec := do(http.MethodGet, "/wegweiser/api/summary", true)
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Feed  int `json:"feed"`
		Refs  int `json:"refs"`
		Notes int `json:"notes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, 2, out.Feed)
	assert.Equal(t, 4, out.Refs)  // 3 + 1
	assert.Equal(t, 0, out.Notes) // NotesSvc nil
}

func TestMiniAppFeed(t *testing.T) {
	_, do := appTestServer(t)
	rec := do(http.MethodGet, "/wegweiser/api/feed?limit=1&offset=0", true)
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Items []appFeedItem `json:"items"`
		Total int           `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, 2, out.Total)
	require.Len(t, out.Items, 1)
	assert.Equal(t, "aaa", out.Items[0].VideoID)
	assert.Equal(t, "1:30", out.Items[0].Duration)
	assert.True(t, out.Items[0].HasNotion)

	// second page: the long-form duration formats as H:MM:SS
	rec2 := do(http.MethodGet, "/wegweiser/api/feed?limit=1&offset=1", true)
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &out))
	require.Len(t, out.Items, 1)
	assert.Equal(t, "bbb", out.Items[0].VideoID)
	assert.Equal(t, "1:02:05", out.Items[0].Duration)
	assert.False(t, out.Items[0].HasNotion)
}

func TestMiniAppHistory(t *testing.T) {
	_, do := appTestServer(t)
	rec := do(http.MethodGet, "/wegweiser/api/history", true)
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Items []appHistoryItem `json:"items"`
		Total int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, 2, out.Total)
	require.Len(t, out.Items, 2)
	assert.True(t, out.Items[1].Deleted)
	assert.NotEmpty(t, out.Items[1].DeletedAt)
}

func TestMiniAppRefs(t *testing.T) {
	_, do := appTestServer(t)

	// summary: types sorted by count desc
	rec := do(http.MethodGet, "/wegweiser/api/refs", true)
	require.Equal(t, http.StatusOK, rec.Code)
	var summary struct {
		Types []appRefType `json:"types"`
		Total int          `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &summary))
	assert.Equal(t, 4, summary.Total)
	require.Len(t, summary.Types, 2)
	assert.Equal(t, "книга", summary.Types[0].Type)
	assert.Equal(t, 3, summary.Types[0].Count)

	// by type: grouped by name, two mentions of one book
	rec2 := do(http.MethodGet, "/wegweiser/api/refs?type=%D0%BA%D0%BD%D0%B8%D0%B3%D0%B0", true)
	require.Equal(t, http.StatusOK, rec2.Code)
	var byType struct {
		Groups []appRefGroup `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &byType))
	require.Len(t, byType.Groups, 1)
	assert.Equal(t, "Дюна", byType.Groups[0].Name)
	assert.Len(t, byType.Groups[0].Mentions, 2)
}

// fakeJobStore is an in-memory NotesJobStore so the API layer can exercise
// EnqueueURL / SetPaused without bolt
type fakeJobStore struct {
	saved  []ytstore.NotesJobRecord
	paused bool
	active bool
}

func (f *fakeJobStore) SaveNotesJob(j ytstore.NotesJobRecord) error {
	f.saved = append(f.saved, j)
	return nil
}
func (f *fakeJobStore) ClaimNextNotesJob() (ytstore.NotesJobRecord, bool, error) {
	return ytstore.NotesJobRecord{}, false, nil
}
func (f *fakeJobStore) LoadNotesJobs(string, int) ([]ytstore.NotesJobRecord, error) {
	return f.saved, nil
}
func (f *fakeJobStore) CountNotesJobs(string) (int, error)        { return 0, nil }
func (f *fakeJobStore) HasActiveNotesJob(string) (bool, error)    { return f.active, nil }
func (f *fakeJobStore) ResetProcessingNotesJobs() (int, error)    { return 0, nil }
func (f *fakeJobStore) DeleteOldNotesJobs(time.Time) (int, error) { return 0, nil }
func (f *fakeJobStore) SetNotesPaused(p bool) error               { f.paused = p; return nil }
func (f *fakeJobStore) NotesPaused() (bool, error)                { return f.paused, nil }

func TestMiniAppActions(t *testing.T) {
	const token = "123456:test-bot-token"
	js := &fakeJobStore{}
	deleted := ""
	srv := &Server{
		BotToken:      token,
		AllowedUserID: 5504926420,
		Conf:          config.Conf{},
		YoutubeStore:  &fakeYTStore{entries: []ytfeed.Entry{makeEntry("aaa", "First", 90, false)}},
		NotesSvc:      &proc.NotesService{JobStore: js},
		DeleteFeedEntry: func(e ytfeed.Entry) error {
			deleted = e.VideoID
			return nil
		},
	}
	handler := srv.router()

	post := func(path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		req.Header.Set(initDataHeader, signInitData(token, map[string]string{
			"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
			"user":      `{"id":5504926420,"first_name":"Pol"}`,
		}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	t.Run("feed delete ok", func(t *testing.T) {
		rec := post("/wegweiser/api/feed/delete", map[string]string{"video_id": "aaa"})
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "aaa", deleted)
	})

	t.Run("feed delete unknown -> 404", func(t *testing.T) {
		rec := post("/wegweiser/api/feed/delete", map[string]string{"video_id": "zzz"})
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("feed delete no video_id -> 400", func(t *testing.T) {
		rec := post("/wegweiser/api/feed/delete", map[string]string{})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("notes enqueue ok", func(t *testing.T) {
		rec := post("/wegweiser/api/notes/enqueue", map[string]string{"url": "https://youtu.be/aaa"})
		require.Equal(t, http.StatusOK, rec.Code)
		require.Len(t, js.saved, 1)
		assert.Equal(t, "notes", js.saved[0].Level)
		assert.Equal(t, "aaa", js.saved[0].SourceID)
		assert.Equal(t, 1, js.saved[0].Priority)
	})

	t.Run("notes enqueue duplicate -> 409", func(t *testing.T) {
		js.active = true
		defer func() { js.active = false }()
		rec := post("/wegweiser/api/notes/enqueue", map[string]string{"url": "https://youtu.be/bbb"})
		assert.Equal(t, http.StatusConflict, rec.Code)
	})

	t.Run("pause then resume", func(t *testing.T) {
		rec := post("/wegweiser/api/queue/pause", map[string]bool{"paused": true})
		require.Equal(t, http.StatusOK, rec.Code)
		assert.True(t, js.paused)
		rec2 := post("/wegweiser/api/queue/pause", map[string]bool{"paused": false})
		require.Equal(t, http.StatusOK, rec2.Code)
		assert.False(t, js.paused)
	})
}

func TestMiniAppShell(t *testing.T) {
	srv := &Server{Conf: config.Conf{}}
	handler := srv.router()

	// shell is public (no initData) — it carries no data
	req := httptest.NewRequest(http.MethodGet, "/wegweiser", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "WEGWEISER")
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")

	// static assets serve from the embedded FS
	for _, f := range []string{"app.js", "app.css"} {
		reqS := httptest.NewRequest(http.MethodGet, "/wegweiser/static/"+f, http.NoBody)
		recS := httptest.NewRecorder()
		handler.ServeHTTP(recS, reqS)
		assert.Equal(t, http.StatusOK, recS.Code, f)
	}
}

func TestMiniAppNilServices(t *testing.T) {
	// notes/read/feeds/status must return 200 with empty payloads when the
	// backing services are not configured
	_, do := appTestServer(t)
	for _, p := range []string{"/notes", "/read", "/feeds", "/status"} {
		t.Run(p, func(t *testing.T) {
			rec := do(http.MethodGet, "/wegweiser/api"+p, true)
			assert.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

package publisher

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeR2 is a stateful in-memory S3 endpoint: it remembers objects across
// PUT/HEAD/DELETE so Activate's Exists-skip and old-key eviction can be tested.
type fakeR2 struct {
	mu      sync.Mutex
	objects map[string]int64 // request path (/bucket/key) → size
	puts    int
	deletes int
}

func (f *fakeR2) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			f.objects[r.URL.Path] = int64(len(body))
			f.puts++
			w.Header().Set("ETag", `"e"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			if size, ok := f.objects[r.URL.Path]; ok {
				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
				w.Header().Set("ETag", `"e"`)
				w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case http.MethodDelete:
			delete(f.objects, r.URL.Path)
			f.deletes++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}
}

func (f *fakeR2) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects["/turnip/"+key]
	return ok
}

// newFakeR2Service wires a Service to a fresh stateful fake R2 endpoint.
func newFakeR2Service(t *testing.T) (*Service, *fakeR2) {
	t.Helper()
	fake := &fakeR2{objects: map[string]int64{}}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	cfg := R2Config{AccountID: "acc", AccessKeyID: "k", SecretKey: "s", Bucket: "turnip", PublicBaseURL: "https://pub.example"}
	r2, err := newR2StoreForEndpoint(strings.TrimPrefix(srv.URL, "http://"), cfg)
	require.NoError(t, err)

	svc := &Service{R2: r2, AudioDir: t.TempDir(), Secret: "s3cr3t", Duration: fakeDuration{}, BaseURL: "http://vm:8080"}
	return svc, fake
}

// writeWork lays out a work folder with the given relative part files under a
// category (books → normalize off, so no ffmpeg needed in tests).
func writeWork(t *testing.T, svc *Service, category, work string, parts ...string) {
	t.Helper()
	for _, rel := range parts {
		p := filepath.Join(svc.categoryDir(category), work, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte("audio-"+rel), 0o600))
	}
}

// spyDuration records which duration path was taken so a test can prove Finalize
// reads from R2 (URL) and never touches the originals' bytes (File).
type spyDuration struct {
	fileCalls int
	urlCalls  int
}

func (s *spyDuration) File(string) int { s.fileCalls++; return 42 }
func (s *spyDuration) URL(string) int  { s.urlCalls++; return 90 }

// TestFinalizeReadsR2NotOriginals is the invariant that makes variant-A stubbing
// safe: once a work's parts are in R2, Finalize rebuilds the episode list from R2
// alone — even if the local originals are blanked to 0-byte manifest stubs.
func TestFinalizeReadsR2NotOriginals(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	spy := &spyDuration{}
	svc.Duration = spy
	writeWork(t, svc, "books", "Work", "01 - One.mp3", "02 - Two.mp3")

	// model the Mac agent having uploaded the parts by running the in-process
	// Activate once to populate R2 + state (this path legitimately probes local files)
	require.NoError(t, svc.Activate(t.Context(), "books", "Work", nil))
	require.Equal(t, 2, spy.fileCalls, "in-process Activate probes local files")

	// variant A after stubbing: blank the originals to 0 bytes, keeping only names
	workDir := filepath.Join(svc.categoryDir("books"), "Work")
	for _, rel := range []string{"01 - One.mp3", "02 - Two.mp3"} {
		require.NoError(t, os.WriteFile(filepath.Join(workDir, rel), nil, 0o600))
	}
	spy.fileCalls, spy.urlCalls = 0, 0

	// Finalize (the remote-activation VM half) must rebuild purely from R2
	require.NoError(t, svc.Finalize(t.Context(), "books", "Work"))
	assert.Zero(t, spy.fileCalls, "Finalize must not read the originals' bytes")
	assert.Equal(t, 2, spy.urlCalls, "Finalize reads duration from the R2 URL")

	eps, err := svc.EpisodeList("books")
	require.NoError(t, err)
	require.Len(t, eps, 2)
	for _, e := range eps {
		assert.Equal(t, 90, e.DurationSec, "duration comes from the R2 URL probe, not local ffprobe")
		assert.NotZero(t, e.SizeBytes, "size still resolved via R2 HEAD")
	}
	assert.True(t, fake.has("a/s3cr3t/books/Work/01 - One.mp3"))
}

func TestServiceActivateAndRegenerate(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "books", "TheBook",
		"02 - Глава вторая.mp3", "01 - Глава первая.mp3", "10 - Глава десятая.mp3")

	var lastDone, lastTotal int
	require.NoError(t, svc.Activate(t.Context(), "books", "TheBook", func(done, total int) {
		lastDone, lastTotal = done, total
	}))
	assert.Equal(t, 3, fake.puts, "each part uploaded once")
	assert.Equal(t, 3, lastDone)
	assert.Equal(t, 3, lastTotal)

	active, err := svc.ActiveWork("books")
	require.NoError(t, err)
	assert.Equal(t, "TheBook", active)

	eps, err := svc.EpisodeList("books")
	require.NoError(t, err)
	require.Len(t, eps, 3)
	assert.Equal(t, "Глава первая", eps[0].Title, "sorted by track number")
	assert.Equal(t, "Глава десятая", eps[2].Title)
	assert.Equal(t, "a/s3cr3t/books/TheBook/01 - Глава первая.mp3", eps[0].R2Key)
	assert.True(t, fake.has("a/s3cr3t/books/TheBook/01 - Глава первая.mp3"))

	// feed lists the work's parts in order
	feedData, err := os.ReadFile(filepath.Join(svc.AudioDir, "feeds", "books.xml"))
	require.NoError(t, err)
	feed := string(feedData)
	assert.Less(t, strings.Index(feed, "Глава первая"), strings.Index(feed, "Глава десятая"))

	// idempotent re-activate: nothing new uploaded (Exists skip)
	fake.puts = 0
	require.NoError(t, svc.Activate(t.Context(), "books", "TheBook", nil))
	assert.Equal(t, 0, fake.puts, "already-uploaded parts are not re-sent")
}

func TestActivateEvictsPrevious(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "books", "First", "01 - One.mp3", "02 - Two.mp3")
	writeWork(t, svc, "books", "Second", "01 - Alpha.mp3")

	require.NoError(t, svc.Activate(t.Context(), "books", "First", nil))
	require.True(t, fake.has("a/s3cr3t/books/First/01 - One.mp3"))

	fake.deletes = 0
	require.NoError(t, svc.Activate(t.Context(), "books", "Second", nil))

	active, _ := svc.ActiveWork("books")
	assert.Equal(t, "Second", active)
	assert.True(t, fake.has("a/s3cr3t/books/Second/01 - Alpha.mp3"), "new work uploaded")
	assert.False(t, fake.has("a/s3cr3t/books/First/01 - One.mp3"), "old work evicted from R2")
	assert.False(t, fake.has("a/s3cr3t/books/First/02 - Two.mp3"))
	assert.Equal(t, 2, fake.deletes, "both old parts deleted")

	eps, _ := svc.EpisodeList("books")
	require.Len(t, eps, 1)
	assert.Equal(t, "Alpha", eps[0].Title)
}

func TestDeactivate(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "books", "Work", "01 - One.mp3", "02 - Two.mp3")
	require.NoError(t, svc.Activate(t.Context(), "books", "Work", nil))

	require.NoError(t, svc.Deactivate(t.Context(), "books"))
	assert.False(t, fake.has("a/s3cr3t/books/Work/01 - One.mp3"), "R2 freed")
	assert.False(t, fake.has("a/s3cr3t/books/Work/02 - Two.mp3"))

	active, _ := svc.ActiveWork("books")
	assert.Equal(t, "", active)

	eps, _ := svc.EpisodeList("books")
	assert.Empty(t, eps)

	// empty feed is still valid XML with the channel and no items
	feedData, err := os.ReadFile(filepath.Join(svc.AudioDir, "feeds", "books.xml"))
	require.NoError(t, err)
	feed := string(feedData)
	assert.Contains(t, feed, "<channel>")
	assert.NotContains(t, feed, "<item>")
}

func TestCatalogScan(t *testing.T) {
	svc, _ := newFakeR2Service(t)

	// a flat work
	writeWork(t, svc, "books", "FlatBook", "01 - A.mp3", "02 - B.mp3")
	// a seasoned work
	writeWork(t, svc, "podcasts", "BigShow",
		"Season 01/01 - E1.mp3", "Season 01/02 - E2.mp3", "Season 02/01 - E1.mp3")
	// a work.yaml title + streaming type
	writeWork(t, svc, "courses", "Deep", "01 - Lesson.mp3")
	require.NoError(t, os.WriteFile(
		filepath.Join(svc.categoryDir("courses"), "Deep", "work.yaml"),
		[]byte("title: Глубокий курс\ntype: streaming"), 0o600))
	// junk that must not become a work
	require.NoError(t, os.MkdirAll(filepath.Join(svc.categoryDir("books"), "EmptyDir"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(svc.categoryDir("books"), "feed.yaml"), []byte("title: Книги"), 0o600))

	cat, err := svc.Catalog()
	require.NoError(t, err)

	byslug := map[string]CatWork{}
	for _, w := range cat {
		byslug[w.Category+"/"+w.Slug] = w
	}
	require.Contains(t, byslug, "books/FlatBook")
	assert.Equal(t, 2, byslug["books/FlatBook"].Parts)
	assert.Equal(t, 0, byslug["books/FlatBook"].Seasons)
	assert.True(t, byslug["books/FlatBook"].Finite)

	require.Contains(t, byslug, "podcasts/BigShow")
	assert.Equal(t, 3, byslug["podcasts/BigShow"].Parts)
	assert.Equal(t, 2, byslug["podcasts/BigShow"].Seasons)

	require.Contains(t, byslug, "courses/Deep")
	assert.Equal(t, "Глубокий курс", byslug["courses/Deep"].Title)
	assert.False(t, byslug["courses/Deep"].Finite, "streaming work")

	_, junk := byslug["books/EmptyDir"]
	assert.False(t, junk, "empty dir is not a work")

	// activation is reflected in the catalog
	require.NoError(t, svc.Activate(t.Context(), "books", "FlatBook", nil))
	cat, _ = svc.Catalog()
	for _, w := range cat {
		if w.Category == "books" && w.Slug == "FlatBook" {
			assert.True(t, w.Active)
		}
	}
}

func TestActivateBadNames(t *testing.T) {
	svc, _ := newFakeR2Service(t)
	require.Error(t, svc.Activate(t.Context(), "../evil", "Work", nil))
	require.Error(t, svc.Activate(t.Context(), "books", "../evil", nil))
	require.Error(t, svc.Activate(t.Context(), "books", "does-not-exist", nil))
}

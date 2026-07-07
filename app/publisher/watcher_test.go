package publisher

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newWatcherTestService(t *testing.T) (*Service, *int) {
	t.Helper()
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
			w.Header().Set("ETag", `"e"`)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := R2Config{AccountID: "acc", AccessKeyID: "k", SecretKey: "s", Bucket: "turnip", PublicBaseURL: "https://pub.example"}
	r2, err := newR2StoreForEndpoint(strings.TrimPrefix(srv.URL, "http://"), cfg)
	require.NoError(t, err)

	svc := &Service{R2: r2, AudioDir: t.TempDir(), Secret: "sec", Duration: fakeDuration{}, BaseURL: "http://vm:8080"}
	return svc, &puts
}

func TestWatcherStabilityAndPublish(t *testing.T) {
	svc, puts := newWatcherTestService(t)

	catDir := filepath.Join(svc.AudioDir, "originals", "books")
	require.NoError(t, os.MkdirAll(catDir, 0o750))
	// normalization off so the test doesn't need ffmpeg
	require.NoError(t, os.WriteFile(filepath.Join(catDir, "feed.yaml"), []byte("normalize: false"), 0o600))
	path := filepath.Join(catDir, "01 - Глава.mp3")
	require.NoError(t, os.WriteFile(path, []byte("audio-bytes"), 0o600))

	var notices []string
	notify := func(s string) { notices = append(notices, s) }
	seen, cooldown := map[string]*seenFile{}, map[string]time.Time{}

	// scan 1: first sight — not published yet
	svc.scanOnce(t.Context(), seen, cooldown, notify)
	assert.Equal(t, 0, *puts, "first scan only records the file")

	// scan 2: stable → published
	svc.scanOnce(t.Context(), seen, cooldown, notify)
	assert.Equal(t, 1, *puts, "stable file published")
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], "Опубликовано")
	assert.Contains(t, notices[0], "/pod/sec/books.xml")

	// scan 3: already in state → no re-publish
	svc.scanOnce(t.Context(), seen, cooldown, notify)
	assert.Equal(t, 1, *puts, "published files are skipped")

	// feed exists
	_, err := os.Stat(filepath.Join(svc.AudioDir, "feeds", "books.xml"))
	require.NoError(t, err)
}

func TestWatcherWaitsWhileFileGrows(t *testing.T) {
	svc, puts := newWatcherTestService(t)

	catDir := filepath.Join(svc.AudioDir, "originals", "books")
	require.NoError(t, os.MkdirAll(catDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(catDir, "feed.yaml"), []byte("normalize: false"), 0o600))
	path := filepath.Join(catDir, "big.mp3")
	require.NoError(t, os.WriteFile(path, []byte("part1"), 0o600))

	seen, cooldown := map[string]*seenFile{}, map[string]time.Time{}
	svc.scanOnce(t.Context(), seen, cooldown, nil)

	// file keeps growing between scans — must not be published
	require.NoError(t, os.WriteFile(path, []byte("part1part2"), 0o600))
	svc.scanOnce(t.Context(), seen, cooldown, nil)
	assert.Equal(t, 0, *puts, "growing file must wait")

	// now stable for two scans
	svc.scanOnce(t.Context(), seen, cooldown, nil)
	svc.scanOnce(t.Context(), seen, cooldown, nil)
	assert.Equal(t, 1, *puts)
}

func TestWatcherIgnoresJunk(t *testing.T) {
	svc, puts := newWatcherTestService(t)

	catDir := filepath.Join(svc.AudioDir, "originals", "books")
	require.NoError(t, os.MkdirAll(filepath.Join(catDir, "subdir"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(catDir, "feed.yaml"), []byte("title: x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(catDir, "cover.jpg"), []byte("img"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(catDir, "notes.txt"), []byte("txt"), 0o600))

	seen, cooldown := map[string]*seenFile{}, map[string]time.Time{}
	svc.scanOnce(t.Context(), seen, cooldown, nil)
	svc.scanOnce(t.Context(), seen, cooldown, nil)
	svc.scanOnce(t.Context(), seen, cooldown, nil)
	assert.Equal(t, 0, *puts, "non-audio files ignored")
	assert.Empty(t, seen)
}

func TestWatcherCooldownAfterFailure(t *testing.T) {
	svc, puts := newWatcherTestService(t)

	catDir := filepath.Join(svc.AudioDir, "originals", "books")
	require.NoError(t, os.MkdirAll(catDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(catDir, "feed.yaml"), []byte("normalize: false"), 0o600))
	path := filepath.Join(catDir, "bad.mp3")
	require.NoError(t, os.WriteFile(path, []byte("audio"), 0o000)) // unreadable → publish fails

	var notices []string
	notify := func(s string) { notices = append(notices, s) }
	seen, cooldown := map[string]*seenFile{}, map[string]time.Time{}

	svc.scanOnce(t.Context(), seen, cooldown, notify)
	svc.scanOnce(t.Context(), seen, cooldown, notify) // stable → attempt → fail
	require.Len(t, notices, 1, "one failure notice")
	assert.Contains(t, notices[0], "❌")
	require.Contains(t, cooldown, path)

	// scans during cooldown must not retry or re-notify
	svc.scanOnce(t.Context(), seen, cooldown, notify)
	svc.scanOnce(t.Context(), seen, cooldown, notify)
	assert.Len(t, notices, 1, "no notification spam during cooldown")
	assert.Equal(t, 0, *puts)

	// fixing the file changes mtime/permissions → user re-touches → fresh chance
	require.NoError(t, os.Chmod(path, 0o600))
	require.NoError(t, os.WriteFile(path, []byte("audio-fixed"), 0o600))
	svc.scanOnce(t.Context(), seen, cooldown, notify) // sees change, resets
	svc.scanOnce(t.Context(), seen, cooldown, notify) // stable → publish
	assert.Equal(t, 1, *puts, "published after the fix")
	assert.Contains(t, notices[len(notices)-1], "Опубликовано")
}

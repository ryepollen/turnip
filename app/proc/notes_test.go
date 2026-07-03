package proc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"

	ytstore "github.com/umputun/feed-master/app/youtube/store"
)

// newTestJobStore opens a temp bolt-backed store for queue tests
func newTestJobStore(t *testing.T) *ytstore.BoltDB {
	dbFile := filepath.Join(t.TempDir(), "test-notes.db")
	db, err := bolt.Open(dbFile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &ytstore.BoltDB{DB: db}
}

// captureNotifier records lifecycle events for assertions
type captureNotifier struct {
	stages []string
	done   chan NotesResult
	failed chan error
}

func newCaptureNotifier() *captureNotifier {
	return &captureNotifier{done: make(chan NotesResult, 8), failed: make(chan error, 8)}
}

func (c *captureNotifier) NotesJobProgress(_ ytstore.NotesJobRecord, stage string) {
	c.stages = append(c.stages, stage)
}
func (c *captureNotifier) NotesJobDone(_ ytstore.NotesJobRecord, res NotesResult) { c.done <- res }
func (c *captureNotifier) NotesJobFailed(_ ytstore.NotesJobRecord, err error)     { c.failed <- err }

func TestNoteFileRoundtrip(t *testing.T) {
	tests := []struct {
		name string
		meta NoteMeta
		body string
	}{
		{
			"full meta",
			NoteMeta{Title: "Эпизод про «дизайн»", Source: "youtube", URL: "https://youtu.be/abc",
				Channel: "Канал", Date: "2026-06-15", DurationMin: 94, Lang: "ru",
				Tags: []string{"product-design", "history"}, Processed: []string{"md", "notes"}},
			"[00:00] Первый блок...\n\n[04:12] Следующий блок...",
		},
		{
			"empty tags and unicode body",
			NoteMeta{Title: "טיטל: yiddish — тест", Source: "article", URL: "https://example.com/a", Date: "2026-01-01", Lang: "en"},
			"Просто текст без таймкодов.\nВторая строка.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "note.md")
			require.NoError(t, writeNoteFile(path, tt.meta, tt.body))

			meta, body, err := readNoteFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.body, body)
			assert.Equal(t, tt.meta.Title, meta.Title)
			assert.Equal(t, tt.meta.URL, meta.URL)
			assert.Equal(t, tt.meta.DurationMin, meta.DurationMin)
			if tt.meta.Tags == nil {
				assert.Empty(t, meta.Tags)
			} else {
				assert.Equal(t, tt.meta.Tags, meta.Tags)
			}
		})
	}
}

func TestReadNoteFileErrors(t *testing.T) {
	dir := t.TempDir()

	_, _, err := readNoteFile(filepath.Join(dir, "missing.md"))
	assert.Error(t, err)

	noFM := filepath.Join(dir, "nofm.md")
	require.NoError(t, os.WriteFile(noFM, []byte("just text"), 0o600))
	_, _, err = readNoteFile(noFM)
	assert.ErrorContains(t, err, "no frontmatter")

	unterminated := filepath.Join(dir, "unterminated.md")
	require.NoError(t, os.WriteFile(unterminated, []byte("---\ntitle: x\nno closing"), 0o600))
	_, _, err = readNoteFile(unterminated)
	assert.ErrorContains(t, err, "unterminated")
}

func TestNoteSourceID(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		wantID     string
		wantSource string
	}{
		{"standard watch", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ", "youtube"},
		{"short link", "https://youtu.be/dQw4w9WgXcQ?t=10", "dQw4w9WgXcQ", "youtube"},
		{"mobile", "https://m.youtube.com/watch?v=dQw4w9WgXcQ&list=x", "dQw4w9WgXcQ", "youtube"},
		{"article", "https://example.com/some-post", "", "article"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, source := noteSourceID(tt.url)
			assert.Equal(t, tt.wantSource, source)
			if tt.wantID != "" {
				assert.Equal(t, tt.wantID, id)
			} else {
				assert.Len(t, id, 16, "url hash is 16 hex chars")
			}
		})
	}

	// hash must be stable
	id1, _ := noteSourceID("https://example.com/some-post")
	id2, _ := noteSourceID("https://example.com/some-post")
	assert.Equal(t, id1, id2)
}

func TestNoteMetaProcessed(t *testing.T) {
	m := NoteMeta{}
	assert.False(t, m.hasProcessed("md"))
	m.addProcessed("md")
	m.addProcessed("md")
	assert.Equal(t, []string{"md"}, m.Processed)
	m.addProcessed("notes")
	assert.True(t, m.hasProcessed("notes"))
}

func TestSanitizeFileName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean", "Обычное название", "Обычное название"},
		{"forbidden chars", `Что/такое: "дизайн"? <и> прочее|*\`, "Что такое дизайн и прочее"},
		{"newlines and tabs", "строка\nвторая\tтретья", "строка вторая третья"},
		{"empty", "", "transcript"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitizeFileName(tt.in))
		})
	}

	long := sanitizeFileName(string(make([]rune, 0, 0)) + repeatRunes('я', 150))
	assert.Len(t, []rune(long), 100, "capped at 100 runes")
}

func repeatRunes(r rune, n int) string {
	runes := make([]rune, n)
	for i := range runes {
		runes[i] = r
	}
	return string(runes)
}

func TestNoteCaption(t *testing.T) {
	res := NotesResult{
		Meta:      NoteMeta{DurationMin: 94, Tags: []string{"design", "history"}},
		WordCount: 12340,
	}
	assert.Equal(t, "⏱ 1ч 34м · 12340 слов · 🏷 design, history", noteCaption(res))

	short := NotesResult{Meta: NoteMeta{DurationMin: 42}}
	assert.Equal(t, "⏱ 42м", noteCaption(short))

	assert.Equal(t, "", noteCaption(NotesResult{}))
}

func TestNotesEnqueueDedupAndOverflow(t *testing.T) {
	store := newTestJobStore(t)
	svc := NewNotesService(NotesParams{MDLocation: t.TempDir(), Concurrency: 1, JobStore: store})

	// worker not running: jobs stay queued in the store
	require.NoError(t, svc.Enqueue(ytstore.NotesJobRecord{SourceID: "vid1", Level: "md", URL: "u1"}))
	assert.ErrorIs(t, svc.Enqueue(ytstore.NotesJobRecord{SourceID: "vid1", Level: "md", URL: "u1"}), errAlreadyQueued)

	for i := 0; i < maxQueuedNotesJobs-1; i++ {
		require.NoError(t, svc.Enqueue(ytstore.NotesJobRecord{SourceID: fmt.Sprintf("vid-%03d", i), Level: "md"}))
	}
	assert.ErrorIs(t, svc.Enqueue(ytstore.NotesJobRecord{SourceID: "overflow", Level: "md"}), errQueueFull)

	queued, err := store.CountNotesJobs(ytstore.NotesJobQueued)
	require.NoError(t, err)
	assert.Equal(t, maxQueuedNotesJobs, queued, "rejected job must not be stored")
}

func TestNotesProcessReusesL1(t *testing.T) {
	dir := t.TempDir()
	svc := NewNotesService(NotesParams{MDLocation: dir, Concurrency: 1})
	notifier := newCaptureNotifier()
	svc.Notifier = notifier

	meta := NoteMeta{Title: "Готовый", Source: "youtube", URL: "https://youtu.be/reuse123",
		Date: "2026-06-01", Lang: "ru", Processed: []string{"md"}}
	require.NoError(t, writeNoteFile(filepath.Join(dir, "reuse123.md"), meta, "[00:00] тело"))

	res, err := svc.process(context.Background(), ytstore.NotesJobRecord{
		URL: "https://youtu.be/reuse123", SourceID: "reuse123", Source: "youtube", Level: "md",
	})
	require.NoError(t, err)
	assert.True(t, res.Reused)
	assert.Equal(t, "Готовый", res.Title)
	assert.Contains(t, notifier.stages, "♻️ найден готовый транскрипт")
}

func TestNotesProcessNotesWithoutNotion(t *testing.T) {
	dir := t.TempDir()
	svc := NewNotesService(NotesParams{MDLocation: dir, Concurrency: 1})

	meta := NoteMeta{Title: "x", Source: "youtube", URL: "u", Date: "2026-06-01", Lang: "ru"}
	require.NoError(t, writeNoteFile(filepath.Join(dir, "vid42.md"), meta, "тело"))

	_, err := svc.process(context.Background(), ytstore.NotesJobRecord{
		URL: "u", SourceID: "vid42", Source: "youtube", Level: "notes",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notion не настроен")
}

func TestNotesWorkerDrainsQueue(t *testing.T) {
	dir := t.TempDir()
	store := newTestJobStore(t)
	svc := NewNotesService(NotesParams{MDLocation: dir, Concurrency: 1, JobStore: store})
	notifier := newCaptureNotifier()
	svc.Notifier = notifier

	// pre-made L1 file: the job completes without any network calls
	meta := NoteMeta{Title: "Из очереди", Source: "youtube", URL: "https://youtu.be/queued01",
		Date: "2026-06-01", Lang: "ru", Processed: []string{"md"}}
	require.NoError(t, writeNoteFile(filepath.Join(dir, "queued01.md"), meta, "[00:00] тело"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	require.NoError(t, svc.Enqueue(ytstore.NotesJobRecord{
		URL: "https://youtu.be/queued01", SourceID: "queued01", Source: "youtube", Level: "md",
	}))

	select {
	case res := <-notifier.done:
		assert.True(t, res.Reused)
		assert.Equal(t, "Из очереди", res.Title)
	case err := <-notifier.failed:
		t.Fatalf("job failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("job was not processed in time")
	}

	// the stored record must be marked done
	require.Eventually(t, func() bool {
		jobs, err := store.LoadNotesJobs(ytstore.NotesJobDone, 1)
		return err == nil && len(jobs) == 1 && jobs[0].SourceID == "queued01"
	}, 5*time.Second, 100*time.Millisecond)
}

func TestNotesRunRequeuesInterrupted(t *testing.T) {
	store := newTestJobStore(t)

	// simulate a job that was processing when the previous run died
	stuck := ytstore.NotesJobRecord{
		ID: "00000000000000000001-stuck1", SourceID: "stuck1", URL: "u",
		Source: "youtube", Level: "md", Status: ytstore.NotesJobProcessing,
	}
	require.NoError(t, store.SaveNotesJob(stuck))

	count, err := store.ResetProcessingNotesJobs()
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	job, ok, err := store.ClaimNextNotesJob()
	require.NoError(t, err)
	require.True(t, ok, "requeued job must be claimable")
	assert.Equal(t, "stuck1", job.SourceID)
	assert.Equal(t, ytstore.NotesJobProcessing, job.Status)
}

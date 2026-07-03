package proc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	svc := NewNotesService(NotesParams{MDLocation: t.TempDir(), Concurrency: 1})

	// worker not running: jobs stay queued
	require.NoError(t, svc.Enqueue(NotesJob{SourceID: "vid1", Level: "md"}))
	assert.ErrorIs(t, svc.Enqueue(NotesJob{SourceID: "vid1", Level: "md"}), errAlreadyQueued)

	for i := 0; i < notesQueueSize-1; i++ {
		require.NoError(t, svc.Enqueue(NotesJob{SourceID: string(rune('a' + i)), Level: "md"}))
	}
	assert.ErrorIs(t, svc.Enqueue(NotesJob{SourceID: "overflow", Level: "md"}), errQueueFull)

	// rejected overflow job must not stay in the inflight set
	svc.mu.Lock()
	_, stillThere := svc.inflight["overflow"]
	svc.mu.Unlock()
	assert.False(t, stillThere)
}

func TestNotesProcessReusesL1(t *testing.T) {
	dir := t.TempDir()
	svc := NewNotesService(NotesParams{MDLocation: dir, Concurrency: 1})

	meta := NoteMeta{Title: "Готовый", Source: "youtube", URL: "https://youtu.be/reuse123",
		Date: "2026-06-01", Lang: "ru", Processed: []string{"md"}}
	require.NoError(t, writeNoteFile(filepath.Join(dir, "reuse123.md"), meta, "[00:00] тело"))

	var stages []string
	res, err := svc.process(context.Background(), NotesJob{
		URL: "https://youtu.be/reuse123", SourceID: "reuse123", Source: "youtube", Level: "md",
		Progress: func(s string) { stages = append(stages, s) },
	})
	require.NoError(t, err)
	assert.True(t, res.Reused)
	assert.Equal(t, "Готовый", res.Title)
	assert.Contains(t, stages, "♻️ найден готовый транскрипт")
}

func TestNotesProcessNotesWithoutNotion(t *testing.T) {
	dir := t.TempDir()
	svc := NewNotesService(NotesParams{MDLocation: dir, Concurrency: 1})

	meta := NoteMeta{Title: "x", Source: "youtube", URL: "u", Date: "2026-06-01", Lang: "ru"}
	require.NoError(t, writeNoteFile(filepath.Join(dir, "vid42.md"), meta, "тело"))

	_, err := svc.process(context.Background(), NotesJob{
		URL: "u", SourceID: "vid42", Source: "youtube", Level: "notes",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notion не настроен")
}

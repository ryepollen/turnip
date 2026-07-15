//go:build integration

// Integration tests for the notes pipeline, hitting real Groq and Notion APIs.
// Run selectively with env keys:
//
//	GROQ_API_KEY=... go test -tags integration ./app/proc -run TestIntegration -v
//	NOTION_TOKEN=... NOTION_TEST_PARENT=<page-id> go test -tags integration ./app/proc -run TestIntegrationNotion -v
package proc

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/feed-master/app/duration"
)

func TestIntegrationTranscribe(t *testing.T) {
	key := os.Getenv("GROQ_API_KEY")
	if key == "" {
		t.Skip("GROQ_API_KEY not set")
	}

	svc := NewTranscribeService(key, "", 600, &duration.Service{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tr, err := svc.Transcribe(ctx, "testdata/audio.mp3", func(done, total int) {
		t.Logf("chunk %d/%d", done, total)
	})
	require.NoError(t, err)
	require.NotEmpty(t, tr.Segments)
	t.Logf("language: %s, duration: %.1fs, segments: %d", tr.Language, tr.DurationSec, len(tr.Segments))
	t.Logf("first segment: [%.1f-%.1f] %s", tr.Segments[0].Start, tr.Segments[0].End, tr.Segments[0].Text)
}

func TestIntegrationEnrich(t *testing.T) {
	key := os.Getenv("GROQ_API_KEY")
	if key == "" {
		t.Skip("GROQ_API_KEY not set")
	}

	svc := NewEnrichService(key, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	tr := &Transcript{Segments: []TranscriptSegment{
		{Start: 0, Text: "привет сегодня поговорим про книгу дюна фрэнка герберта"},
		{Start: 12, Text: "она вышла в 1965 году и стала классикой фантастики"},
		{Start: 25, Text: "для заметок я использую обсидиан очень удобный инструмент"},
	}}

	cleaned, err := svc.CleanTranscript(ctx, tr, nil)
	require.NoError(t, err)
	t.Logf("cleaned:\n%s", cleaned)
	assert.Contains(t, cleaned, "[00:00]")

	meta, err := svc.ExtractMeta(ctx, "Про Дюну и заметки", "Тестовый канал", cleaned)
	require.NoError(t, err)
	t.Logf("tags: %v, lang: %s", meta.Tags, meta.Lang)
	assert.NotEmpty(t, meta.Tags)

	refs, err := svc.ExtractReferences(ctx, cleaned)
	require.NoError(t, err)
	t.Logf("references: %+v", refs)

	summary, err := svc.Summarize(ctx, cleaned, "")
	require.NoError(t, err)
	t.Logf("summary:\n%s", summary)
	assert.NotEmpty(t, summary)
}

func TestIntegrationNotion(t *testing.T) {
	token := os.Getenv("NOTION_TOKEN")
	parent := os.Getenv("NOTION_TEST_PARENT")
	if token == "" || parent == "" {
		t.Skip("NOTION_TOKEN or NOTION_TEST_PARENT not set")
	}

	w := NewNotionWriter(token, parent, newMemMetaStore())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	require.NoError(t, w.EnsureDatabases(ctx))

	url, created, err := w.WriteEpisode(ctx, "integration-test", EpisodeInput{
		Title: "Интеграционный тест", URL: "https://example.com", Channel: "Тест",
		Date: time.Now().Format("2006-01-02"), DurationMin: 1, Tags: []string{"test"},
		Summary:    "Тестовое саммари.\n\n- пункт раз\n- пункт два",
		Transcript: "[00:00] Тестовый транскрипт.\n\n[00:30] Второй абзац.",
		Refs:       []Reference{{Type: "книга", Name: "Дюна", Timecode: "00:00", Quote: "тест"}},
	})
	require.NoError(t, err)
	assert.True(t, created)
	t.Logf("page: %s", url)
}

package proc

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ytstore "github.com/umputun/feed-master/app/youtube/store"
)

func writeDigestFixture(t *testing.T, dir, id string, meta NoteMeta) {
	t.Helper()
	require.NoError(t, writeNoteFile(filepath.Join(dir, id+".md"), meta, "[00:00] тело "+id))
}

func TestTagStatsAndCollectSources(t *testing.T) {
	dir := t.TempDir()
	svc := NewNotesService(NotesParams{MDLocation: dir})

	writeDigestFixture(t, dir, "a1", NoteMeta{Title: "A", Date: "2026-06-02", Tags: []string{"Design", "history"}, Processed: []string{"md"}})
	writeDigestFixture(t, dir, "a2", NoteMeta{Title: "B", Date: "2026-06-01", Tags: []string{"design"}, Processed: []string{"md", "digest:design"}})
	writeDigestFixture(t, dir, "a3", NoteMeta{Title: "C", Date: "2026-06-03", Tags: []string{"other"}, Processed: []string{"md"}})

	stats, err := svc.TagStats()
	require.NoError(t, err)
	assert.Equal(t, 2, stats["design"], "tags are case-normalized")
	assert.Equal(t, 1, stats["history"])
	assert.Equal(t, 1, stats["other"])

	newOnes, included, err := svc.collectDigestSources("design")
	require.NoError(t, err)
	require.Len(t, newOnes, 1)
	assert.Equal(t, "A", newOnes[0].Meta.Title)
	require.Len(t, included, 1)
	assert.Equal(t, "B", included[0].Meta.Title)

	total, fresh, err := svc.DigestStatus("DESIGN")
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	assert.Equal(t, 1, fresh)
}

func TestProcessDigestEndToEnd(t *testing.T) {
	dir := t.TempDir()

	// groq chat mock: summaries and the final digest
	groq := mockGroqChat(t, func(userMsg string, jsonMode bool) string {
		if strings.Contains(userMsg, "тело") {
			return "саммари эпизода"
		}
		return "СВОДНЫЙ КОНСПЕКТ ПО ДИЗАЙНУ"
	})
	defer groq.Close()
	enricher := NewEnrichService("test-key", "")
	enricher.BaseURL = groq.URL

	notionSrv, state := newNotionMock(t)
	defer notionSrv.Close()
	store := newMemMetaStore()
	notion := newTestNotionWriter(notionSrv.URL, store)

	svc := NewNotesService(NotesParams{MDLocation: dir, Enricher: enricher, Notion: notion})

	writeDigestFixture(t, dir, "d1", NoteMeta{Title: "Выпуск 1", Date: "2026-06-01", Tags: []string{"design"}, Processed: []string{"md"}})
	writeDigestFixture(t, dir, "d2", NoteMeta{Title: "Выпуск 2", Date: "2026-06-02", Tags: []string{"design"}, Processed: []string{"md"}})

	res, err := svc.processDigest(context.Background(), ytstore.NotesJobRecord{
		URL: "design", SourceID: "digest_design", Source: "digest", Level: "digest",
	})
	require.NoError(t, err)
	assert.Contains(t, res.Title, "design")
	assert.NotEmpty(t, res.NotionPageURL)

	// local canonical digest written
	meta, body, err := readNoteFile(svc.digestMDPath("design"))
	require.NoError(t, err)
	assert.Equal(t, "digest", meta.Source)
	assert.Contains(t, body, "СВОДНЫЙ КОНСПЕКТ")

	// sources marked as included
	m1, _, err := readNoteFile(filepath.Join(dir, "d1.md"))
	require.NoError(t, err)
	assert.True(t, m1.hasProcessed("digest:design"))

	// rerun without new material short-circuits: no extra notion pages
	pagesBefore := state.pageCreates
	res2, err := svc.processDigest(context.Background(), ytstore.NotesJobRecord{
		URL: "design", SourceID: "digest_design", Source: "digest", Level: "digest",
	})
	require.NoError(t, err)
	assert.True(t, res2.Reused)
	assert.Equal(t, pagesBefore, state.pageCreates)

	// digest files must not leak into the L1 listing or tag stats
	stats, err := svc.TagStats()
	require.NoError(t, err)
	assert.Equal(t, 2, stats["design"], "digest md lives in a subdir, not counted")
}

func TestProcessDigestNoNotion(t *testing.T) {
	svc := NewNotesService(NotesParams{MDLocation: t.TempDir()})
	_, err := svc.processDigest(context.Background(), ytstore.NotesJobRecord{URL: "x", Level: "digest"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Notion")
}

func TestSlugifyTopic(t *testing.T) {
	assert.Equal(t, "нефтедобыча-и-переработка", slugifyTopic("  Нефтедобыча  и Переработка "))
	assert.Equal(t, "design", slugifyTopic("design"))
}

func TestProcessDigestSmartSelection(t *testing.T) {
	dir := t.TempDir()

	groq := mockGroqChat(t, func(userMsg string, jsonMode bool) string {
		if jsonMode && strings.Contains(userMsg, "\t") {
			// the catalog selection call: pick the two oil episodes by id
			return `{"ids":["oil1","oil2","ghost99"]}`
		}
		if strings.Contains(userMsg, "тело") {
			return "саммари эпизода"
		}
		return "СВОДНЫЙ КОНСПЕКТ ПО НЕФТИ"
	})
	defer groq.Close()
	enricher := NewEnrichService("test-key", "")
	enricher.BaseURL = groq.URL

	notionSrv, _ := newNotionMock(t)
	defer notionSrv.Close()
	notion := newTestNotionWriter(notionSrv.URL, newMemMetaStore())

	svc := NewNotesService(NotesParams{MDLocation: dir, Enricher: enricher, Notion: notion})

	// none of these carry the tag «нефтедобыча» — smart selection must kick in
	writeDigestFixture(t, dir, "oil1", NoteMeta{Title: "Как качают нефть", Date: "2026-06-01", Tags: []string{"oil-and-gas"}, Processed: []string{"md"}})
	writeDigestFixture(t, dir, "oil2", NoteMeta{Title: "Переработка в бензин", Date: "2026-06-02", Tags: []string{"petroleum"}, Processed: []string{"md"}})
	writeDigestFixture(t, dir, "cook", NoteMeta{Title: "Как варить борщ", Date: "2026-06-03", Tags: []string{"cooking"}, Processed: []string{"md"}})

	res, err := svc.processDigest(context.Background(), ytstore.NotesJobRecord{
		URL: "Нефтедобыча и переработка", SourceID: "digest_нефтедобыча-и-переработка", Level: "digest",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.NotionPageURL)

	// local digest written under the slug
	meta, body, err := readNoteFile(svc.digestMDPath("нефтедобыча-и-переработка"))
	require.NoError(t, err)
	assert.Contains(t, body, "СВОДНЫЙ КОНСПЕКТ")
	assert.Equal(t, []string{"нефтедобыча-и-переработка"}, meta.Tags)

	// selected sources are marked, the borscht is untouched
	m1, _, _ := readNoteFile(filepath.Join(dir, "oil1.md"))
	assert.True(t, m1.hasProcessed("digest:нефтедобыча-и-переработка"))
	mc, _, _ := readNoteFile(filepath.Join(dir, "cook.md"))
	assert.False(t, mc.hasProcessed("digest:нефтедобыча-и-переработка"))
}

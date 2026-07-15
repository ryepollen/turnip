package proc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadSourceID(t *testing.T) {
	url := "https://example.com/post"
	id := readSourceID(url)
	assert.Len(t, id, 16)
	assert.Equal(t, id, readSourceID(url), "stable for the same url")
	assert.NotEqual(t, readSourceID("https://example.com/other"), id)

	// distinct namespace from the notes layer: same url must not collide
	noteID, _ := noteSourceID(url)
	assert.NotEqual(t, noteID, id, "read and note ids use different salts")
}

func TestReadFileRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "abc.md")
	meta := ReadMeta{
		Title:      "Hello",
		SourceURL:  "https://example.com/x",
		Site:       "Example",
		Author:     "Jane",
		DateAdded:  "2026-07-13",
		ReadingMin: 4,
		Lang:       "ru",
		Tags:       []string{"a", "b"},
	}
	body := "# Hello\n\nsome content"

	require.NoError(t, writeReadFile(path, meta, body))

	got, gotBody, err := readReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, meta, got)
	assert.Equal(t, body, gotBody)
}

func TestReadFileNilTags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notags.md")
	require.NoError(t, writeReadFile(path, ReadMeta{Title: "T", DateAdded: "2026-07-13"}, "body"))
	got, _, err := readReadFile(path)
	require.NoError(t, err)
	assert.Empty(t, got.Tags) // normalized to [] on write, decodes as empty
}

func TestReadServiceSaveDedupAndList(t *testing.T) {
	const page = `<!DOCTYPE html><html><head><title>Saved Piece</title></head><body>
<article><h1>Saved Piece</h1>
<p>A body long enough to look like a real article so readability keeps it around for the test.</p>
<p>Another paragraph so the extractor is confident this is content worth reading.</p>
</article></body></html>`

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	dir := t.TempDir()
	svc := NewReadService(dir, NewArticleExtractor(), nil) // nil enricher: no LLM tags

	res, err := svc.Save(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.False(t, res.Reused)
	assert.Equal(t, "Saved Piece", res.Title)
	assert.FileExists(t, res.MDPath)
	assert.FileExists(t, res.HTMLPath)
	assert.Equal(t, 1, hits)

	// second save of the same url is a cache hit: no second fetch
	res2, err := svc.Save(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.True(t, res2.Reused)
	assert.Equal(t, res.SourceID, res2.SourceID)
	assert.Equal(t, 1, hits, "reused result must not refetch")

	items, err := svc.List()
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "Saved Piece", items[0].Meta.Title)

	// delete removes both the md and the archived html
	require.NoError(t, svc.Delete(res.SourceID))
	assert.NoFileExists(t, res.MDPath)
	assert.NoFileExists(t, res.HTMLPath)
	items, err = svc.List()
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestReadServiceDeleteGuardsPaths(t *testing.T) {
	svc := NewReadService(t.TempDir(), NewArticleExtractor(), nil)
	for _, bad := range []string{"", "../escape", "a/b", "x\\y"} {
		assert.Error(t, svc.Delete(bad), "must reject %q", bad)
	}
}

func TestReadBodyStructure(t *testing.T) {
	meta := ReadMeta{Title: "T", SourceURL: "https://ex.com/a", Site: "Ex"}
	body := readBody(meta, "## Section\n\ntext")
	assert.True(t, len(body) > 0)
	assert.Contains(t, body, "# T")
	assert.Contains(t, body, "[Источник](https://ex.com/a)")
	assert.Contains(t, body, "## Section")

	noSite := readBody(ReadMeta{Title: "T", SourceURL: "https://ex.com/a"}, "text")
	assert.Contains(t, noSite, "[Источник](https://ex.com/a)")
	assert.NotContains(t, noSite, " · ")
}

func TestReadListUnreadableFrontmatter(t *testing.T) {
	dir := t.TempDir()
	// a file without frontmatter must still be listed (fallback title = id)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deadbeef.md"), []byte("no frontmatter"), 0o640))
	svc := NewReadService(dir, NewArticleExtractor(), nil)
	items, err := svc.List()
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "deadbeef", items[0].Meta.Title)
}

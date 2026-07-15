package proc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTMLToMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		html     string
		contains []string
		absent   []string
	}{
		{
			name:     "heading and paragraph",
			html:     "<h1>Title</h1><p>Hello world</p>",
			contains: []string{"# Title", "Hello world"},
		},
		{
			name:     "unordered list",
			html:     "<ul><li>one</li><li>two</li></ul>",
			contains: []string{"- one", "- two"},
		},
		{
			name:     "link preserved",
			html:     `<p>see <a href="https://example.com/x">here</a></p>`,
			contains: []string{"[here](https://example.com/x)"},
		},
		{
			name:     "blockquote",
			html:     "<blockquote>a quote</blockquote>",
			contains: []string{"> a quote"},
		},
		{
			name:     "script stripped by sanitizer",
			html:     `<p>safe</p><script>alert('x')</script>`,
			contains: []string{"safe"},
			absent:   []string{"alert", "<script"},
		},
		{
			name:     "inline style attribute dropped",
			html:     `<p style="color:red" onclick="evil()">text</p>`,
			contains: []string{"text"},
			absent:   []string{"onclick", "evil", "color:red"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md, err := htmlToMarkdown(tt.html, "example.com")
			require.NoError(t, err)
			for _, want := range tt.contains {
				assert.Contains(t, md, want)
			}
			for _, no := range tt.absent {
				assert.NotContains(t, md, no)
			}
		})
	}
}

func TestReadingMinutes(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"one word rounds to 1", "hello", 1},
		{"exactly one page", strings.TrimSpace(strings.Repeat("word ", readingWordsPerMinute)), 1},
		{"just over one page", strings.TrimSpace(strings.Repeat("word ", readingWordsPerMinute+1)), 2},
		{"three pages", strings.TrimSpace(strings.Repeat("word ", readingWordsPerMinute*3)), 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, readingMinutes(tt.text))
		})
	}
}

func TestExtractStructuredDirect(t *testing.T) {
	const page = `<!DOCTYPE html><html><head><title>Test Article</title>
<meta name="author" content="Jane Doe"></head><body>
<article><h1>Test Article</h1>
<p>This is the first paragraph with enough words to be considered a real article body by the readability heuristics used here.</p>
<p>Second paragraph continues the thought and links to <a href="https://example.com/more">more reading</a> for context.</p>
<ul><li>alpha</li><li>beta</li></ul>
<blockquote>a memorable quote</blockquote>
</article></body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	e := NewArticleExtractor()
	doc, err := e.ExtractStructured(context.Background(), srv.URL)
	require.NoError(t, err)

	assert.Equal(t, "Test Article", doc.Title)
	assert.Contains(t, doc.MD, "first paragraph")
	assert.Contains(t, doc.MD, "[more reading](https://example.com/more)")
	assert.Contains(t, doc.MD, "- alpha")
	assert.Contains(t, doc.MD, "> a memorable quote")
	assert.NotEmpty(t, doc.RawHTML, "direct path archives the raw HTML")
	assert.Greater(t, doc.ReadingMin, 0)
}

func TestExtractStructuredJinaFallback(t *testing.T) {
	// origin always fails so extraction falls through to the jina reader
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer origin.Close()

	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Title: Blocked Piece\nURL Source: " + origin.URL +
			"\nMarkdown Content:\n# Blocked Piece\n\nBody text rendered by the reader.\n"))
	}))
	defer jina.Close()

	oldBase := jinaReaderBase
	jinaReaderBase = jina.URL + "/"
	defer func() { jinaReaderBase = oldBase }()

	e := NewArticleExtractor()
	doc, err := e.ExtractStructured(context.Background(), origin.URL)
	require.NoError(t, err)

	assert.Equal(t, "Blocked Piece", doc.Title)
	assert.Contains(t, doc.MD, "Body text rendered by the reader.")
	assert.Empty(t, doc.RawHTML, "jina fallback has no raw HTML to archive")
}

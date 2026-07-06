package proc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseJinaReader(t *testing.T) {
	raw := "Title: Заголовок статьи\nURL Source: https://example.com/a\n\nMarkdown Content:\nПервый абзац.\n\nВторой абзац."
	title, text := parseJinaReader(raw)
	assert.Equal(t, "Заголовок статьи", title)
	assert.Equal(t, "Первый абзац.\n\nВторой абзац.", text)

	// no header block: whole thing is the body
	title, text = parseJinaReader("просто текст")
	assert.Equal(t, "", title)
	assert.Equal(t, "просто текст", text)
}

func TestExtractFallsBackToJina(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // cloudflare-style block
	}))
	defer blocked.Close()

	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Title: Спасённая статья\n\nMarkdown Content:\nТекст добыт через ридер."))
	}))
	defer jina.Close()

	oldBase := jinaReaderBase
	jinaReaderBase = jina.URL + "/"
	defer func() { jinaReaderBase = oldBase }()

	e := NewArticleExtractor()
	article, err := e.Extract(context.Background(), blocked.URL+"/post")
	require.NoError(t, err)
	assert.Equal(t, "Спасённая статья", article.Title)
	assert.Contains(t, article.TextContent, "добыт через ридер")
}

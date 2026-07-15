package proc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	htmltomd "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/go-shiori/go-readability"
	"github.com/microcosm-cc/bluemonday"
)

// ReadableDoc is the reading-layer artifact: article content converted to
// Markdown with its structure preserved (headings, lists, links, blockquotes,
// code) — unlike the flat TTS text — plus the raw page HTML kept as insurance.
type ReadableDoc struct {
	Title      string
	Author     string
	SiteName   string
	MD         string // structural markdown
	RawHTML    string // original page HTML, archived as-is ("" for jina fallback)
	Image      string
	ReadingMin int
	URL        string
}

// readSanitizer keeps the formatting tags html-to-markdown understands and
// drops scripts/styles/attributes. Built once, it is safe for concurrent use.
var readSanitizer = bluemonday.UGCPolicy()

// ExtractStructured fetches a page and returns Markdown that keeps the article
// structure. It parses the page itself when possible (readability → sanitize →
// html→markdown, raw HTML archived); on failure it falls back to r.jina.ai,
// which already returns Markdown (no raw HTML to archive then).
func (e *ArticleExtractor) ExtractStructured(ctx context.Context, rawURL string) (*ReadableDoc, error) {
	doc, err := e.extractStructuredDirect(ctx, rawURL)
	if err == nil {
		return doc, nil
	}

	fallback, ferr := e.extractStructuredViaJina(ctx, rawURL)
	if ferr != nil {
		return nil, fmt.Errorf("%w (jina fallback: %v)", err, ferr)
	}
	return fallback, nil
}

// extractStructuredDirect fetches the page, keeps the raw HTML, runs
// readability, sanitizes and converts the cleaned HTML to Markdown
func (e *ArticleExtractor) extractStructuredDirect(ctx context.Context, rawURL string) (*ReadableDoc, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7")

	resp, err := e.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error: %d", resp.StatusCode)
	}

	// keep the raw bytes: they feed both readability and the HTML archive
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	article, err := readability.FromReader(bytes.NewReader(raw), parsedURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse article: %w", err)
	}
	if strings.TrimSpace(article.Content) == "" {
		return nil, fmt.Errorf("no content extracted from article")
	}

	markdown, err := htmlToMarkdown(article.Content, parsedURL.Host)
	if err != nil {
		return nil, fmt.Errorf("failed to convert to markdown: %w", err)
	}
	if strings.TrimSpace(markdown) == "" {
		return nil, fmt.Errorf("markdown conversion produced empty output")
	}

	return &ReadableDoc{
		Title:      article.Title,
		Author:     article.Byline,
		SiteName:   article.SiteName,
		MD:         markdown,
		RawHTML:    string(raw),
		Image:      article.Image,
		ReadingMin: readingMinutes(article.TextContent),
		URL:        rawURL,
	}, nil
}

// extractStructuredViaJina uses r.jina.ai, which renders the page server-side
// and already returns Markdown; there is no raw HTML to archive in this path
func (e *ArticleExtractor) extractStructuredViaJina(ctx context.Context, rawURL string) (*ReadableDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jinaReaderBase+rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create jina request: %w", err)
	}
	resp, err := e.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jina request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jina HTTP error: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("failed to read jina response: %w", err)
	}

	title, markdown := parseJinaReader(string(data))
	if strings.TrimSpace(markdown) == "" {
		return nil, fmt.Errorf("jina returned no content")
	}
	return &ReadableDoc{
		Title:      title,
		MD:         markdown,
		ReadingMin: readingMinutes(markdown),
		URL:        rawURL,
	}, nil
}

// htmlToMarkdown sanitizes cleaned article HTML and converts it to Markdown.
// domain resolves any leftover relative links to absolute URLs.
func htmlToMarkdown(rawHTML, domain string) (string, error) {
	safe := readSanitizer.Sanitize(rawHTML)
	conv := htmltomd.NewConverter(domain, true, nil)
	return conv.ConvertString(safe)
}

// readingWordsPerMinute is a typical adult silent-reading rate; the reading
// layer reports time-to-read, not time-to-listen (that is EstimateDuration)
const readingWordsPerMinute = 200

// readingMinutes estimates minutes to read text at readingWordsPerMinute,
// rounding up so a short article is never "0 min"
func readingMinutes(text string) int {
	words := len(strings.Fields(text))
	if words == 0 {
		return 0
	}
	m := (words + readingWordsPerMinute - 1) / readingWordsPerMinute
	if m < 1 {
		m = 1
	}
	return m
}

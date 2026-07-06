package proc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-shiori/go-readability"
)

// Article represents extracted article content
type Article struct {
	Title       string
	Content     string
	TextContent string
	Image       string
	SiteName    string
	URL         string
}

// ArticleExtractor extracts readable content from URLs
type ArticleExtractor struct {
	HTTPClient *http.Client
}

// NewArticleExtractor creates a new article extractor
func NewArticleExtractor() *ArticleExtractor {
	return &ArticleExtractor{
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Extract fetches URL and extracts article content. Sites behind Cloudflare
// or aggressive bot protection (403 etc.) go through the r.jina.ai reader
// as a fallback.
func (e *ArticleExtractor) Extract(ctx context.Context, rawURL string) (*Article, error) {
	article, err := e.extractDirect(ctx, rawURL)
	if err == nil {
		return article, nil
	}

	fallback, ferr := e.extractViaJina(ctx, rawURL)
	if ferr != nil {
		return nil, fmt.Errorf("%w (jina fallback: %v)", err, ferr)
	}
	return fallback, nil
}

// extractDirect fetches the page itself and runs readability over it
func (e *ArticleExtractor) extractDirect(ctx context.Context, rawURL string) (*Article, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	// Create request with context
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set user agent to avoid being blocked
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

	// Parse with readability, limit response body to 5 MB to prevent OOM on huge pages
	limitedBody := io.LimitReader(resp.Body, 5*1024*1024)
	article, err := readability.FromReader(limitedBody, parsedURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse article: %w", err)
	}

	if article.TextContent == "" {
		return nil, fmt.Errorf("no content extracted from article")
	}

	return &Article{
		Title:       article.Title,
		Content:     article.Content,
		TextContent: cleanText(article.TextContent),
		Image:       article.Image,
		SiteName:    article.SiteName,
		URL:         rawURL,
	}, nil
}

// jinaReaderBase is the free r.jina.ai reader endpoint (var for tests)
var jinaReaderBase = "https://r.jina.ai/"

// extractViaJina reads the page through the r.jina.ai reader: it renders
// JS-heavy and bot-protected pages server-side and returns
// "Title: ...\nURL Source: ...\nMarkdown Content:\n<text>"
func (e *ArticleExtractor) extractViaJina(ctx context.Context, rawURL string) (*Article, error) {
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

	title, text := parseJinaReader(string(data))
	if text == "" {
		return nil, fmt.Errorf("jina returned no content")
	}
	return &Article{
		Title:       title,
		TextContent: cleanText(text),
		URL:         rawURL,
	}, nil
}

// parseJinaReader splits the reader's header block from the markdown body
func parseJinaReader(raw string) (title, text string) {
	body := raw
	if idx := strings.Index(raw, "Markdown Content:"); idx >= 0 {
		header := raw[:idx]
		body = raw[idx+len("Markdown Content:"):]
		for _, line := range strings.Split(header, "\n") {
			if strings.HasPrefix(line, "Title:") {
				title = strings.TrimSpace(strings.TrimPrefix(line, "Title:"))
				break
			}
		}
	}
	return title, strings.TrimSpace(body)
}

// cleanText removes extra whitespace and cleans up text for TTS
func cleanText(text string) string {
	// Replace multiple newlines with single newline
	lines := strings.Split(text, "\n")
	var cleaned []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			cleaned = append(cleaned, line)
		}
	}
	text = strings.Join(cleaned, "\n")

	// Replace multiple spaces with single space
	for strings.Contains(text, "  ") {
		text = strings.ReplaceAll(text, "  ", " ")
	}

	return text
}

// IsArticleURL checks if URL looks like an article (not YouTube, not image, etc.)
func IsArticleURL(rawURL string) bool {
	// Check if it's a YouTube URL
	if isYouTubeURL(rawURL) {
		return false
	}

	// Parse URL
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	// Must be http or https
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}

	// Check for media file extensions
	path := strings.ToLower(parsed.Path)
	mediaExtensions := []string{".jpg", ".jpeg", ".png", ".gif", ".webp", ".mp3", ".mp4", ".pdf", ".zip", ".rar"}
	for _, ext := range mediaExtensions {
		if strings.HasSuffix(path, ext) {
			return false
		}
	}

	return true
}

// isYouTubeURL checks if URL is a YouTube video URL
func isYouTubeURL(rawURL string) bool {
	patterns := []string{
		"youtube.com/watch",
		"youtu.be/",
		"youtube.com/embed/",
		"youtube.com/v/",
		"youtube.com/shorts/",
	}

	lowerURL := strings.ToLower(rawURL)
	for _, pattern := range patterns {
		if strings.Contains(lowerURL, pattern) {
			return true
		}
	}
	return false
}

// EstimateDuration estimates audio duration based on text length
// Average Russian speech rate is about 120-150 words per minute
// Average word length in Russian is about 6 characters
func EstimateDuration(text string) time.Duration {
	charCount := len([]rune(text))
	// ~6 chars per word, ~130 words per minute = ~780 chars per minute
	minutes := float64(charCount) / 780.0
	return time.Duration(minutes * float64(time.Minute))
}

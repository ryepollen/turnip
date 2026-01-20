package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// Lingva Translate instances (fallback list)
var lingvaInstances = []string{
	"https://lingva.lunar.icu/api/v1",
	"https://translate.plausibility.cloud/api/v1",
	"https://lingva.garudalinux.org/api/v1",
	"https://translate.projectsegfau.lt/api/v1",
}

// Translator handles text translation using Lingva Translate (Google Translate frontend)
type Translator struct {
	instances  []string
	targetLang string
	client     *http.Client
}

// NewTranslator creates a new translator instance
func NewTranslator(targetLang string) *Translator {
	if targetLang == "" {
		targetLang = "ru"
	}
	return &Translator{
		instances:  lingvaInstances,
		targetLang: targetLang,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// lingvaResponse is the response from Lingva Translate API
type lingvaResponse struct {
	Translation string `json:"translation"`
	Error       string `json:"error,omitempty"`
}

// DetectLanguage detects if text is primarily in Russian or another language
// Returns "ru" for Russian, "en" for English, etc.
func DetectLanguage(text string) string {
	// Count Cyrillic vs Latin characters
	var cyrillic, latin int
	for _, r := range text {
		if unicode.Is(unicode.Cyrillic, r) {
			cyrillic++
		} else if unicode.Is(unicode.Latin, r) {
			latin++
		}
	}

	// If more than 30% Cyrillic, consider it Russian
	total := cyrillic + latin
	if total == 0 {
		return "en" // default to English
	}

	if float64(cyrillic)/float64(total) > 0.3 {
		return "ru"
	}
	return "en"
}

// NeedsTranslation checks if text needs translation to target language
func (t *Translator) NeedsTranslation(text string) bool {
	detectedLang := DetectLanguage(text)
	return detectedLang != t.targetLang
}

// Translate translates text to target language
func (t *Translator) Translate(ctx context.Context, text string) (string, error) {
	sourceLang := DetectLanguage(text)
	if sourceLang == t.targetLang {
		return text, nil // already in target language
	}

	// Split into chunks if text is too long (URL length limits, 500 chars is safe)
	const maxChunkSize = 500
	if len(text) <= maxChunkSize {
		return t.translateChunk(ctx, text, sourceLang)
	}

	// Split by paragraphs and translate
	chunks := splitTextForTranslation(text, maxChunkSize)
	var result strings.Builder

	for i, chunk := range chunks {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		translated, err := t.translateChunk(ctx, chunk, sourceLang)
		if err != nil {
			return "", fmt.Errorf("failed to translate chunk %d: %w", i, err)
		}
		result.WriteString(translated)

		// Small delay between requests
		if i < len(chunks)-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}

	return result.String(), nil
}

// translateChunk translates a single chunk of text using Lingva API with fallback instances
func (t *Translator) translateChunk(ctx context.Context, text, sourceLang string) (string, error) {
	// Lingva API uses URL path: /api/v1/:source/:target/:query
	encodedText := url.QueryEscape(text)

	var lastErr error
	for _, instance := range t.instances {
		apiURL := fmt.Sprintf("%s/%s/%s/%s", instance, sourceLang, t.targetLang, encodedText)

		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			lastErr = fmt.Errorf("failed to create request: %w", err)
			continue
		}

		resp, err := t.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request to %s failed: %w", instance, err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("%s returned status %d", instance, resp.StatusCode)
			continue
		}

		var result lingvaResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			lastErr = fmt.Errorf("failed to decode response from %s: %w", instance, err)
			continue
		}
		resp.Body.Close()

		if result.Error != "" {
			lastErr = fmt.Errorf("translation error from %s: %s", instance, result.Error)
			continue
		}

		return result.Translation, nil
	}

	return "", fmt.Errorf("all translation instances failed, last error: %w", lastErr)
}

// splitTextForTranslation splits text into chunks at paragraph boundaries
func splitTextForTranslation(text string, maxSize int) []string {
	if len(text) <= maxSize {
		return []string{text}
	}

	var chunks []string
	paragraphs := strings.Split(text, "\n\n")
	var current strings.Builder

	for _, para := range paragraphs {
		if current.Len()+len(para)+2 > maxSize && current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(para)
	}

	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	return chunks
}

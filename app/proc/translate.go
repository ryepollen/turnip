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

// Translator handles text translation using Lingva Translate (Google Translate frontend)
type Translator struct {
	apiURL     string
	targetLang string
	client     *http.Client
}

// NewTranslator creates a new translator instance
func NewTranslator(targetLang string) *Translator {
	if targetLang == "" {
		targetLang = "ru"
	}
	return &Translator{
		apiURL:     "https://lingva.ml/api/v1", // Lingva Translate (free Google Translate frontend)
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

	// Split into chunks if text is too long (URL length limits ~2000 chars)
	const maxChunkSize = 1500
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

// translateChunk translates a single chunk of text using Lingva API
func (t *Translator) translateChunk(ctx context.Context, text, sourceLang string) (string, error) {
	// Lingva API uses URL path: /api/v1/:source/:target/:query
	// URL encode the text
	encodedText := url.QueryEscape(text)
	apiURL := fmt.Sprintf("%s/%s/%s/%s", t.apiURL, sourceLang, t.targetLang, encodedText)

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("translation API returned status %d", resp.StatusCode)
	}

	var result lingvaResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if result.Error != "" {
		return "", fmt.Errorf("translation error: %s", result.Error)
	}

	return result.Translation, nil
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

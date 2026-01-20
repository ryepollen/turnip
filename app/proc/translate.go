package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// Translator handles text translation
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
		apiURL:     "https://libretranslate.com/translate", // public instance
		targetLang: targetLang,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// translateRequest is the request body for LibreTranslate API
type translateRequest struct {
	Q      string `json:"q"`
	Source string `json:"source"`
	Target string `json:"target"`
}

// translateResponse is the response from LibreTranslate API
type translateResponse struct {
	TranslatedText string `json:"translatedText"`
	Error          string `json:"error,omitempty"`
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

	// Split into chunks if text is too long (LibreTranslate has limits)
	const maxChunkSize = 5000
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

// translateChunk translates a single chunk of text
func (t *Translator) translateChunk(ctx context.Context, text, sourceLang string) (string, error) {
	reqBody := translateRequest{
		Q:      text,
		Source: sourceLang,
		Target: t.targetLang,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", t.apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("translation API returned status %d", resp.StatusCode)
	}

	var result translateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if result.Error != "" {
		return "", fmt.Errorf("translation error: %s", result.Error)
	}

	return result.TranslatedText, nil
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

package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// Translator handles text translation using Yandex Translate API
type Translator struct {
	apiKey     string
	targetLang string
	folderID   string
	client     *http.Client
}

// NewTranslator creates a new translator instance
// apiKey should be passed from environment variable YANDEX_TRANSLATE_KEY
func NewTranslator(targetLang string) *Translator {
	if targetLang == "" {
		targetLang = "ru"
	}
	return &Translator{
		apiKey:     "", // will be set via SetAPIKey
		targetLang: targetLang,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// NewTranslatorWithKey creates a translator with API key and folder ID
func NewTranslatorWithKey(apiKey, folderID, targetLang string) *Translator {
	if targetLang == "" {
		targetLang = "ru"
	}
	return &Translator{
		apiKey:     apiKey,
		folderID:   folderID,
		targetLang: targetLang,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// yandexRequest is the request body for Yandex Translate API
type yandexRequest struct {
	FolderID           string   `json:"folderId"`
	TargetLanguageCode string   `json:"targetLanguageCode"`
	Texts              []string `json:"texts"`
}

// yandexResponse is the response from Yandex Translate API
type yandexResponse struct {
	Translations []struct {
		Text string `json:"text"`
	} `json:"translations"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
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

	// Split into chunks if text is too long (Yandex limit is 10000 chars per request)
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

// translateChunk translates a single chunk of text using Yandex Translate API
func (t *Translator) translateChunk(ctx context.Context, text, sourceLang string) (string, error) {
	if t.apiKey == "" {
		return "", fmt.Errorf("Yandex Translate API key not configured (set YANDEX_TRANSLATE_KEY)")
	}
	if t.folderID == "" {
		return "", fmt.Errorf("Yandex Folder ID not configured (set YANDEX_FOLDER_ID)")
	}

	// Yandex Translate API endpoint
	apiURL := "https://translate.api.cloud.yandex.net/translate/v2/translate"

	reqBody := yandexRequest{
		FolderID:           t.folderID,
		TargetLanguageCode: t.targetLang,
		Texts:              []string{text},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Api-Key "+t.apiKey)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read raw body for debugging
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Yandex API error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var result yandexResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if len(result.Translations) == 0 {
		return "", fmt.Errorf("no translations returned")
	}

	return result.Translations[0].Text, nil
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

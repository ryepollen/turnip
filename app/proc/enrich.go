package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// enrichChunkSize is the max chars per LLM call for chunked passes,
// sized to stay within Groq free-tier TPM limits (~3k tokens per chunk)
const enrichChunkSize = 9000

// summarizeSinglePassLimit is the max chars summarized in one call, above it map-reduce kicks in
const summarizeSinglePassLimit = 24000

// EnrichService runs LLM passes over transcripts via Groq chat completions
type EnrichService struct {
	APIKey  string
	Model   string
	BaseURL string
	client  *http.Client
}

// NewEnrichService creates an enrichment service
func NewEnrichService(apiKey, model string) *EnrichService {
	if model == "" {
		model = "llama-3.3-70b-versatile"
	}
	return &EnrichService{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: groqAPIBase,
		client:  &http.Client{Timeout: 3 * time.Minute},
	}
}

// NoteTags is the L1 metadata extracted by the LLM
type NoteTags struct {
	Tags []string `json:"tags"`
	Lang string   `json:"lang"`
}

// Reference is one extracted mention (book/person/tool/article)
type Reference struct {
	Type     string `json:"type"` // книга|человек|инструмент|статья|другое
	Name     string `json:"name"`
	Timecode string `json:"timecode"` // "MM:SS" or "H:MM:SS"
	Quote    string `json:"quote"`
}

// CleanTranscript turns raw Whisper segments into readable markdown paragraphs,
// keeping one leading [MM:SS] marker per semantic block. Long transcripts are
// processed in chunks split at segment boundaries; the tail of the previous
// cleaned chunk is passed as context so joins stay coherent.
func (e *EnrichService) CleanTranscript(ctx context.Context, tr *Transcript, progress func(done, total int)) (string, error) {
	raw := renderSegments(tr.Segments)
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("empty transcript")
	}

	chunks := packLines(strings.Split(raw, "\n"), enrichChunkSize)
	var out []string
	prevTail := ""
	for i, chunk := range chunks {
		cleaned, err := e.chat(ctx, cleanupPrompt(prevTail), chunk, false)
		if err != nil {
			return "", fmt.Errorf("failed to clean chunk %d/%d: %w", i+1, len(chunks), err)
		}
		cleaned = strings.TrimSpace(cleaned)
		out = append(out, cleaned)
		prevTail = tailChars(cleaned, 300)
		if progress != nil {
			progress(i+1, len(chunks))
		}
	}
	return strings.Join(out, "\n\n"), nil
}

// CleanPlainText cleans timecode-less text (e.g. parsed subtitles) into readable
// paragraphs, chunked the same way as CleanTranscript
func (e *EnrichService) CleanPlainText(ctx context.Context, text string, progress func(done, total int)) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("empty text")
	}
	chunks := packLines(splitTextForTranslation(text, 3000), enrichChunkSize)
	var out []string
	prevTail := ""
	for i, chunk := range chunks {
		cleaned, err := e.chat(ctx, plainCleanupPrompt(prevTail), chunk, false)
		if err != nil {
			return "", fmt.Errorf("failed to clean chunk %d/%d: %w", i+1, len(chunks), err)
		}
		cleaned = strings.TrimSpace(cleaned)
		out = append(out, cleaned)
		prevTail = tailChars(cleaned, 300)
		if progress != nil {
			progress(i+1, len(chunks))
		}
	}
	return strings.Join(out, "\n\n"), nil
}

// ExtractMeta asks the LLM for 2-4 topic tags and the content language (JSON mode)
func (e *EnrichService) ExtractMeta(ctx context.Context, title, channel, cleanedHead string) (*NoteTags, error) {
	user := fmt.Sprintf("Заголовок: %s\nКанал: %s\n\nНачало текста:\n%s", title, channel, headChars(cleanedHead, 12000))
	resp, err := e.chat(ctx, metaPrompt(), user, true)
	if err != nil {
		return nil, fmt.Errorf("failed to extract meta: %w", err)
	}
	var meta NoteTags
	if err := json.Unmarshal([]byte(resp), &meta); err != nil {
		return nil, fmt.Errorf("failed to parse meta json: %w", err)
	}
	if len(meta.Tags) > 4 {
		meta.Tags = meta.Tags[:4]
	}
	return &meta, nil
}

// Summarize produces a summary; texts above summarizeSinglePassLimit go through map-reduce
func (e *EnrichService) Summarize(ctx context.Context, cleaned string) (string, error) {
	if len(cleaned) <= summarizeSinglePassLimit {
		return e.chat(ctx, summaryPrompt(), cleaned, false)
	}

	chunks := packLines(strings.Split(cleaned, "\n"), summarizeSinglePassLimit)
	var partials []string
	for i, chunk := range chunks {
		part, err := e.chat(ctx, partialSummaryPrompt(), chunk, false)
		if err != nil {
			return "", fmt.Errorf("failed to summarize part %d/%d: %w", i+1, len(chunks), err)
		}
		partials = append(partials, part)
	}
	return e.chat(ctx, combineSummaryPrompt(), strings.Join(partials, "\n\n---\n\n"), false)
}

// ExtractReferences extracts mentions chunk by chunk (JSON mode), merging and
// deduplicating by normalized name. Malformed chunk output is skipped: references
// are best-effort, a partial list beats a failed job.
func (e *EnrichService) ExtractReferences(ctx context.Context, cleaned string) ([]Reference, error) {
	chunks := packLines(strings.Split(cleaned, "\n"), enrichChunkSize)
	var refs []Reference
	seen := map[string]bool{}
	for i, chunk := range chunks {
		resp, err := e.chat(ctx, referencesPrompt(), chunk, true)
		if err != nil {
			return nil, fmt.Errorf("failed to extract references from chunk %d/%d: %w", i+1, len(chunks), err)
		}
		var parsed struct {
			References []Reference `json:"references"`
		}
		if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
			log.Printf("[WARN] skipping malformed references json in chunk %d/%d: %v", i+1, len(chunks), err)
			continue
		}
		for _, r := range parsed.References {
			key := strings.ToLower(strings.TrimSpace(r.Name))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, r)
		}
	}
	return refs, nil
}

// chat makes one Groq chat completions call
func (e *EnrichService) chat(ctx context.Context, system, user string, jsonMode bool) (string, error) {
	if e.APIKey == "" {
		return "", fmt.Errorf("groq api key not configured (set GROQ_API_KEY)")
	}

	payload := map[string]any{
		"model":       e.Model,
		"temperature": 0.2,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	if jsonMode {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", e.BaseURL+"/chat/completions", bytes.NewReader(jsonData))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
		return req, nil
	}

	resp, err := doWithRetry(ctx, e.client, build)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices returned")
	}
	return result.Choices[0].Message.Content, nil
}

// renderSegments formats Whisper segments as "[MM:SS] text" lines
func renderSegments(segments []TranscriptSegment) string {
	var b strings.Builder
	for _, seg := range segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		b.WriteString(formatTimecode(seg.Start))
		b.WriteString(" ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatTimecode renders seconds as [MM:SS] or [H:MM:SS]
func formatTimecode(seconds float64) string {
	s := int(seconds)
	if s < 3600 {
		return fmt.Sprintf("[%02d:%02d]", s/60, s%60)
	}
	return fmt.Sprintf("[%d:%02d:%02d]", s/3600, (s%3600)/60, s%60)
}

// packLines greedily packs lines into chunks of at most maxChars each.
// A single line longer than maxChars becomes its own chunk.
func packLines(lines []string, maxChars int) []string {
	var chunks []string
	var current strings.Builder
	for _, line := range lines {
		if current.Len() > 0 && current.Len()+len(line)+1 > maxChars {
			chunks = append(chunks, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(line)
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}

// tailChars returns up to n last chars of s without splitting runes
func tailChars(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// headChars returns up to n first chars of s without splitting runes
func headChars(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// prompt builders are package-level for testability

func cleanupPrompt(prevTail string) string {
	p := `Ты редактор транскриптов. На входе — сырой текст распознанной речи, каждая строка начинается с таймкода [MM:SS].
Твоя задача:
- исправь пунктуацию и очевидные ошибки распознавания;
- объедини строки в смысловые абзацы (3-8 предложений), сохрани у каждого абзаца ОДИН ведущий таймкод — таймкод первой строки абзаца;
- ничего не сокращай и не пересказывай, сохрани весь смысл и язык оригинала;
- не добавляй заголовков, комментариев и пояснений — только очищенный текст.`
	if prevTail != "" {
		p += "\n\nКонец предыдущего фрагмента (для связности, НЕ повторяй его в ответе):\n" + prevTail
	}
	return p
}

func plainCleanupPrompt(prevTail string) string {
	p := `Ты редактор транскриптов. На входе — сырой текст распознанной речи без таймкодов.
Исправь пунктуацию и очевидные ошибки распознавания, разбей на смысловые абзацы (3-8 предложений).
Ничего не сокращай и не пересказывай, сохрани весь смысл и язык оригинала.
Не добавляй заголовков, комментариев и пояснений — только очищенный текст.`
	if prevTail != "" {
		p += "\n\nКонец предыдущего фрагмента (для связности, НЕ повторяй его в ответе):\n" + prevTail
	}
	return p
}

func metaPrompt() string {
	return `Определи язык текста и подбери 2-4 тематических тега (короткие, kebab-case, на английском).
Ответь строго JSON-объектом: {"tags": ["tag-one", "tag-two"], "lang": "ru"}`
}

func summaryPrompt() string {
	return `Сделай конспект текста на русском языке: 5-10 предложений общего саммари, затем список ключевых мыслей буллетами.
Если в тексте есть таймкоды [MM:SS] — начинай каждый буллет с таймкода соответствующего блока.
Пиши по существу, без вводных фраз про "этот текст" и "автор рассказывает".`
}

func partialSummaryPrompt() string {
	return `Сделай краткий конспект фрагмента текста на русском языке: главные мысли и факты, 5-8 предложений. Без вводных фраз.`
}

func combineSummaryPrompt() string {
	return `Ниже конспекты последовательных фрагментов одного выпуска, разделённые "---".
Собери из них единый конспект на русском: 5-10 предложений общего саммари, затем список ключевых мыслей буллетами. Убери повторы.`
}

func referencesPrompt() string {
	return `Найди в тексте все упоминания конкретных книг, фильмов, сериалов, людей, инструментов/продуктов, статей, компаний, подкастов и концепций.
Каждая строка текста или абзац начинается с таймкода [MM:SS] или [H:MM:SS] — укажи таймкод абзаца, где встретилось упоминание.
Ответь строго JSON-объектом:
{"references": [{"type": "книга|фильм|сериал|человек|инструмент|статья|компания|подкаст|концепция|другое", "name": "точное название", "timecode": "MM:SS", "quote": "короткая цитата-контекст"}]}
Если упоминаний нет — {"references": []}. Не выдумывай: только то, что явно названо в тексте.`
}

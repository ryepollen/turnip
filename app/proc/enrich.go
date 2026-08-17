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

// The notes pipeline runs two Groq models: a cheap fast one for the bulk cleanup
// pass (gpt-oss-20b) and a stronger one for summary/references/digest
// (gpt-oss-120b). Both sit at a 250K TPM ceiling, so the budgets below leave
// generous headroom; they stay conservative anyway because a single request
// whose input+output overshoots the ceiling gets a non-retryable 413 "Request
// too large". Sizes below are in *estimated tokens*, not chars — Cyrillic
// tokenizes at roughly one token per character (~2 bytes/token) versus
// ~4 bytes/token for Latin, so a byte/char budget that is safe for English
// silently overflows on Russian transcripts.
const (
	// cleanupChunkTokens caps the input of a cleanup pass, where the output is
	// about as large as the input; leaves room for both under the TPM ceiling.
	cleanupChunkTokens = 2200
	cleanupMaxOut      = 2800

	// summaryChunkTokens caps the input of a summary/references/digest pass,
	// whose output is comparatively small (bounded by summaryMaxOut).
	summaryChunkTokens = 3200
	summaryMaxOut      = 1400

	// metaMaxOut / selectMaxOut bound the tiny JSON responses of those passes.
	metaMaxOut   = 500
	selectMaxOut = 800
)

// EnrichService runs LLM passes over transcripts via Groq chat completions.
// Model is the strong model used for summary/references/digest; CleanModel is
// the cheaper, faster one used for the high-volume transcript cleanup pass.
type EnrichService struct {
	APIKey     string
	Model      string
	CleanModel string
	BaseURL    string
	client     *http.Client
}

// NewEnrichService creates an enrichment service. model is the strong summary
// model, cleanModel the cheap cleanup model; empty values fall back to Groq's
// current production GPT-OSS pair.
func NewEnrichService(apiKey, model, cleanModel string) *EnrichService {
	if model == "" {
		model = "openai/gpt-oss-120b"
	}
	if cleanModel == "" {
		cleanModel = "openai/gpt-oss-20b"
	}
	return &EnrichService{
		APIKey:     apiKey,
		Model:      model,
		CleanModel: cleanModel,
		BaseURL:    groqAPIBase,
		client:     &http.Client{Timeout: 3 * time.Minute},
	}
}

// cleanModelID returns the model to use for cleanup passes, falling back to the
// strong model if no dedicated cleanup model is configured.
func (e *EnrichService) cleanModelID() string {
	if e.CleanModel != "" {
		return e.CleanModel
	}
	return e.Model
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

	chunks := packLines(strings.Split(raw, "\n"), cleanupChunkTokens)
	var out []string
	prevTail := ""
	for i, chunk := range chunks {
		cleaned, err := e.chatModel(ctx, e.cleanModelID(), cleanupPrompt(prevTail), chunk, false, cleanupMaxOut)
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
	chunks := packLines(splitTextForTranslation(text, 3000), cleanupChunkTokens)
	var out []string
	prevTail := ""
	for i, chunk := range chunks {
		cleaned, err := e.chatModel(ctx, e.cleanModelID(), plainCleanupPrompt(prevTail), chunk, false, cleanupMaxOut)
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
	// head only, capped in runes: Cyrillic ≈ 1 token/char, keep well under the TPM ceiling
	user := fmt.Sprintf("Заголовок: %s\nКанал: %s\n\nНачало текста:\n%s", title, channel, headChars(cleanedHead, 4000))
	resp, err := e.chat(ctx, metaPrompt(), user, true, metaMaxOut)
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

// summary length presets; "" means normal
const (
	SummaryShort = "short"
	SummaryLong  = "long"
)

// Summarize produces a summary; texts above summaryChunkTokens go through
// map-reduce. length ("" normal | SummaryShort | SummaryLong) tunes the final
// summary's depth without changing the intermediate map step.
func (e *EnrichService) Summarize(ctx context.Context, cleaned, length string) (string, error) {
	if estimateTokens(cleaned) <= summaryChunkTokens {
		return e.chat(ctx, summaryPrompt(length), cleaned, false, summaryMaxOut)
	}

	chunks := packLines(strings.Split(cleaned, "\n"), summaryChunkTokens)
	partials := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		part, err := e.chat(ctx, partialSummaryPrompt(), chunk, false, summaryMaxOut)
		if err != nil {
			return "", fmt.Errorf("failed to summarize part %d/%d: %w", i+1, len(chunks), err)
		}
		partials = append(partials, part)
	}
	return e.reduceSummaries(ctx, partials, combineSummaryPrompt(length))
}

// reduceSummaries combines partial summaries into one. A single combine call can
// itself overflow the TPM ceiling when there are many partials, so partials are
// merged in token-bounded groups and the reduction repeats until one remains.
func (e *EnrichService) reduceSummaries(ctx context.Context, partials []string, combinePrompt string) (string, error) {
	if len(partials) == 0 {
		return "", nil
	}
	// a single partial (e.g. one oversized chunk that never got a combine pass)
	// still needs the combine prompt applied to shape it to the length preset
	if len(partials) == 1 {
		return e.chat(ctx, combinePrompt, partials[0], false, summaryMaxOut)
	}
	// leave headroom under the budget for the "\n\n---\n\n" separators the join
	// inserts and the system prompt, so a full group can't tip over the ceiling
	budget := summaryChunkTokens - 256
	for len(partials) > 1 {
		groups := groupByTokens(partials, budget)
		// guard against a stuck reduction (lone partials each at/over budget):
		// force the first two together so the count always shrinks
		if len(groups) == len(partials) {
			groups = [][]string{{partials[0], partials[1]}}
			for _, p := range partials[2:] {
				groups = append(groups, []string{p})
			}
		}
		next := make([]string, 0, len(groups))
		for _, g := range groups {
			if len(g) == 1 {
				next = append(next, g[0])
				continue
			}
			merged, err := e.chat(ctx, combinePrompt, strings.Join(g, "\n\n---\n\n"), false, summaryMaxOut)
			if err != nil {
				return "", fmt.Errorf("failed to combine %d summaries: %w", len(g), err)
			}
			next = append(next, merged)
		}
		partials = next
	}
	// the last iteration always ends in a merge, so partials[0] is combine-shaped
	return partials[0], nil
}

// ExtractReferences extracts mentions chunk by chunk (JSON mode), merging and
// deduplicating by normalized name. Malformed chunk output is skipped: references
// are best-effort, a partial list beats a failed job.
func (e *EnrichService) ExtractReferences(ctx context.Context, cleaned string) ([]Reference, error) {
	chunks := packLines(strings.Split(cleaned, "\n"), summaryChunkTokens)
	var refs []Reference
	seen := map[string]bool{}
	for i, chunk := range chunks {
		resp, err := e.chat(ctx, referencesPrompt(), chunk, true, summaryMaxOut)
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

// SelectRelevant picks catalog entries relevant to a freeform topic (JSON
// mode). catalog lines are "id\ttitle\ttags"; returns the chosen ids.
func (e *EnrichService) SelectRelevant(ctx context.Context, topic string, catalog []string) ([]string, error) {
	resp, err := e.chat(ctx, selectRelevantPrompt(topic), strings.Join(catalog, "\n"), true, selectMaxOut)
	if err != nil {
		return nil, fmt.Errorf("failed to select relevant sources: %w", err)
	}
	var parsed struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse selection json: %w", err)
	}
	return parsed.IDs, nil
}

// SynthesizeDigest builds one thematic digest from per-episode summaries and
// the previous digest text. Oversized input goes through map-reduce.
func (e *EnrichService) SynthesizeDigest(ctx context.Context, tag, combined string) (string, error) {
	if estimateTokens(combined) <= summaryChunkTokens {
		return e.chat(ctx, digestPrompt(tag), combined, false, summaryMaxOut)
	}
	chunks := packLines(strings.Split(combined, "\n"), summaryChunkTokens)
	partials := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		part, err := e.chat(ctx, digestPrompt(tag), chunk, false, summaryMaxOut)
		if err != nil {
			return "", fmt.Errorf("failed to digest part %d/%d: %w", i+1, len(chunks), err)
		}
		partials = append(partials, part)
	}
	return e.reduceSummaries(ctx, partials, digestCombinePrompt(tag))
}

// chat makes one Groq chat completions call on the strong Model. maxOut (>0)
// caps the completion via max_tokens so a request's input+output stays under
// the TPM ceiling.
func (e *EnrichService) chat(ctx context.Context, system, user string, jsonMode bool, maxOut int) (string, error) {
	return e.chatModel(ctx, e.Model, system, user, jsonMode, maxOut)
}

// chatModel makes one Groq chat completions call on the named model, letting
// cleanup passes run on the cheaper CleanModel while summary passes use Model.
func (e *EnrichService) chatModel(ctx context.Context, model, system, user string, jsonMode bool, maxOut int) (string, error) {
	if e.APIKey == "" {
		return "", fmt.Errorf("groq api key not configured (set GROQ_API_KEY)")
	}

	payload := map[string]any{
		"model":       model,
		"temperature": 0.2,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	if maxOut > 0 {
		payload["max_tokens"] = maxOut
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

// estimateTokens approximates the token count of s for budgeting against Groq's
// TPM ceiling. Non-ASCII runes (Cyrillic, CJK) cost about one token each; ASCII
// runs at roughly four chars per token. Deliberately conservative — better to
// over-chunk than to eat a non-retryable 413.
func estimateTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return other + (ascii+3)/4
}

// packLines greedily packs lines into chunks of at most maxTokens each (estimated).
// A single line over the budget becomes its own chunk.
func packLines(lines []string, maxTokens int) []string {
	var chunks []string
	var current strings.Builder
	curTokens := 0
	for _, line := range lines {
		lineTokens := estimateTokens(line)
		if current.Len() > 0 && curTokens+lineTokens+1 > maxTokens {
			chunks = append(chunks, current.String())
			current.Reset()
			curTokens = 0
		}
		if current.Len() > 0 {
			current.WriteString("\n")
			curTokens++
		}
		current.WriteString(line)
		curTokens += lineTokens
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}

// groupByTokens greedily packs items into groups whose combined estimated tokens
// stay under maxTokens. Each group is later joined and fed to one LLM call, so
// this bounds a map-reduce combine step against the TPM ceiling. An item already
// over budget forms its own group.
func groupByTokens(items []string, maxTokens int) [][]string {
	var groups [][]string
	var cur []string
	curTokens := 0
	for _, it := range items {
		t := estimateTokens(it)
		if len(cur) > 0 && curTokens+t > maxTokens {
			groups = append(groups, cur)
			cur = nil
			curTokens = 0
		}
		cur = append(cur, it)
		curTokens += t
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
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

// summaryShape describes the target size/structure for a given length preset,
// shared by the single-pass and map-reduce combine prompts.
func summaryShape(length string) string {
	switch length {
	case SummaryShort:
		return `сжатый конспект: 3-5 предложений общего саммари, затем 3-5 ключевых мыслей буллетами`
	case SummaryLong:
		return `подробный конспект: развёрнутое саммари, структурированное по разделам/подтемам (с подзаголовками), под каждым — ключевые мысли буллетами с важными деталями, примерами и цифрами`
	default:
		return `конспект: 5-10 предложений общего саммари, затем список ключевых мыслей буллетами`
	}
}

func summaryPrompt(length string) string {
	return fmt.Sprintf(`Сделай %s на русском языке.
Если в тексте есть таймкоды [MM:SS] — начинай каждый буллет с таймкода соответствующего блока.
Пиши по существу, без вводных фраз про "этот текст" и "автор рассказывает".`, summaryShape(length))
}

func partialSummaryPrompt() string {
	return `Сделай краткий конспект фрагмента текста на русском языке: главные мысли и факты, 5-8 предложений. Без вводных фраз.`
}

func combineSummaryPrompt(length string) string {
	return fmt.Sprintf(`Ниже конспекты последовательных фрагментов одного выпуска, разделённые "---".
Собери из них единый %s на русском. Убери повторы.`, summaryShape(length))
}

func selectRelevantPrompt(topic string) string {
	return fmt.Sprintf(`Ниже каталог транскриптов, по одному на строку: id, название, теги (через таб).
Выбери те, что относятся к теме «%s». Отбирай по смыслу, а не по буквальному совпадению слов.
Ответь строго JSON-объектом: {"ids": ["id1", "id2"]}. Если ничего не подходит — {"ids": []}.`, topic)
}

func digestPrompt(tag string) string {
	return fmt.Sprintf(`Ниже — конспекты нескольких выпусков по теме «%s» (и, возможно, предыдущая версия сводного конспекта).
Собери единый тематический конспект на русском:
- структурируй по подтемам, а не по выпускам;
- синтезируй: сопоставляй позиции разных выпусков, убирай повторы;
- где важно, указывай источник в скобках по названию выпуска;
- в конце — блок «Открытые вопросы», если по теме остались противоречия.
Не добавляй вводных фраз про "этот текст".`, tag)
}

func digestCombinePrompt(tag string) string {
	return fmt.Sprintf(`Ниже — части сводного конспекта по теме «%s», разделённые "---".
Собери из них один цельный конспект: единая структура по подтемам, без повторов, сохрани указания на источники.`, tag)
}

func referencesPrompt() string {
	return `Найди в тексте все упоминания конкретных книг, фильмов, сериалов, людей, инструментов/продуктов, статей, компаний, подкастов и концепций.
Каждая строка текста или абзац начинается с таймкода [MM:SS] или [H:MM:SS] — укажи таймкод абзаца, где встретилось упоминание.
Ответь строго JSON-объектом:
{"references": [{"type": "книга|фильм|сериал|человек|инструмент|статья|компания|подкаст|концепция|другое", "name": "точное название", "timecode": "MM:SS", "quote": "короткая цитата-контекст"}]}
Если упоминаний нет — {"references": []}. Не выдумывай: только то, что явно названо в тексте.`
}

package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// groqAPIBase is the default OpenAI-compatible Groq endpoint
const groqAPIBase = "https://api.groq.com/openai/v1"

// TranscribeService transcribes audio files via Groq Whisper API.
// Long files are split into chunks with ffmpeg to fit the per-request size limit.
type TranscribeService struct {
	APIKey       string
	Model        string
	BaseURL      string
	ChunkSeconds int
	DurationSvc  DurationService
	client       *http.Client
}

// NewTranscribeService creates a transcription service
func NewTranscribeService(apiKey, model string, chunkSeconds int, dur DurationService) *TranscribeService {
	if model == "" {
		model = "whisper-large-v3"
	}
	if chunkSeconds == 0 {
		chunkSeconds = 600
	}
	return &TranscribeService{
		APIKey:       apiKey,
		Model:        model,
		BaseURL:      groqAPIBase,
		ChunkSeconds: chunkSeconds,
		DurationSvc:  dur,
		client:       &http.Client{Timeout: 5 * time.Minute},
	}
}

// TranscriptSegment is one Whisper segment with absolute timestamps
type TranscriptSegment struct {
	Start float64
	End   float64
	Text  string
}

// Transcript is the assembled result over all chunks
type Transcript struct {
	Segments    []TranscriptSegment
	Language    string
	DurationSec float64
}

// groqVerboseResp is the verbose_json response from the transcriptions endpoint
type groqVerboseResp struct {
	Text     string  `json:"text"`
	Language string  `json:"language"`
	Duration float64 `json:"duration"`
	Segments []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	} `json:"segments"`
}

// Transcribe chunks the audio file and transcribes each chunk, assembling segments
// with absolute timestamps. progress is called after each transcribed chunk.
func (s *TranscribeService) Transcribe(ctx context.Context, audioPath string, progress func(done, total int)) (*Transcript, error) {
	if s.APIKey == "" {
		return nil, fmt.Errorf("groq api key not configured (set GROQ_API_KEY)")
	}

	workDir, err := os.MkdirTemp(os.TempDir(), "notes-chunks-")
	if err != nil {
		return nil, fmt.Errorf("failed to create work dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	chunks, err := s.chunkAudio(ctx, audioPath, workDir)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk audio: %w", err)
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("ffmpeg produced no chunks for %s", audioPath)
	}

	res := &Transcript{}
	offset := 0.0
	for i, chunkPath := range chunks {
		resp, err := s.transcribeChunk(ctx, chunkPath)
		if err != nil {
			return nil, fmt.Errorf("failed to transcribe chunk %d/%d: %w", i+1, len(chunks), err)
		}
		if res.Language == "" {
			res.Language = resp.Language
		}
		for _, seg := range resp.Segments {
			res.Segments = append(res.Segments, TranscriptSegment{
				Start: seg.Start + offset,
				End:   seg.End + offset,
				Text:  seg.Text,
			})
		}
		// accumulate measured chunk duration so timestamp drift never builds up
		offset += float64(s.DurationSvc.File(chunkPath))
		if progress != nil {
			progress(i+1, len(chunks))
		}
	}
	res.DurationSec = offset
	return res, nil
}

// chunkAudio splits audio into ChunkSeconds-long mono 16kHz mp3 pieces.
// 10 min @ 48kbps mono is ~3.6MB, well under Groq's 25MB request limit.
func (s *TranscribeService) chunkAudio(ctx context.Context, audioPath, workDir string) ([]string, error) {
	ffmpegCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	outPattern := filepath.Join(workDir, "chunk-%04d.mp3")
	cmd := exec.CommandContext(ffmpegCtx, "ffmpeg", "-nostdin", "-i", audioPath,
		"-ar", "16000", "-ac", "1", "-b:a", "48k",
		"-f", "segment", "-segment_time", strconv.Itoa(s.ChunkSeconds), outPattern)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	log.Printf("[DEBUG] chunking audio %s into %ds segments", audioPath, s.ChunkSeconds)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg failed: %w, stderr: %s", err, lastLines(stderr.String(), 5))
	}

	chunks, err := filepath.Glob(filepath.Join(workDir, "chunk-*.mp3"))
	if err != nil {
		return nil, fmt.Errorf("failed to glob chunks: %w", err)
	}
	sort.Strings(chunks)
	return chunks, nil
}

// transcribeChunk sends one chunk to the Groq transcriptions endpoint
func (s *TranscribeService) transcribeChunk(ctx context.Context, chunkPath string) (*groqVerboseResp, error) {
	build := func() (*http.Request, error) {
		f, err := os.Open(chunkPath) // nolint
		if err != nil {
			return nil, fmt.Errorf("failed to open chunk: %w", err)
		}
		defer f.Close()

		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, err := mw.CreateFormFile("file", filepath.Base(chunkPath))
		if err != nil {
			return nil, fmt.Errorf("failed to create form file: %w", err)
		}
		if _, err := io.Copy(part, f); err != nil {
			return nil, fmt.Errorf("failed to copy chunk data: %w", err)
		}
		_ = mw.WriteField("model", s.Model)
		_ = mw.WriteField("response_format", "verbose_json")
		_ = mw.WriteField("timestamp_granularities[]", "segment")
		if err := mw.Close(); err != nil {
			return nil, fmt.Errorf("failed to finalize multipart body: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, "POST", s.BaseURL+"/audio/transcriptions", bytes.NewReader(body.Bytes()))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+s.APIKey)
		return req, nil
	}

	resp, err := doWithRetry(ctx, s.client, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result groqVerboseResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &result, nil
}

// llmRate keeps the latest rate-limit snapshot from Groq response headers,
// surfaced in /status so quota exhaustion is visible, not mysterious
var llmRate struct {
	mu               sync.Mutex
	remaining, limit string
	at               time.Time
}

// captureRateHeaders records x-ratelimit-* when present (Groq sends them on
// every response; Notion doesn't, so this is a no-op there)
func captureRateHeaders(h http.Header) {
	remaining := h.Get("x-ratelimit-remaining-tokens")
	if remaining == "" {
		return
	}
	llmRate.mu.Lock()
	llmRate.remaining, llmRate.limit, llmRate.at = remaining, h.Get("x-ratelimit-limit-tokens"), time.Now()
	llmRate.mu.Unlock()
}

// llmRateLine renders the /status line ("" until the first LLM call)
func llmRateLine() string {
	llmRate.mu.Lock()
	defer llmRate.mu.Unlock()
	if llmRate.remaining == "" {
		return ""
	}
	age := "только что"
	if d := time.Since(llmRate.at); d > time.Minute {
		age = fmt.Sprintf("%d мин назад", int(d.Minutes()))
	}
	return fmt.Sprintf("🧠 LLM: осталось %s из %s токенов (%s)", llmRate.remaining, llmRate.limit, age)
}

// doWithRetry executes an HTTP request built by build, retrying on 429 and 5xx.
// Honors Retry-After header when present, otherwise exponential backoff 2s..32s.
// On success the caller must close the response body.
func doWithRetry(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	const maxAttempts = 5
	backoff := 2 * time.Second

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
		} else if resp.StatusCode == http.StatusOK {
			captureRateHeaders(resp.Header)
			return resp, nil
		} else {
			captureRateHeaders(resp.Header)
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("api error (status %d): %s", resp.StatusCode, string(bodyBytes))
			if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
				return nil, lastErr // 4xx other than 429 won't get better on retry
			}
			if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
				backoff = ra
			}
		}

		if attempt == maxAttempts {
			break
		}
		log.Printf("[WARN] retrying in %v (attempt %d/%d): %v", backoff, attempt, maxAttempts, lastErr)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 32*time.Second {
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", maxAttempts, lastErr)
}

// parseRetryAfter parses the Retry-After header (seconds form only)
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

// lastLines returns up to n last non-empty lines of s, for compact error messages
func lastLines(s string, n int) string {
	lines := bytes.Split(bytes.TrimSpace([]byte(s)), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte("\n")))
}

package proc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// YandexTTS implements TTSProvider over Yandex SpeechKit (REST v1 synthesize).
// It replaced Edge TTS in prod after Microsoft started 403-ing datacenter IPs
// on the free endpoint (#81): SpeechKit is a real API that serves DCs, and the
// translate service account's Api-Key already in the container covers it once
// the ai.speechkit-tts.user role is added — no new secrets, no recreate.
type YandexTTS struct {
	apiKey   string
	Voice    string
	Lang     string
	endpoint string // test hook; empty = production endpoint
	client   *http.Client
}

const yandexTTSEndpoint = "https://tts.api.cloud.yandex.net/speech/v1/tts:synthesize"

// yandexTTSMaxChunk is SpeechKit's per-request text limit (5000); our callers
// chunk at 3000 anyway, this is the hard ceiling guard.
const yandexTTSMaxChunk = 5000

// NewYandexTTS creates the SpeechKit provider. An Edge-style voice name from
// an unchanged config (ru-RU-DmitryNeural) maps to the default male voice, so
// flipping tts_provider alone is enough.
func NewYandexTTS(apiKey, voice string) *YandexTTS {
	if voice == "" || strings.Contains(voice, "Neural") {
		voice = "filipp" // SpeechKit's neutral male Russian
	}
	return &YandexTTS{
		apiKey: apiKey,
		Voice:  voice,
		Lang:   "ru-RU",
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// Synthesize converts one chunk of text to MP3 bytes.
func (y *YandexTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if len(text) > yandexTTSMaxChunk {
		return nil, fmt.Errorf("text chunk too long for SpeechKit: %d > %d", len(text), yandexTTSMaxChunk)
	}
	form := url.Values{}
	form.Set("text", text)
	form.Set("lang", y.Lang)
	form.Set("voice", y.Voice)
	form.Set("format", "mp3")
	// folderId is deliberately NOT sent: with Api-Key auth SpeechKit derives
	// the folder from the service account itself.

	endpoint := y.endpoint
	if endpoint == "" {
		endpoint = yandexTTSEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to build SpeechKit request: %w", err)
	}
	req.Header.Set("Authorization", "Api-Key "+y.apiKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := y.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SpeechKit request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("SpeechKit returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read SpeechKit audio: %w", err)
	}
	if len(audio) == 0 {
		return nil, fmt.Errorf("SpeechKit returned empty audio")
	}
	return audio, nil
}

// SynthesizeLongText chunks long text through the shared loop.
func (y *YandexTTS) SynthesizeLongText(ctx context.Context, text string, maxChunkSize int) ([]byte, error) {
	return synthesizeLongWith(ctx, y.Synthesize, text, maxChunkSize)
}

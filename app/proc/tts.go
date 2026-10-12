package proc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/wujunwei928/edge-tts-go/edge_tts"
)

// escapeXML escapes characters that are invalid in XML/SSML content.
func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// TTSProvider interface for text-to-speech services. SynthesizeLongText is
// part of the interface so callers never need to know the backend: Edge TTS is
// 403-blocked for datacenter IPs (#81), so prod runs on Yandex SpeechKit while
// Edge remains the local/default fallback.
type TTSProvider interface {
	Synthesize(ctx context.Context, text string) ([]byte, error)
	SynthesizeLongText(ctx context.Context, text string, maxChunkSize int) ([]byte, error)
}

// EdgeTTS implements TTSProvider using Microsoft Edge TTS
type EdgeTTS struct {
	Voice string
}

// NewEdgeTTS creates a new Edge TTS provider
func NewEdgeTTS(voice string) *EdgeTTS {
	if voice == "" {
		voice = "ru-RU-DmitryNeural"
	}
	return &EdgeTTS{Voice: voice}
}

// Synthesize converts text to speech using Edge TTS
func (e *EdgeTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	text = escapeXML(text)
	comm, err := edge_tts.NewCommunicate(text, edge_tts.SetVoice(e.Voice))
	if err != nil {
		return nil, fmt.Errorf("failed to create TTS communicator: %w", err)
	}

	audioData, err := comm.Stream()
	if err != nil {
		return nil, fmt.Errorf("failed to synthesize speech: %w", err)
	}

	return audioData, nil
}

// SynthesizeToFile synthesizes text and writes to an io.Writer
func (e *EdgeTTS) SynthesizeToFile(ctx context.Context, text string, w io.Writer) error {
	data, err := e.Synthesize(ctx, text)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// SynthesizeLongText handles long text by splitting into chunks
func (e *EdgeTTS) SynthesizeLongText(ctx context.Context, text string, maxChunkSize int) ([]byte, error) {
	return synthesizeLongWith(ctx, e.Synthesize, text, maxChunkSize)
}

// synthesizeLongWith is the provider-agnostic chunking loop: split at sentence
// boundaries, synthesize with retries, concatenate the audio (MP3 frames glue
// cleanly), and pause between chunks to stay under rate limits.
func synthesizeLongWith(ctx context.Context, synth func(context.Context, string) ([]byte, error), text string, maxChunkSize int) ([]byte, error) {
	if maxChunkSize <= 0 {
		maxChunkSize = 3000 // Edge TTS has ~3000 char limit per request; SpeechKit allows 5000
	}

	chunks := splitTextIntoChunks(text, maxChunkSize)
	var result bytes.Buffer

	for i, chunk := range chunks {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Retry logic for transient errors
		var audio []byte
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				// Exponential backoff: 5s, 10s, 20s
				backoff := time.Duration(5<<attempt) * time.Second
				time.Sleep(backoff)
			}

			audio, err = synth(ctx, chunk)
			if err == nil {
				break
			}
		}
		if err != nil {
			return nil, fmt.Errorf("failed to synthesize chunk %d: %w", i, err)
		}
		result.Write(audio)

		// Delay between chunks to avoid rate limiting (2 seconds)
		if i < len(chunks)-1 {
			time.Sleep(2 * time.Second)
		}
	}

	return result.Bytes(), nil
}

// newTTSProvider picks the synthesis backend by config name. "yandex" needs the
// translate service account's key (with the ai.speechkit-tts.user role added) —
// already in the container env, no new secrets. Missing creds fall back to Edge
// with a warning so local runs keep working.
func newTTSProvider(provider, voice string) TTSProvider {
	if strings.EqualFold(provider, "yandex") {
		key := os.Getenv("YANDEX_TRANSLATE_KEY")
		if key == "" {
			log.Printf("[WARN] tts_provider=yandex but YANDEX_TRANSLATE_KEY is empty, falling back to Edge TTS")
		} else {
			return NewYandexTTS(key, voice)
		}
	}
	return NewEdgeTTS(voice)
}

// splitTextIntoChunks splits text into chunks at sentence boundaries
func splitTextIntoChunks(text string, maxSize int) []string {
	if len(text) <= maxSize {
		return []string{text}
	}

	var chunks []string
	sentences := splitIntoSentences(text)
	var current strings.Builder

	for _, sentence := range sentences {
		if current.Len()+len(sentence) > maxSize && current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
		}
		current.WriteString(sentence)
	}

	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	return chunks
}

// splitIntoSentences splits text into sentences
func splitIntoSentences(text string) []string {
	var sentences []string
	var current strings.Builder

	for _, r := range text {
		current.WriteRune(r)
		if r == '.' || r == '!' || r == '?' || r == '\n' {
			sentences = append(sentences, current.String())
			current.Reset()
		}
	}

	if current.Len() > 0 {
		sentences = append(sentences, current.String())
	}

	return sentences
}

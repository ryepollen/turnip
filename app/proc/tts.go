package proc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/wujunwei928/edge-tts-go/edge_tts"
)

// TTSProvider interface for text-to-speech services
type TTSProvider interface {
	Synthesize(ctx context.Context, text string) ([]byte, error)
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
	if maxChunkSize <= 0 {
		maxChunkSize = 3000 // Edge TTS has ~3000 char limit per request
	}

	chunks := splitTextIntoChunks(text, maxChunkSize)
	var result bytes.Buffer

	for i, chunk := range chunks {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		audio, err := e.Synthesize(ctx, chunk)
		if err != nil {
			return nil, fmt.Errorf("failed to synthesize chunk %d: %w", i, err)
		}
		result.Write(audio)
	}

	return result.Bytes(), nil
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

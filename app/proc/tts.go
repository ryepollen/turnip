package proc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	trustedClientToken  = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	chromiumFullVersion = "134.0.3124.66"
	secMSGECVersion     = "1-" + chromiumFullVersion
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

// generateSecMSGEC generates the Sec-MS-GEC security token required by Microsoft
// Algorithm based on https://github.com/wujunwei928/edge-tts-go
func generateSecMSGEC() string {
	// Get current Unix timestamp as float (for precision)
	ticks := float64(time.Now().UTC().UnixNano()) / 1e9

	// Convert to Windows epoch (add seconds from 1601 to 1970)
	ticks += 11644473600

	// Round down to nearest 5 minutes (300 seconds)
	ticks -= float64(int64(ticks) % 300)

	// Convert to 100-nanosecond intervals
	ticks *= 1e9 / 100 // = 1e7

	// Round down to nearest 1e9 (keeps first 9 digits, rest are zeros)
	ticksRounded := int64(ticks/1e9) * int64(1e9)

	// Create hash input: rounded ticks + trusted client token
	input := fmt.Sprintf("%d%s", ticksRounded, trustedClientToken)

	// Generate SHA256 hash
	hash := sha256.Sum256([]byte(input))

	// Return uppercase hex
	return strings.ToUpper(hex.EncodeToString(hash[:]))
}

// Synthesize converts text to speech using Edge TTS
func (e *EdgeTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	// Generate connection ID and security token
	connectionID := strings.ReplaceAll(uuid.New().String(), "-", "")
	secGEC := generateSecMSGEC()

	// Edge TTS WebSocket endpoint with security parameters
	wsURL := fmt.Sprintf(
		"wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1?TrustedClientToken=%s&ConnectionId=%s&Sec-MS-GEC=%s&Sec-MS-GEC-Version=%s",
		trustedClientToken, connectionID, secGEC, secMSGECVersion,
	)

	// Create connection
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	header := http.Header{}
	header.Set("Origin", "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold")
	header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/"+chromiumFullVersion+" Safari/537.36 Edg/"+chromiumFullVersion)
	header.Set("Pragma", "no-cache")
	header.Set("Cache-Control", "no-cache")

	conn, _, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Edge TTS: %w", err)
	}
	defer conn.Close()

	requestID := connectionID // reuse connection ID as request ID

	// Send configuration
	configMsg := fmt.Sprintf(
		"X-Timestamp:%s\r\nContent-Type:application/json; charset=utf-8\r\nPath:speech.config\r\n\r\n"+
			`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},"outputFormat":"audio-24khz-48kbitrate-mono-mp3"}}}}`,
		time.Now().UTC().Format("Jan 02 2006 15:04:05"),
	)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(configMsg)); err != nil {
		return nil, fmt.Errorf("failed to send config: %w", err)
	}

	// Send SSML request
	ssml := e.buildSSML(text)
	ssmlMsg := fmt.Sprintf(
		"X-RequestId:%s\r\nContent-Type:application/ssml+xml\r\nX-Timestamp:%s\r\nPath:ssml\r\n\r\n%s",
		requestID,
		time.Now().UTC().Format("Jan 02 2006 15:04:05"),
		ssml,
	)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(ssmlMsg)); err != nil {
		return nil, fmt.Errorf("failed to send SSML: %w", err)
	}

	// Collect audio data
	var audioData bytes.Buffer
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				break
			}
			return nil, fmt.Errorf("failed to read message: %w", err)
		}

		if msgType == websocket.TextMessage {
			msg := string(data)
			if strings.Contains(msg, "Path:turn.end") {
				break
			}
		} else if msgType == websocket.BinaryMessage {
			// Binary message contains audio data after header
			if len(data) > 2 {
				headerLen := binary.BigEndian.Uint16(data[:2])
				if int(headerLen)+2 <= len(data) {
					audioData.Write(data[headerLen+2:])
				}
			}
		}
	}

	if audioData.Len() == 0 {
		return nil, fmt.Errorf("no audio data received")
	}

	return audioData.Bytes(), nil
}

// buildSSML creates SSML markup for the text
func (e *EdgeTTS) buildSSML(text string) string {
	// Escape XML special characters
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")
	text = strings.ReplaceAll(text, "\"", "&quot;")
	text = strings.ReplaceAll(text, "'", "&apos;")

	return fmt.Sprintf(
		`<speak version="1.0" xmlns="http://www.w3.org/2001/10/synthesis" xmlns:mstts="https://www.w3.org/2001/mstts" xml:lang="ru-RU">`+
			`<voice name="%s">`+
			`<prosody rate="0%%" pitch="0Hz">%s</prosody>`+
			`</voice></speak>`,
		e.Voice, text,
	)
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

		// Small delay between requests to avoid rate limiting
		if i < len(chunks)-1 {
			time.Sleep(100 * time.Millisecond)
		}
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

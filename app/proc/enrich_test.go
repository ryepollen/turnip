package proc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderSegments(t *testing.T) {
	segs := []TranscriptSegment{
		{Start: 0, End: 4.5, Text: " hello there "},
		{Start: 4.5, End: 10, Text: ""},
		{Start: 65.2, End: 70, Text: "next block"},
		{Start: 3725, End: 3730, Text: "over an hour"},
	}
	got := renderSegments(segs)
	want := "[00:00] hello there\n[01:05] next block\n[1:02:05] over an hour"
	assert.Equal(t, want, got)
}

func TestFormatTimecode(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "[00:00]"},
		{59.9, "[00:59]"},
		{60, "[01:00]"},
		{754, "[12:34]"},
		{3600, "[1:00:00]"},
		{3905, "[1:05:05]"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, formatTimecode(tt.in), "in=%v", tt.in)
	}
}

func TestPackLines(t *testing.T) {
	tests := []struct {
		name     string
		lines    []string
		maxChars int
		want     []string
	}{
		{"all fit", []string{"aa", "bb"}, 100, []string{"aa\nbb"}},
		{"split at boundary", []string{"aaaa", "bbbb", "cccc"}, 9, []string{"aaaa\nbbbb", "cccc"}},
		{"oversized line own chunk", []string{"aa", "bbbbbbbbbb", "cc"}, 5, []string{"aa", "bbbbbbbbbb", "cc"}},
		{"empty input", nil, 10, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, packLines(tt.lines, tt.maxChars))
		})
	}
}

func TestTailHeadChars(t *testing.T) {
	assert.Equal(t, "вет", tailChars("привет", 3), "no rune splitting")
	assert.Equal(t, "привет", tailChars("привет", 10))
	assert.Equal(t, "при", headChars("привет", 3))
	assert.Equal(t, "привет", headChars("привет", 10))
}

func TestPromptBuilders(t *testing.T) {
	assert.NotContains(t, cleanupPrompt(""), "Конец предыдущего фрагмента")
	assert.Contains(t, cleanupPrompt("хвост"), "хвост")
	assert.Contains(t, referencesPrompt(), `"references"`)
	assert.Contains(t, metaPrompt(), `"tags"`)
}

// mockGroqChat returns a chat completions server that responds with content
// produced by fn from the incoming user message
func mockGroqChat(t *testing.T, fn func(userMsg string, jsonMode bool) string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			ResponseFormat *struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Len(t, req.Messages, 2)
		content := fn(req.Messages[1].Content, req.ResponseFormat != nil)
		resp := map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": content}}},
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
}

func TestCleanTranscriptChunked(t *testing.T) {
	var calls int32
	ts := mockGroqChat(t, func(userMsg string, jsonMode bool) string {
		assert.False(t, jsonMode)
		atomic.AddInt32(&calls, 1)
		return "cleaned: " + strings.Split(userMsg, "\n")[0]
	})
	defer ts.Close()

	svc := NewEnrichService("test-key", "")
	svc.BaseURL = ts.URL

	// build enough segments to force at least 2 chunks of enrichChunkSize
	var segs []TranscriptSegment
	for i := 0; i < 200; i++ {
		segs = append(segs, TranscriptSegment{Start: float64(i * 30), Text: strings.Repeat("слово ", 20)})
	}

	var progressCalls int
	got, err := svc.CleanTranscript(context.Background(), &Transcript{Segments: segs}, func(done, total int) {
		progressCalls++
		assert.LessOrEqual(t, done, total)
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&calls), int32(2), "long transcript must be chunked")
	assert.Equal(t, int(atomic.LoadInt32(&calls)), progressCalls)
	assert.Contains(t, got, "cleaned: [00:00]")
}

func TestExtractMeta(t *testing.T) {
	ts := mockGroqChat(t, func(userMsg string, jsonMode bool) string {
		assert.True(t, jsonMode)
		assert.Contains(t, userMsg, "My Title")
		return `{"tags":["one","two","three","four","five"],"lang":"ru"}`
	})
	defer ts.Close()

	svc := NewEnrichService("test-key", "")
	svc.BaseURL = ts.URL

	meta, err := svc.ExtractMeta(context.Background(), "My Title", "My Channel", "текст")
	require.NoError(t, err)
	assert.Equal(t, "ru", meta.Lang)
	assert.Len(t, meta.Tags, 4, "tags capped at 4")
}

func TestExtractReferencesMergeAndMalformed(t *testing.T) {
	var calls int32
	ts := mockGroqChat(t, func(userMsg string, jsonMode bool) string {
		assert.True(t, jsonMode)
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			return `{"references":[{"type":"книга","name":"Дюна","timecode":"01:00","quote":"q1"},
				{"type":"человек","name":"Herbert","timecode":"02:00","quote":"q2"}]}`
		case 2:
			return `not a json at all`
		default:
			return `{"references":[{"type":"книга","name":"дюна","timecode":"55:00","quote":"dup"},
				{"type":"инструмент","name":"Figma","timecode":"56:00","quote":"q3"}]}`
		}
	})
	defer ts.Close()

	svc := NewEnrichService("test-key", "")
	svc.BaseURL = ts.URL

	// force exactly 3 chunks
	lines := []string{
		strings.Repeat("a", enrichChunkSize-10),
		strings.Repeat("b", enrichChunkSize-10),
		strings.Repeat("c", enrichChunkSize-10),
	}
	refs, err := svc.ExtractReferences(context.Background(), strings.Join(lines, "\n"))
	require.NoError(t, err)
	require.Len(t, refs, 3, "dup 'дюна' dropped, malformed chunk skipped")
	assert.Equal(t, "Дюна", refs[0].Name)
	assert.Equal(t, "01:00", refs[0].Timecode, "first occurrence wins")
	assert.Equal(t, "Figma", refs[2].Name)
}

func TestSummaryPromptLength(t *testing.T) {
	// each preset must yield a distinct shape; normal is the "" default
	normal := summaryPrompt("")
	short := summaryPrompt(SummaryShort)
	long := summaryPrompt(SummaryLong)
	assert.Contains(t, short, "3-5")
	assert.Contains(t, long, "по разделам")
	assert.Contains(t, normal, "5-10")
	assert.NotEqual(t, normal, short)
	assert.NotEqual(t, normal, long)

	// unknown length falls back to normal
	assert.Equal(t, normal, summaryPrompt("bogus"))
	// combine prompt tracks the same shape
	assert.Contains(t, combineSummaryPrompt(SummaryShort), "3-5")
}

func TestSummarizeMapReduce(t *testing.T) {
	var calls int32
	ts := mockGroqChat(t, func(userMsg string, jsonMode bool) string {
		n := atomic.AddInt32(&calls, 1)
		if strings.Contains(userMsg, "---") {
			return "combined summary"
		}
		return "partial " + string(rune('0'+n))
	})
	defer ts.Close()

	svc := NewEnrichService("test-key", "")
	svc.BaseURL = ts.URL

	short, err := svc.Summarize(context.Background(), "короткий текст", "")
	require.NoError(t, err)
	assert.Equal(t, "partial 1", short, "single pass for short text")

	long := strings.Repeat(strings.Repeat("x", 1000)+"\n", 60) // 60k chars → map-reduce
	combined, err := svc.Summarize(context.Background(), long, "")
	require.NoError(t, err)
	assert.Equal(t, "combined summary", combined)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&calls), int32(4), "partials + combine")
}

package proc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranscribeChunk(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/audio/transcriptions", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		require.NoError(t, r.ParseMultipartForm(32<<20))
		assert.Equal(t, "whisper-large-v3", r.FormValue("model"))
		assert.Equal(t, "verbose_json", r.FormValue("response_format"))
		assert.Equal(t, "segment", r.FormValue("timestamp_granularities[]"))
		_, _, err := r.FormFile("file")
		require.NoError(t, err)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello world","language":"en","duration":12.5,
			"segments":[{"start":0.0,"end":5.2,"text":"hello"},{"start":5.2,"end":12.5,"text":"world"}]}`))
	}))
	defer ts.Close()

	chunk := filepath.Join(t.TempDir(), "chunk-0000.mp3")
	require.NoError(t, os.WriteFile(chunk, []byte("fake mp3 data"), 0o600))

	svc := NewTranscribeService("test-key", "", 0, nil)
	svc.BaseURL = ts.URL

	resp, err := svc.transcribeChunk(context.Background(), chunk)
	require.NoError(t, err)
	assert.Equal(t, "en", resp.Language)
	require.Len(t, resp.Segments, 2)
	assert.Equal(t, 5.2, resp.Segments[1].Start)
	assert.Equal(t, "world", resp.Segments[1].Text)
}

func TestDoWithRetry429ThenOK(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0.01")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`ok`))
	}))
	defer ts.Close()

	build := func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), "GET", ts.URL, http.NoBody)
	}
	resp, err := doWithRetry(context.Background(), ts.Client(), build)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

func TestDoWithRetryFatal4xx(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad model"}`))
	}))
	defer ts.Close()

	build := func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), "GET", ts.URL, http.NoBody)
	}
	_, err := doWithRetry(context.Background(), ts.Client(), build)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 400")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "no retries on non-429 4xx")
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty", "", 0},
		{"seconds", "5", 5 * time.Second},
		{"fractional", "0.5", 500 * time.Millisecond},
		{"http date unsupported", "Wed, 21 Oct 2026 07:28:00 GMT", 0},
		{"negative", "-3", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseRetryAfter(tt.in))
		})
	}
}

func TestLastLines(t *testing.T) {
	assert.Equal(t, "c\nd", lastLines("a\nb\nc\nd", 2))
	assert.Equal(t, "a", lastLines("a", 5))
	assert.Equal(t, "", lastLines("", 3))
}

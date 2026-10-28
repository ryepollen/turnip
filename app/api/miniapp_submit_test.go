package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanTrackingParams(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"youtube si+feature", "https://youtu.be/abc123?si=XyZ&feature=share&t=42", "https://youtu.be/abc123?t=42"},
		{"utm pack", "https://example.com/a?utm_source=tg&utm_medium=social&id=7", "https://example.com/a?id=7"},
		{"content params survive", "https://www.youtube.com/watch?v=abc&list=PL1", "https://www.youtube.com/watch?v=abc&list=PL1"},
		{"apple episode id survives", "https://podcasts.apple.com/us/podcast/x/id123?i=456", "https://podcasts.apple.com/us/podcast/x/id123?i=456"},
		{"no scheme untouched", "not a url", "not a url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cleanTrackingParams(tt.in))
		})
	}
}

func TestAppSubmit(t *testing.T) {
	post := func(s *Server, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/wegweiser/api/submit", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		s.appSubmitCtrl(rec, req)
		return rec
	}

	t.Run("audio goes to the hook with a cleaned url", func(t *testing.T) {
		var got string
		s := &Server{SubmitAudio: func(u string) error { got = u; return nil }}
		rec := post(s, `{"url":"https://youtu.be/abc?si=TRACK","action":"audio"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "https://youtu.be/abc", got, "tracking params must not reach the pipelines")
	})

	t.Run("audio without the bot is 503", func(t *testing.T) {
		rec := post(&Server{}, `{"url":"https://youtu.be/abc","action":"audio"}`)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	})

	t.Run("unknown action is 400", func(t *testing.T) {
		rec := post(&Server{}, `{"url":"https://x.org","action":"teleport"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("empty url is 400", func(t *testing.T) {
		rec := post(&Server{}, `{"url":"  ","action":"notes"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("notes without the service is 503", func(t *testing.T) {
		rec := post(&Server{}, `{"url":"https://x.org","action":"notes"}`)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	})
}

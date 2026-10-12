package proc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestYandexTTSSynthesize(t *testing.T) {
	var gotAuth, gotForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotForm = string(b)
		_, _ = w.Write([]byte("MP3BYTES"))
	}))
	defer srv.Close()

	y := NewYandexTTS("key-123", "")
	y.endpoint = srv.URL

	audio, err := y.Synthesize(t.Context(), "привет & <мир>")
	require.NoError(t, err)
	assert.Equal(t, "MP3BYTES", string(audio))
	assert.Equal(t, "Api-Key key-123", gotAuth)
	assert.Contains(t, gotForm, "voice=filipp", "empty voice maps to the default male")
	assert.Contains(t, gotForm, "lang=ru-RU")
	assert.Contains(t, gotForm, "format=mp3")
	assert.NotContains(t, gotForm, "folderId", "Api-Key auth derives the folder itself")
	assert.Contains(t, gotForm, "%D0%BF%D1%80%D0%B8%D0%B2%D0%B5%D1%82", "text rides form-encoded, no SSML escaping")
}

func TestYandexTTSVoiceMapping(t *testing.T) {
	assert.Equal(t, "filipp", NewYandexTTS("k", "ru-RU-DmitryNeural").Voice,
		"an Edge voice left in config maps to the SpeechKit default — flipping tts_provider alone is enough")
	assert.Equal(t, "alena", NewYandexTTS("k", "alena").Voice, "an explicit SpeechKit voice is kept")
}

func TestYandexTTSErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error_code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	y := NewYandexTTS("sekret-key", "filipp")
	y.endpoint = srv.URL

	_, err := y.Synthesize(t.Context(), "текст")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.NotContains(t, err.Error(), "sekret-key", "the key never leaks into errors/logs")

	_, err = y.Synthesize(t.Context(), strings.Repeat("а", 10001))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too long")
}

func TestNewTTSProviderSelection(t *testing.T) {
	t.Setenv("YANDEX_TRANSLATE_KEY", "")
	_, isEdge := newTTSProvider("yandex", "x").(*EdgeTTS)
	assert.True(t, isEdge, "yandex without creds falls back to Edge")

	t.Setenv("YANDEX_TRANSLATE_KEY", "k")
	_, isYandex := newTTSProvider("yandex", "").(*YandexTTS)
	assert.True(t, isYandex)

	_, isEdge = newTTSProvider("", "v").(*EdgeTTS)
	assert.True(t, isEdge, "empty/default = Edge, deploy is inert until config flips")
}

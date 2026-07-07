package publisher

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestR2ConfigEnabled(t *testing.T) {
	assert.False(t, R2Config{}.Enabled())
	full := R2Config{AccountID: "a", AccessKeyID: "k", SecretKey: "s", Bucket: "b", PublicBaseURL: "https://pub.example"}
	assert.True(t, full.Enabled())
	partial := full
	partial.Bucket = ""
	assert.False(t, partial.Enabled())
}

func TestRandomSecret(t *testing.T) {
	a, b := RandomSecret(), RandomSecret()
	assert.Len(t, a, 16)
	assert.NotEqual(t, a, b)
}

func TestPublicURLEscaping(t *testing.T) {
	s := &R2Store{publicBase: "https://pub.example"}
	assert.Equal(t, "https://pub.example/a/deadbeef/lesson-01.mp3", s.PublicURL("a/deadbeef/lesson-01.mp3"))
	assert.Equal(t, "https://pub.example/a/x/%D0%93%D0%BB%D0%B0%D0%B2%D0%B0%201.mp3", s.PublicURL("a/x/Глава 1.mp3"))
}

func TestUploadAgainstFakeS3(t *testing.T) {
	var gotPath, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			gotPath = r.URL.Path
			gotContentType = r.Header.Get("Content-Type")
			w.Header().Set("ETag", `"fake-etag"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := R2Config{AccountID: "acc", AccessKeyID: "key", SecretKey: "secret", Bucket: "turnip", PublicBaseURL: "https://pub.example"}
	store, err := newR2StoreForEndpoint(strings.TrimPrefix(srv.URL, "http://"), cfg)
	require.NoError(t, err)

	f := filepath.Join(t.TempDir(), "test.mp3")
	require.NoError(t, os.WriteFile(f, []byte("fake audio bytes"), 0o600))

	url, err := store.Upload(t.Context(), f, "a/deadbeef/test.mp3", "")
	require.NoError(t, err)
	assert.Equal(t, "https://pub.example/a/deadbeef/test.mp3", url)
	assert.Equal(t, "/turnip/a/deadbeef/test.mp3", gotPath)
	assert.Equal(t, "audio/mpeg", gotContentType)
}

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

func TestFeedMediaKeysAndPublicBase(t *testing.T) {
	fm := &FeedMedia{Store: &R2Store{publicBase: "https://pub.example"}, Secret: "sec"}
	assert.Equal(t, "m/sec/ep.mp3", fm.key("/srv/var/yt/ep.mp3"))
	assert.Equal(t, "https://pub.example/m/sec", fm.PublicBase())
}

func TestTotalSizeAgainstFakeS3(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult><Name>turnip</Name><IsTruncated>false</IsTruncated>
<Contents><Key>a/x/one.mp3</Key><Size>1000</Size></Contents>
<Contents><Key>m/x/two.mp3</Key><Size>2500</Size></Contents>
</ListBucketResult>`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := R2Config{AccountID: "acc", AccessKeyID: "k", SecretKey: "s", Bucket: "turnip", PublicBaseURL: "https://pub.example"}
	store, err := newR2StoreForEndpoint(strings.TrimPrefix(srv.URL, "http://"), cfg)
	require.NoError(t, err)

	total, err := store.TotalSize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(3500), total)
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

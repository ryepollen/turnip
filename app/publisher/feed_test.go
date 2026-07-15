package publisher

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTrackName(t *testing.T) {
	tests := []struct {
		in    string
		order int
		title string
	}{
		{"03 - Введение в добычу.mp3", 3, "Введение в добычу"},
		{"3-Intro.mp3", 3, "Intro"},
		{"12. Chapter Twelve.m4a", 12, "Chapter Twelve"},
		{"No Number Here.mp3", 0, "No Number Here"},
		{"2024 retrospective.mp3", 0, "2024 retrospective"}, // year, not a track: no separator
	}
	for _, tt := range tests {
		order, title := parseTrackName(tt.in)
		assert.Equal(t, tt.order, order, tt.in)
		assert.Equal(t, tt.title, title, tt.in)
	}
}

func TestLoadFeedConfigDefaults(t *testing.T) {
	cfg := LoadFeedConfig(t.TempDir(), "books")
	assert.Equal(t, "books", cfg.Title)
	assert.Equal(t, "ru", cfg.Language)
	assert.Equal(t, "serial", cfg.Type)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "feed.yaml"),
		[]byte("title: Аудиокниги\ntype: episodic\nauthor: Я"), 0o600))
	cfg = LoadFeedConfig(dir, "books")
	assert.Equal(t, "Аудиокниги", cfg.Title)
	assert.Equal(t, "episodic", cfg.Type)
	assert.Equal(t, "Я", cfg.Author)
}

func TestBuildFeedXMLSerialOrder(t *testing.T) {
	eps := []Episode{
		{File: "02 - Two.mp3", Title: "Two", Order: 2, R2Key: "a/s/c/02.mp3", PublicURL: "https://pub/02.mp3", SizeBytes: 200, DurationSec: 120},
		{File: "01 - One.mp3", Title: "One", Order: 1, R2Key: "a/s/c/01.mp3", PublicURL: "https://pub/01.mp3", SizeBytes: 100, DurationSec: 60},
		{File: "10 - Ten.mp3", Title: "Ten", Order: 10, R2Key: "a/s/c/10.mp3", PublicURL: "https://pub/10.mp3", SizeBytes: 1000, DurationSec: 600},
	}
	cfg := FeedConfig{Title: "Курс «Нефть & Газ»", Type: "serial", Language: "ru", Cover: "https://pub/cover.jpg"}

	data, err := BuildFeedXML(cfg, eps)
	require.NoError(t, err)
	xml := string(data)

	// order: 1, 2, 10 (numeric, not lexicographic)
	i1, i2, i10 := strings.Index(xml, "<title>One</title>"), strings.Index(xml, "<title>Two</title>"), strings.Index(xml, "<title>Ten</title>")
	require.True(t, i1 > 0 && i2 > 0 && i10 > 0)
	assert.True(t, i1 < i2 && i2 < i10, "serial order must be numeric")

	// synthetic dates keep the order for players sorting by pubDate
	assert.Contains(t, xml, "Wed, 01 Jan 2020 12:00:00 +0000")
	assert.Contains(t, xml, "Wed, 01 Jan 2020 12:01:00 +0000")
	assert.Contains(t, xml, `<itunes:type>serial</itunes:type>`)
	assert.Contains(t, xml, `<itunes:episode>1</itunes:episode>`)
	assert.Contains(t, xml, `<enclosure url="https://pub/01.mp3" length="100" type="audio/mpeg">`)
	assert.Contains(t, xml, `<itunes:duration>00:02:00</itunes:duration>`)
	assert.Contains(t, xml, "Курс «Нефть &amp; Газ»", "title escaped")
	assert.Contains(t, xml, `isPermaLink="false"`)
}

func TestBuildFeedXMLEpisodicNewestFirst(t *testing.T) {
	old := Episode{File: "old.mp3", Title: "Old", PublicURL: "https://pub/old.mp3", PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	fresh := Episode{File: "new.mp3", Title: "New", PublicURL: "https://pub/new.mp3", PublishedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}

	data, err := BuildFeedXML(FeedConfig{Title: "pods", Type: "episodic"}, []Episode{old, fresh})
	require.NoError(t, err)
	xml := string(data)
	assert.True(t, strings.Index(xml, "<title>New</title>") < strings.Index(xml, "<title>Old</title>"))
	assert.NotContains(t, xml, "2020", "no synthetic dates for episodic feeds")
}

func TestEpisodeGUIDStable(t *testing.T) {
	e := Episode{R2Key: "a/secret/books/01.mp3"}
	assert.Equal(t, e.GUID(), e.GUID())
	assert.True(t, strings.HasPrefix(e.GUID(), "turnip-"))
	e2 := Episode{R2Key: "a/secret/books/02.mp3"}
	assert.NotEqual(t, e.GUID(), e2.GUID())
}

type fakeDuration struct{}

func (fakeDuration) File(string) int { return 90 }

func TestServicePublishAndRegenerate(t *testing.T) {
	var puts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
			w.Header().Set("ETag", `"e"`)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := R2Config{AccountID: "acc", AccessKeyID: "k", SecretKey: "s", Bucket: "turnip", PublicBaseURL: "https://pub.example"}
	r2, err := newR2StoreForEndpoint(strings.TrimPrefix(srv.URL, "http://"), cfg)
	require.NoError(t, err)

	audioDir := t.TempDir()
	svc := &Service{R2: r2, AudioDir: audioDir, Secret: "s3cr3t", Duration: fakeDuration{}, BaseURL: "http://vm:8080"}

	src := filepath.Join(t.TempDir(), "01 - Глава первая.mp3")
	require.NoError(t, os.WriteFile(src, []byte("audio"), 0o600))

	ep, err := svc.PublishFile(t.Context(), src, "books")
	require.NoError(t, err)
	assert.Equal(t, "Глава первая", ep.Title)
	assert.Equal(t, 1, ep.Order)
	assert.Equal(t, 90, ep.DurationSec)
	assert.Equal(t, "a/s3cr3t/books/01 - Глава первая.mp3", ep.R2Key)
	assert.Equal(t, 1, puts)

	// idempotent: same file again → no second upload
	ep2, err := svc.PublishFile(t.Context(), src, "books")
	require.NoError(t, err)
	assert.Equal(t, ep.R2Key, ep2.R2Key)
	assert.Equal(t, 1, puts, "no re-upload for a known file")

	// feed.xml generated and lists the episode
	feedData, err := os.ReadFile(filepath.Join(audioDir, "feeds", "books.xml"))
	require.NoError(t, err)
	assert.Contains(t, string(feedData), "Глава первая")
	assert.Contains(t, string(feedData), "https://pub.example/a/s3cr3t/books/01%20-%20%D0%93")

	// subscription URL
	assert.Equal(t, "http://vm:8080/holzweg/s3cr3t/books.xml", svc.FeedURL("books"))

	cats, err := svc.Categories()
	require.NoError(t, err)
	assert.Equal(t, []string{"books"}, cats)

	// path traversal guard
	_, err = svc.PublishFile(t.Context(), src, "../evil")
	require.Error(t, err)
}

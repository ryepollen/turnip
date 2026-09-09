package publisher

import (
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
	// normalize is off by default for every category (decision B): under variant A
	// the VM never reads the bytes, so loudnorm can't run on the active path
	assert.False(t, cfg.NormalizeEnabled(), "books default to normalize off")
	assert.False(t, LoadFeedConfig(t.TempDir(), "courses").NormalizeEnabled(),
		"courses/podcasts also default to normalize off in variant A")

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "feed.yaml"),
		[]byte("title: Аудиокниги\ntype: episodic\nauthor: Я"), 0o600))
	cfg = LoadFeedConfig(dir, "books")
	assert.Equal(t, "Аудиокниги", cfg.Title)
	assert.Equal(t, "episodic", cfg.Type)
	assert.Equal(t, "Я", cfg.Author)

	// an explicit normalize: in feed.yaml overrides the category default both ways
	on := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(on, "feed.yaml"), []byte("normalize: true"), 0o600))
	assert.True(t, LoadFeedConfig(on, "books").NormalizeEnabled(), "explicit true wins for books")

	off := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(off, "feed.yaml"), []byte("normalize: false"), 0o600))
	assert.False(t, LoadFeedConfig(off, "courses").NormalizeEnabled(), "explicit false wins for courses")
}

func TestStripEpisodePrefix(t *testing.T) {
	const book = "Гэри Стивенсон. «Бешеные деньги. Исповедь валютного трейдера»."
	tests := []struct {
		name, title, prefix, want string
	}{
		{"exact prefix + separator", "Гэри Стивенсон. «Бешеные деньги. Исповедь валютного трейдера». Часть 7", book, "Часть 7"},
		{"already short", "Часть 7", book, "Часть 7"},
		{"no prefix configured", "Гэри Стивенсон. Часть 1", "", "Гэри Стивенсон. Часть 1"},
		{"prefix not present", "Другая книга. Глава 2", book, "Другая книга. Глава 2"},
		{"case-insensitive", "гэри стивенсон. «бешеные деньги. исповедь валютного трейдера». Часть 9", book, "Часть 9"},
		{"strip would empty → keep original", book, book, book},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, stripEpisodePrefix(tt.title, tt.prefix))
		})
	}
}

func TestBuildFeedXMLEpisodeStrip(t *testing.T) {
	eps := []Episode{
		{File: "01 - Автор. «Кн». Часть 1.mp3", Title: "Автор. «Кн». Часть 1", Order: 1, PublicURL: "https://pub/01.mp3"},
	}
	cfg := FeedConfig{Title: "Кн", Type: "serial", EpisodeStrip: "Автор. «Кн»."}
	data, err := BuildFeedXML(cfg, eps)
	require.NoError(t, err)
	assert.Contains(t, string(data), "<title>Часть 1</title>")
	assert.NotContains(t, string(data), "<title>Автор")
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
func (fakeDuration) URL(string) int  { return 90 }

func TestBuildFeedXMLSerialSeasonOrder(t *testing.T) {
	// two seasons, interleaved on disk — feed must order by (season, track)
	eps := []Episode{
		{File: "S02/01 - A.mp3", Title: "S2E1", Season: 2, Order: 1, R2Key: "k/s2e1", PublicURL: "https://pub/s2e1.mp3"},
		{File: "S01/02 - B.mp3", Title: "S1E2", Season: 1, Order: 2, R2Key: "k/s1e2", PublicURL: "https://pub/s1e2.mp3"},
		{File: "S01/01 - C.mp3", Title: "S1E1", Season: 1, Order: 1, R2Key: "k/s1e1", PublicURL: "https://pub/s1e1.mp3"},
	}
	data, err := BuildFeedXML(FeedConfig{Title: "Show", Type: "serial"}, eps)
	require.NoError(t, err)
	xml := string(data)

	i11 := strings.Index(xml, "<title>S1E1</title>")
	i12 := strings.Index(xml, "<title>S1E2</title>")
	i21 := strings.Index(xml, "<title>S2E1</title>")
	require.True(t, i11 > 0 && i12 > 0 && i21 > 0)
	assert.True(t, i11 < i12 && i12 < i21, "order must be season then track")
	assert.Contains(t, xml, "<itunes:season>1</itunes:season>")
	assert.Contains(t, xml, "<itunes:season>2</itunes:season>")
}

func TestLoadWorkConfig(t *testing.T) {
	// missing work.yaml → empty config, finite by default
	assert.True(t, LoadWorkConfig(t.TempDir()).Finite())

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "work.yaml"),
		[]byte("title: Моя книга\ntype: streaming"), 0o600))
	wc := LoadWorkConfig(dir)
	assert.Equal(t, "Моя книга", wc.Title)
	assert.False(t, wc.Finite(), "type: streaming is not finite")
}

func TestParseSeasonNum(t *testing.T) {
	assert.Equal(t, 1, parseSeasonNum("Сезон 01"))
	assert.Equal(t, 3, parseSeasonNum("Season 3"))
	assert.Equal(t, 2, parseSeasonNum("S02"))
	assert.Equal(t, 1, parseSeasonNum("bonus"), "no digits → fallback 1")
}

package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsApplePodcastURL(t *testing.T) {
	assert.True(t, IsApplePodcastURL("https://podcasts.apple.com/us/podcast/some-show/id1200361736?i=1000700000001"))
	assert.True(t, IsApplePodcastURL("https://podcasts.apple.com/ru/podcast/id123456"))
	assert.False(t, IsApplePodcastURL("https://www.youtube.com/watch?v=abc"))
	assert.False(t, IsApplePodcastURL("https://music.apple.com/us/album/id555"))
}

func TestParseAppleURL(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		podcastID string
		episodeID string
		wantErr   bool
	}{
		{"episode link", "https://podcasts.apple.com/us/podcast/the-daily/id1200361736?i=1000712345678", "1200361736", "1000712345678", false},
		{"show link no episode", "https://podcasts.apple.com/us/podcast/the-daily/id1200361736", "1200361736", "", false},
		{"extra params", "https://podcasts.apple.com/ru/podcast/x/id99?l=en&i=555", "99", "555", false},
		{"not apple", "https://example.com/id123", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podcastID, episodeID, err := parseAppleURL(tt.url)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.podcastID, podcastID)
			assert.Equal(t, tt.episodeID, episodeID)
		})
	}
}

func TestAppleEpisodeIDFromURL(t *testing.T) {
	assert.Equal(t, "ap_555", appleEpisodeIDFromURL("https://podcasts.apple.com/us/podcast/x/id99?i=555"))
	assert.Equal(t, "", appleEpisodeIDFromURL("https://podcasts.apple.com/us/podcast/x/id99"), "show link has no episode id")
	assert.Equal(t, "", appleEpisodeIDFromURL("https://example.com"))
}

func TestAppleResolverResolve(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/lookup", r.URL.Path)
		assert.Equal(t, "99", r.URL.Query().Get("id"))
		assert.Equal(t, "podcastEpisode", r.URL.Query().Get("entity"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"wrapperType":"collection","collectionName":"My Show"},
			{"wrapperType":"track","kind":"podcast-episode","trackId":554,"trackName":"Other Episode",
			 "collectionName":"My Show","episodeUrl":"https://cdn.example.com/554.mp3","releaseDate":"2026-06-01T10:00:00Z","trackTimeMillis":600000},
			{"wrapperType":"track","kind":"podcast-episode","trackId":555,"trackName":"Правильный эпизод",
			 "collectionName":"My Show","episodeUrl":"https://cdn.example.com/555.mp3","releaseDate":"2026-07-01T10:00:00Z",
			 "trackTimeMillis":5640000,"artworkUrl600":"https://img.example.com/a.jpg","shortDescription":"Про всё"}
		]}`))
	}))
	defer ts.Close()

	r := NewAppleResolver()
	r.BaseURL = ts.URL

	ep, err := r.Resolve(context.Background(), "https://podcasts.apple.com/us/podcast/x/id99?i=555")
	require.NoError(t, err)
	assert.Equal(t, "Правильный эпизод", ep.Title)
	assert.Equal(t, "My Show", ep.Show)
	assert.Equal(t, "https://cdn.example.com/555.mp3", ep.AudioURL)
	assert.Equal(t, "2026-07-01", ep.Date)
	assert.Equal(t, 94, ep.DurationMin)
	assert.Equal(t, "https://img.example.com/a.jpg", ep.Artwork)
	assert.Equal(t, "ap_555", ep.SourceID())

	// missing episode
	_, err = r.Resolve(context.Background(), "https://podcasts.apple.com/us/podcast/x/id99?i=777")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "не найден")

	// show link without episode id
	_, err = r.Resolve(context.Background(), "https://podcasts.apple.com/us/podcast/x/id99")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "конкретный эпизод")
}

func TestAppleDescriptionBothShapes(t *testing.T) {
	var item lookupItem
	require.NoError(t, json.Unmarshal([]byte(`{"trackId":1,"description":"plain string form"}`), &item))
	require.NotNil(t, item.DescriptionBlock)
	assert.Equal(t, "plain string form", item.DescriptionBlock.Standard)

	require.NoError(t, json.Unmarshal([]byte(`{"trackId":2,"description":{"standard":"nested form"}}`), &item))
	assert.Equal(t, "nested form", item.DescriptionBlock.Standard)

	// garbage shape must not fail the decode
	require.NoError(t, json.Unmarshal([]byte(`{"trackId":3,"description":42}`), &item))
}

func TestAppleResolverResolveShow(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"wrapperType":"collection","collectionName":"Boomtown"},
			{"wrapperType":"track","kind":"podcast-episode","trackId":2,"trackName":"Episode 2","description":"string desc",
			 "collectionName":"Boomtown","episodeUrl":"https://cdn.example.com/2.mp3","releaseDate":"2026-02-01T10:00:00Z","trackTimeMillis":60000},
			{"wrapperType":"track","kind":"podcast-episode","trackId":3,"trackName":"No Audio","collectionName":"Boomtown","releaseDate":"2026-03-01T10:00:00Z"},
			{"wrapperType":"track","kind":"podcast-episode","trackId":1,"trackName":"Episode 1",
			 "collectionName":"Boomtown","episodeUrl":"https://cdn.example.com/1.mp3","releaseDate":"2026-01-01T10:00:00Z","trackTimeMillis":60000}
		]}`))
	}))
	defer ts.Close()

	r := NewAppleResolver()
	r.BaseURL = ts.URL

	show, eps, err := r.ResolveShow(context.Background(), "https://podcasts.apple.com/us/podcast/boomtown/id99")
	require.NoError(t, err)
	assert.Equal(t, "Boomtown", show)
	require.Len(t, eps, 2, "episode without audio skipped")
	assert.Equal(t, "Episode 1", eps[0].Title, "oldest first")
	assert.Equal(t, "Episode 2", eps[1].Title)
	assert.Equal(t, "https://podcasts.apple.com/podcast/id99?i=1", eps[0].EpisodeLink())
}

func TestFetchEpisodeExtrasAndOfficialTranscript(t *testing.T) {
	var baseURL string // set after the server starts, handlers close over it
	mux := http.NewServeMux()
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0"?>
<rss xmlns:podcast="https://podcastindex.org/namespace/1.0">
<channel>
  <item>
    <guid>guid-other</guid>
    <title>Other</title>
    <enclosure url="https://cdn.example.com/other.mp3"/>
  </item>
  <item>
    <guid>guid-555</guid>
    <title>Правильный эпизод</title>
    <description>Полные show notes, длиннее каталожного огрызка</description>
    <enclosure url="https://cdn.example.com/555.mp3"/>
    <podcast:transcript url="%s/t.html" type="text/html"/>
    <podcast:transcript url="%s/t.srt" type="application/srt"/>
    <podcast:chapters url="%s/ch.json" type="application/json+chapters"/>
  </item>
</channel>
</rss>`, baseURL, baseURL, baseURL)
	})
	mux.HandleFunc("/t.srt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("1\n00:00:01,000 --> 00:00:03,000\nОфициальный текст\n"))
	})
	mux.HandleFunc("/ch.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"chapters":[{"startTime":0,"title":"Intro"},{"startTime":754,"title":"Основная часть"}]}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	baseURL = ts.URL

	r := NewAppleResolver()
	r.BaseURL = ts.URL

	ep := &ApplePodcastEpisode{
		PodcastID: "99", EpisodeID: "555", GUID: "guid-555",
		Title: "Правильный эпизод", AudioURL: "https://cdn.example.com/555.mp3",
		FeedURL: ts.URL + "/feed.xml",
	}

	extras, err := r.FetchEpisodeExtras(context.Background(), ep)
	require.NoError(t, err)
	assert.Contains(t, extras.Description, "Полные show notes")
	assert.Equal(t, ts.URL+"/t.srt", extras.TranscriptURL, "timed format preferred over html")
	assert.Equal(t, "application/srt", extras.TranscriptType)

	tr, plain := r.OfficialTranscript(context.Background(), ep)
	require.NotNil(t, tr)
	assert.Empty(t, plain)
	require.Len(t, tr.Segments, 1)
	assert.Equal(t, "Официальный текст", tr.Segments[0].Text)
	assert.Equal(t, 1.0, tr.Segments[0].Start)

	chapters := r.FetchChapters(context.Background(), ts.URL+"/ch.json")
	assert.Equal(t, "00:00 Intro\n12:34 Основная часть", chapters)

	// missing episode in feed
	epMissing := &ApplePodcastEpisode{GUID: "nope", Title: "nope", FeedURL: ts.URL + "/feed.xml"}
	_, err = r.FetchEpisodeExtras(context.Background(), epMissing)
	require.Error(t, err)

	// no feed url at all
	_, err = r.FetchEpisodeExtras(context.Background(), &ApplePodcastEpisode{})
	require.Error(t, err)
}

func TestPickTranscript(t *testing.T) {
	item := &podcastRSSItem{}
	url, _ := pickTranscript(item)
	assert.Equal(t, "", url, "no transcripts")

	item.Transcripts = []struct {
		URL  string `xml:"url,attr"`
		Type string `xml:"type,attr"`
	}{
		{URL: "a.html", Type: "text/html"},
		{URL: "a.txt", Type: "text/plain"},
		{URL: "a.vtt", Type: "text/vtt"},
	}
	url, typ := pickTranscript(item)
	assert.Equal(t, "a.vtt", url, "timed format wins")
	assert.Equal(t, "text/vtt", typ)
}

func TestNoteSourceIDPodcast(t *testing.T) {
	id, source := noteSourceID("https://podcasts.apple.com/us/podcast/x/id99?i=555")
	assert.Equal(t, "ap_555", id)
	assert.Equal(t, "podcast", source)
}

package proc

import (
	"context"
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

func TestAppleResolverResolveShow(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"wrapperType":"collection","collectionName":"Boomtown"},
			{"wrapperType":"track","kind":"podcast-episode","trackId":2,"trackName":"Episode 2",
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

func TestNoteSourceIDPodcast(t *testing.T) {
	id, source := noteSourceID("https://podcasts.apple.com/us/podcast/x/id99?i=555")
	assert.Equal(t, "ap_555", id)
	assert.Equal(t, "podcast", source)
}

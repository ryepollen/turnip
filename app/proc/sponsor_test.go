package proc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSponsorChaptersFormatsSegments(t *testing.T) {
	var gotID, gotCats string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = r.URL.Query().Get("videoID")
		gotCats = r.URL.Query().Get("categories")
		_, _ = w.Write([]byte(`[
			{"category":"sponsor","actionType":"skip","segment":[185,200]},
			{"category":"selfpromo","actionType":"skip","segment":[10,20]},
			{"category":"sponsor","actionType":"mute","segment":[50,60]},
			{"category":"sponsor","actionType":"skip","segment":[12.9,18]},
			{"category":"sponsor","actionType":"skip","segment":[3661,3700]}
		]`))
	}))
	defer ts.Close()

	c := &SponsorClient{BaseURL: ts.URL, HTTP: ts.Client()}
	lines, err := c.Chapters(context.Background(), "dQw4w9WgXcQ")
	require.NoError(t, err)
	assert.Equal(t, "dQw4w9WgXcQ", gotID)
	assert.Equal(t, `["sponsor"]`, gotCats)
	assert.Equal(t, "00:12 реклама\n03:05 реклама\n1:01:01 реклама", lines)

	got := applySponsorChapters("show notes", lines, nil)
	assert.Equal(t, "show notes\n\nГлавы:\n00:12 реклама\n03:05 реклама\n1:01:01 реклама", got)
}

func TestSponsorChaptersNoSegments(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	c := &SponsorClient{BaseURL: ts.URL, HTTP: ts.Client()}
	lines, err := c.Chapters(context.Background(), "dQw4w9WgXcQ")
	require.NoError(t, err)
	assert.Empty(t, lines)
	assert.Equal(t, "show notes", applySponsorChapters("show notes", lines, err))

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer empty.Close()
	c.BaseURL = empty.URL
	lines, err = c.Chapters(context.Background(), "dQw4w9WgXcQ")
	require.NoError(t, err)
	assert.Empty(t, lines)

	lines, err = c.Chapters(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, lines)
	assert.Equal(t, "show notes", applySponsorChapters("show notes", lines, err))
}

func TestSponsorChaptersNetworkError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	c := &SponsorClient{BaseURL: ts.URL, HTTP: &http.Client{Timeout: time.Second}}
	lines, err := c.Chapters(context.Background(), "dQw4w9WgXcQ")
	require.Error(t, err)
	assert.Empty(t, lines)
	assert.Equal(t, "show notes", applySponsorChapters("show notes", "00:01 реклама", err))

	ts.Close()
	_, err = c.Chapters(context.Background(), "dQw4w9WgXcQ")
	require.Error(t, err)
}

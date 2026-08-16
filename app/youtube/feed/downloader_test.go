package feed

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDownloader_Get(t *testing.T) {
	lw := bytes.NewBuffer(nil)
	loc := os.TempDir()
	fh, err := os.CreateTemp(loc, "downloader_test*.mp3")
	require.NoError(t, err)
	defer os.Remove(fh.Name())

	fname := filepath.Base(fh.Name())

	d := NewDownloader("echo {{.ID}} blah {{.FileName}}.mp3 12345", lw, lw, loc, "")
	res, err := d.Get(context.Background(), "id1", strings.TrimSuffix(fname, path.Ext(fname)))
	require.NoError(t, err)
	assert.Equal(t, fh.Name(), res)
	l := lw.String()
	assert.Equal(t, fmt.Sprintf("id1 blah %s 12345\n", fname), l)
	t.Log(l)
}

func TestInjectSponsorBlock(t *testing.T) {
	const base = `yt-dlp --extract-audio -f m4a/bestaudio "https://youtu.be/x" -o out.mp3`
	tests := []struct {
		name string
		cats string
		want string
	}{
		{"empty disables", "", base},
		{"whitespace disables", "  ", base},
		{"single category", "sponsor",
			`yt-dlp --sponsorblock-remove sponsor --extract-audio -f m4a/bestaudio "https://youtu.be/x" -o out.mp3`},
		{"curated set", "sponsor,selfpromo,interaction",
			`yt-dlp --sponsorblock-remove sponsor,selfpromo,interaction --extract-audio -f m4a/bestaudio "https://youtu.be/x" -o out.mp3`},
		{"trimmed", "  sponsor  ",
			`yt-dlp --sponsorblock-remove sponsor --extract-audio -f m4a/bestaudio "https://youtu.be/x" -o out.mp3`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, injectSponsorBlock(base, tt.cats))
		})
	}
}

func TestSetSponsorBlock(t *testing.T) {
	d := NewDownloader("yt-dlp {{.ID}}", nil, nil, "", "")
	assert.Empty(t, d.sponsorBlockCat)
	d.SetSponsorBlock("  sponsor,selfpromo  ")
	assert.Equal(t, "sponsor,selfpromo", d.sponsorBlockCat, "categories are trimmed on set")
}

func TestDownloader_GetSkip(t *testing.T) {
	lw := bytes.NewBuffer(nil)
	loc := os.TempDir()
	fh, err := os.CreateTemp(loc, "downloader_test")
	require.NoError(t, err)
	assert.NoError(t, os.Remove(fh.Name()))

	fname := filepath.Base(fh.Name())
	d := NewDownloader("echo {{.ID}} blah {{.FileName}} 12345", lw, lw, loc, "")
	res, err := d.Get(context.Background(), "id1", fname)
	require.EqualError(t, err, "skip")
	assert.Equal(t, fh.Name()+".mp3", res)
}

func TestDownloader_GetFailed(t *testing.T) {
	lw := bytes.NewBuffer(nil)
	loc := os.TempDir()
	fh, err := os.CreateTemp(loc, "downloader_test*.mp3")
	require.NoError(t, err)
	assert.NoError(t, os.Remove(fh.Name()))

	fname := filepath.Base(fh.Name())

	d := NewDownloader("echo {{.ID}} blah {{.FileName}}.mp3 12345", lw, lw, loc, "")
	res, err := d.Get(context.Background(), "id1", strings.TrimSuffix(fname, path.Ext(fname)))
	require.EqualError(t, err, "skip")
	assert.Equal(t, fh.Name(), res)
}

func TestIsCookieError(t *testing.T) {
	tests := []struct {
		name string
		err  string
		want bool
	}{
		{"cookies no longer valid", "ERROR: cookies are no longer valid", true},
		{"cookies expired", "cookies have expired or been revoked", true},
		{"please sign in", "ERROR: Please sign in to access this video", true},
		{"sign in age", "Sign in to confirm your age", true},
		{"sign in confirm", "Sign in to confirm you're not a bot", true},
		{"unrelated error", "network timeout", false},
		{"empty string", "", false},
		{"partial match", "cookie", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsCookieError(tt.err))
		})
	}
}

func TestParseFlatPlaylistIDs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"single", "dQw4w9WgXcQ\n", []string{"dQw4w9WgXcQ"}},
		{"order preserved, deduped", "aaaaaaaaaaa\nbbbbbbbbbbb\naaaaaaaaaaa\n",
			[]string{"aaaaaaaaaaa", "bbbbbbbbbbb"}},
		{"stray non-11-char lines skipped", "short\naaaaaaaaaaa\nway_too_long_id_here\n  bbbbbbbbbbb  \n",
			[]string{"aaaaaaaaaaa", "bbbbbbbbbbb"}},
		{"surrounding blank lines", "\n\naaaaaaaaaaa\n\n", []string{"aaaaaaaaaaa"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseFlatPlaylistIDs(tt.raw))
		})
	}
}

func TestParseFlatPlaylistItems(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []PlaylistItem
	}{
		{"empty", "", nil},
		{"id and title", "dQw4w9WgXcQ\tNever Gonna Give You Up\n",
			[]PlaylistItem{{ID: "dQw4w9WgXcQ", Title: "Never Gonna Give You Up"}}},
		{"title with spaces and tabs kept as one", "aaaaaaaaaaa\tHello   World\n",
			[]PlaylistItem{{ID: "aaaaaaaaaaa", Title: "Hello   World"}}},
		{"missing title falls back to id", "aaaaaaaaaaa\t\nbbbbbbbbbbb\tNA\n",
			[]PlaylistItem{{ID: "aaaaaaaaaaa", Title: "aaaaaaaaaaa"}, {ID: "bbbbbbbbbbb", Title: "bbbbbbbbbbb"}}},
		{"no tab at all (id only)", "aaaaaaaaaaa\n",
			[]PlaylistItem{{ID: "aaaaaaaaaaa", Title: "aaaaaaaaaaa"}}},
		{"dedup keeps first title", "aaaaaaaaaaa\tFirst\naaaaaaaaaaa\tSecond\n",
			[]PlaylistItem{{ID: "aaaaaaaaaaa", Title: "First"}}},
		{"bad id lines skipped", "short\tx\naaaaaaaaaaa\tOK\n",
			[]PlaylistItem{{ID: "aaaaaaaaaaa", Title: "OK"}}},
		{"CRLF trimmed", "aaaaaaaaaaa\tTitle\r\n",
			[]PlaylistItem{{ID: "aaaaaaaaaaa", Title: "Title"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseFlatPlaylistItems(tt.raw))
		})
	}
}

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

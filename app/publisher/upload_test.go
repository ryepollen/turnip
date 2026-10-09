package publisher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckUploadName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"plain part", "01 - Часть первая.mp3", false},
		{"m4b part", "02 - Kapitel.m4b", false},
		{"empty", "", true},
		{"too long", strings.Repeat("x", 201) + ".mp3", true},
		{"path separator", "a/b.mp3", true},
		{"backslash", `a\b.mp3`, true},
		{"hidden", ".sneaky.mp3", true},
		{"dotdot", "..", true},
		{"not audio", "notes.txt", true},
		{"no extension", "part-one", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkUploadName(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestCheckUploadTarget(t *testing.T) {
	svc, _ := newFakeR2Service(t)
	require.NoError(t, os.MkdirAll(svc.categoryDir("books"), 0o750))

	assert.NoError(t, svc.checkUploadTarget("books", "Новая книга"))
	assert.Error(t, svc.checkUploadTarget("nope", "W"), "category dir must already exist — web must not mint feed slugs")
	assert.Error(t, svc.checkUploadTarget("books", ""), "empty work")
	assert.Error(t, svc.checkUploadTarget("books", "a/b"), "slash in work")
	assert.Error(t, svc.checkUploadTarget("books", ".hidden"), "hidden work")
	assert.Error(t, svc.checkUploadTarget("books", " padded "), "surrounding spaces")
	assert.Error(t, svc.checkUploadTarget("../etc", "W"), "category traversal")
}

func TestPresignUploadPart(t *testing.T) {
	svc, _ := newFakeR2Service(t)
	require.NoError(t, os.MkdirAll(svc.categoryDir("books"), 0o750))

	url, err := svc.PresignUploadPart(t.Context(), "books", "Веб-Книга", "01 - Один.mp3")
	require.NoError(t, err)
	assert.Contains(t, url, "/turnip/", "bucket in path")
	assert.Contains(t, url, "X-Amz-Signature=", "a signed URL, usable without our creds")
	assert.Contains(t, url, "books", "key carries the category")

	_, err = svc.PresignUploadPart(t.Context(), "books", "W", "../../etc/passwd")
	assert.Error(t, err)
	_, err = svc.PresignUploadPart(t.Context(), "courses", "W", "01.mp3")
	assert.Error(t, err, "unknown category dir")
}

// TestWebIngestEndToEnd is variant B in one piece: browser PUTs modeled by
// seeding the fake R2, commit verifies + writes the stub skeleton, and a
// Resident Activate rebuilds state purely from R2 — local bytes never read.
func TestWebIngestEndToEnd(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	svc.Resident = true
	spy := &spyDuration{}
	svc.Duration = spy
	require.NoError(t, os.MkdirAll(svc.categoryDir("books"), 0o750))

	names := []string{"01 - Один.mp3", "02 - Два.mp3"}

	// half-finished upload: commit must name what's missing and register nothing
	fake.seed("a/s3cr3t/books/Веб-Книга/01 - Один.mp3", 111)
	missing, err := svc.VerifyUploaded(t.Context(), "books", "Веб-Книга", names)
	require.NoError(t, err)
	assert.Equal(t, []string{"02 - Два.mp3"}, missing)

	// the browser finishes; verify clears and the skeleton lands
	fake.seed("a/s3cr3t/books/Веб-Книга/02 - Два.mp3", 222)
	missing, err = svc.VerifyUploaded(t.Context(), "books", "Веб-Книга", names)
	require.NoError(t, err)
	assert.Empty(t, missing)
	require.NoError(t, svc.RegisterWork("books", "Веб-Книга", names))

	for _, n := range names {
		fi, serr := os.Stat(filepath.Join(svc.categoryDir("books"), "Веб-Книга", n))
		require.NoError(t, serr)
		assert.Zero(t, fi.Size(), "skeleton stubs carry names only, never bytes")
	}

	// the dormant work is in the catalog and activates without any upload
	works, err := svc.Catalog()
	require.NoError(t, err)
	require.Len(t, works, 1)
	assert.Equal(t, "Веб-Книга", works[0].Slug)
	assert.False(t, works[0].Active)

	require.NoError(t, svc.Activate(t.Context(), "books", "Веб-Книга", nil))
	assert.Zero(t, fake.puts, "Resident Activate = Finalize: the browser already uploaded")
	assert.Zero(t, spy.fileCalls, "durations stream from R2, local stubs are never read")
	assert.Positive(t, spy.urlCalls)

	active, err := svc.ActiveWork("books")
	require.NoError(t, err)
	assert.Equal(t, "Веб-Книга", active)
	eps, err := svc.EpisodeList("books")
	require.NoError(t, err)
	require.Len(t, eps, 2)
	assert.EqualValues(t, 111, eps[0].SizeBytes, "size from the R2 HEAD")
}

func TestResidentActivateIncompleteUpload(t *testing.T) {
	svc, _ := newFakeR2Service(t)
	svc.Resident = true
	require.NoError(t, os.MkdirAll(svc.categoryDir("books"), 0o750))
	require.NoError(t, svc.RegisterWork("books", "Пустышка", []string{"01 - One.mp3"}))

	err := svc.Activate(t.Context(), "books", "Пустышка", nil)
	require.Error(t, err, "nothing was uploaded for the stubs")
	assert.Contains(t, err.Error(), "upload incomplete")
}

func TestRegisterWorkNeverTruncates(t *testing.T) {
	svc, _ := newFakeR2Service(t)
	writeWork(t, svc, "books", "Настоящая", "01 - One.mp3") // a real file with bytes

	require.NoError(t, svc.RegisterWork("books", "Настоящая", []string{"01 - One.mp3", "02 - Two.mp3"}))

	b, err := os.ReadFile(filepath.Join(svc.categoryDir("books"), "Настоящая", "01 - One.mp3"))
	require.NoError(t, err)
	assert.NotEmpty(t, b, "an existing original must survive a re-commit untouched")
	fi, err := os.Stat(filepath.Join(svc.categoryDir("books"), "Настоящая", "02 - Two.mp3"))
	require.NoError(t, err)
	assert.Zero(t, fi.Size(), "only the genuinely new name gets a stub")
}

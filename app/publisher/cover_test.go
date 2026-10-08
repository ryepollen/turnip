package publisher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Work covers: tune_get drops cover.jpg into every work folder; the Mac agent
// uploads it beside the parts, and Finalize turns it into per-episode artwork.
// The channel cover (feed.yaml) never changes on activation.

func TestFinalizeSetsEpisodeCoverFromR2(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "podcasts", "Show", "01 - One.mp3", "02 - Two.mp3")
	fake.seed("a/s3cr3t/podcasts/Show/01 - One.mp3", 10)
	fake.seed("a/s3cr3t/podcasts/Show/02 - Two.mp3", 20)
	fake.seed("a/s3cr3t/podcasts/Show/cover.jpg", 5)

	require.NoError(t, svc.Finalize(t.Context(), "podcasts", "Show"))

	st, err := svc.loadState("podcasts")
	require.NoError(t, err)
	assert.Equal(t, "a/s3cr3t/podcasts/Show/cover.jpg", st.CoverKey)
	require.Len(t, st.Episodes, 2)
	want := svc.R2.PublicURL("a/s3cr3t/podcasts/Show/cover.jpg")
	for _, e := range st.Episodes {
		assert.Equal(t, want, e.Image)
	}

	xml, err := os.ReadFile(filepath.Join(svc.AudioDir, "feeds", "podcasts.xml"))
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(xml), `<itunes:image href="`+want+`"`),
		"every episode carries the work cover")
}

func TestFinalizeWithoutCoverLeavesEpisodesPlain(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "books", "Bare", "01 - One.mp3")
	fake.seed("a/s3cr3t/books/Bare/01 - One.mp3", 10)

	require.NoError(t, svc.Finalize(t.Context(), "books", "Bare"))

	st, err := svc.loadState("books")
	require.NoError(t, err)
	assert.Empty(t, st.CoverKey)
	assert.Empty(t, st.Episodes[0].Image)
	xml, err := os.ReadFile(filepath.Join(svc.AudioDir, "feeds", "books.xml"))
	require.NoError(t, err)
	assert.NotContains(t, string(xml), "itunes:image", "no cover → no item artwork (and no channel cover in this test)")
}

func TestActivatingAnotherWorkEvictsOldCover(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "books", "A", "01 - A.mp3")
	writeWork(t, svc, "books", "B", "01 - B.mp3")
	fake.seed("a/s3cr3t/books/A/01 - A.mp3", 10)
	fake.seed("a/s3cr3t/books/A/cover.jpg", 5)
	fake.seed("a/s3cr3t/books/B/01 - B.mp3", 10)
	fake.seed("a/s3cr3t/books/B/cover.jpg", 5)

	require.NoError(t, svc.Finalize(t.Context(), "books", "A"))
	require.NoError(t, svc.Finalize(t.Context(), "books", "B"))

	assert.False(t, fake.has("a/s3cr3t/books/A/cover.jpg"), "old work's cover freed with its parts")
	assert.True(t, fake.has("a/s3cr3t/books/B/cover.jpg"))
	st, _ := svc.loadState("books")
	assert.Equal(t, "a/s3cr3t/books/B/cover.jpg", st.CoverKey)
}

func TestRefinalizeSameWorkKeepsCover(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "books", "A", "01 - A.mp3")
	fake.seed("a/s3cr3t/books/A/01 - A.mp3", 10)
	fake.seed("a/s3cr3t/books/A/cover.jpg", 5)

	require.NoError(t, svc.Finalize(t.Context(), "books", "A"))
	require.NoError(t, svc.Finalize(t.Context(), "books", "A"))
	assert.True(t, fake.has("a/s3cr3t/books/A/cover.jpg"), "idempotent re-run never evicts its own cover")
}

func TestDeactivateDeletesCover(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "books", "A", "01 - A.mp3")
	fake.seed("a/s3cr3t/books/A/01 - A.mp3", 10)
	fake.seed("a/s3cr3t/books/A/cover.jpg", 5)
	require.NoError(t, svc.Finalize(t.Context(), "books", "A"))

	require.NoError(t, svc.Deactivate(t.Context(), "books"))
	assert.False(t, fake.has("a/s3cr3t/books/A/cover.jpg"))
	st, _ := svc.loadState("books")
	assert.Empty(t, st.CoverKey)
}

func TestActivateUploadsLocalCover(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "books", "Local", "01 - One.mp3")
	cover := filepath.Join(svc.categoryDir("books"), "Local", WorkCoverFile)
	require.NoError(t, os.WriteFile(cover, []byte("jpeg-bytes"), 0o600))

	require.NoError(t, svc.Activate(t.Context(), "books", "Local", nil))

	assert.True(t, fake.has("a/s3cr3t/books/Local/cover.jpg"), "in-process path uploads the folder's cover")
	st, _ := svc.loadState("books")
	assert.Equal(t, svc.R2.PublicURL("a/s3cr3t/books/Local/cover.jpg"), st.Episodes[0].Image)
}

func TestActivateIgnoresZeroByteCoverStub(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "books", "Stub", "01 - One.mp3")
	cover := filepath.Join(svc.categoryDir("books"), "Stub", WorkCoverFile)
	require.NoError(t, os.WriteFile(cover, nil, 0o600)) // manifest-style 0-byte stub

	require.NoError(t, svc.Activate(t.Context(), "books", "Stub", nil))
	assert.False(t, fake.has("a/s3cr3t/books/Stub/cover.jpg"), "a stub is never uploaded as a cover")
	st, _ := svc.loadState("books")
	assert.Empty(t, st.Episodes[0].Image)
}

func TestBuildFeedXMLItemImage(t *testing.T) {
	out, err := BuildFeedXML(FeedConfig{Title: "T", Type: "serial", Cover: "https://x/channel.png"}, []Episode{
		{File: "01 - A.mp3", Title: "A", Order: 1, R2Key: "k1", PublicURL: "https://x/1.mp3", Image: "https://x/cover.jpg"},
		{File: "02 - B.mp3", Title: "B", Order: 2, R2Key: "k2", PublicURL: "https://x/2.mp3"},
	})
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, `<itunes:image href="https://x/channel.png"></itunes:image>`, "channel cover untouched")
	assert.Equal(t, 1, strings.Count(s, `<itunes:image href="https://x/cover.jpg"></itunes:image>`))
	assert.Equal(t, 2, strings.Count(s, "<itunes:image "), "channel + one item; the coverless item has none")
}

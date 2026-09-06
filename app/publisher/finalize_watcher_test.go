package publisher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seed simulates the Mac agent having uploaded an object to R2: it records the
// key at the given size so the VM's HEAD-based Finalize sees it as present.
func (f *fakeR2) seed(key string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects["/turnip/"+key] = size
}

// recNotifier records finalize outcomes for assertions.
type recNotifier struct {
	done    []ActivationRequest
	feeds   []string
	eps     []int
	failed  []ActivationRequest
	reasons []string
}

func (r *recNotifier) ActivationDone(req ActivationRequest, feedURL string, episodes int) {
	r.done = append(r.done, req)
	r.feeds = append(r.feeds, feedURL)
	r.eps = append(r.eps, episodes)
}

func (r *recNotifier) ActivationFailed(req ActivationRequest, reason string) {
	r.failed = append(r.failed, req)
	r.reasons = append(r.reasons, reason)
}

func TestR2Size(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	fake.seed("a/s3cr3t/books/W/01.mp3", 4242)

	sz, err := svc.R2.Size(t.Context(), "a/s3cr3t/books/W/01.mp3")
	require.NoError(t, err)
	assert.EqualValues(t, 4242, sz)

	// an absent key reads as size 0 with no error (not-uploaded, not a failure)
	sz, err = svc.R2.Size(t.Context(), "a/s3cr3t/books/W/missing.mp3")
	require.NoError(t, err)
	assert.Zero(t, sz)
}

func TestFinalizeBuildsStateFromR2(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "books", "TheBook", "01 - Один.mp3", "02 - Два.mp3")
	// the Mac agent already uploaded both parts; the VM only reads their sizes
	fake.seed("a/s3cr3t/books/TheBook/01 - Один.mp3", 111)
	fake.seed("a/s3cr3t/books/TheBook/02 - Два.mp3", 222)

	require.NoError(t, svc.Finalize(t.Context(), "books", "TheBook"))
	assert.Zero(t, fake.puts, "Finalize never uploads — the Mac agent already did")

	active, err := svc.ActiveWork("books")
	require.NoError(t, err)
	assert.Equal(t, "TheBook", active)

	eps, err := svc.EpisodeList("books")
	require.NoError(t, err)
	require.Len(t, eps, 2)
	assert.Equal(t, "Один", eps[0].Title)
	assert.EqualValues(t, 111, eps[0].SizeBytes, "size comes from the R2 HEAD, not local bytes")
	assert.EqualValues(t, 222, eps[1].SizeBytes)
	assert.Equal(t, "a/s3cr3t/books/TheBook/01 - Один.mp3", eps[0].R2Key)
	assert.Contains(t, eps[0].PublicURL, "TheBook")

	// the feed was regenerated on disk
	_, err = os.Stat(filepath.Join(svc.AudioDir, "feeds", "books.xml"))
	require.NoError(t, err)
}

func TestFinalizeMissingPartErrors(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "books", "Half", "01 - One.mp3", "02 - Two.mp3")
	fake.seed("a/s3cr3t/books/Half/01 - One.mp3", 10) // second part never uploaded

	err := svc.Finalize(t.Context(), "books", "Half")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upload incomplete")

	// state untouched — the old (empty) active work keeps serving
	active, _ := svc.ActiveWork("books")
	assert.Empty(t, active)
}

func TestFinalizeEvictsPreviousWork(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	writeWork(t, svc, "books", "First", "01 - One.mp3")
	writeWork(t, svc, "books", "Second", "01 - Alpha.mp3")
	fake.seed("a/s3cr3t/books/First/01 - One.mp3", 10)
	fake.seed("a/s3cr3t/books/Second/01 - Alpha.mp3", 20)

	require.NoError(t, svc.Finalize(t.Context(), "books", "First"))
	require.True(t, fake.has("a/s3cr3t/books/First/01 - One.mp3"))

	fake.deletes = 0
	require.NoError(t, svc.Finalize(t.Context(), "books", "Second"))
	assert.False(t, fake.has("a/s3cr3t/books/First/01 - One.mp3"), "previous work evicted from R2")
	assert.Equal(t, 1, fake.deletes)

	active, _ := svc.ActiveWork("books")
	assert.Equal(t, "Second", active)
}

func TestDrainDoneActivates(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	q := NewActivationQueue(svc.AudioDir)
	writeWork(t, svc, "books", "Book", "01 - One.mp3", "02 - Two.mp3")
	fake.seed("a/s3cr3t/books/Book/01 - One.mp3", 10)
	fake.seed("a/s3cr3t/books/Book/02 - Two.mp3", 20)

	// drive the request to done, as the Mac agent leaves it after uploading
	id, err := q.Enqueue(ActivationRequest{Op: OpActivate, Category: "books", Work: "Book", ChatID: 7, StatusMsgID: 9})
	require.NoError(t, err)
	_, err = q.Claim(id)
	require.NoError(t, err)
	require.NoError(t, q.Complete(id))

	n := &recNotifier{}
	svc.drainDone(t.Context(), q, n)

	active, _ := svc.ActiveWork("books")
	assert.Equal(t, "Book", active)

	doneLeft, _ := q.ListDone()
	assert.Empty(t, doneLeft, "finalized request archived out of the done worklist")

	require.Len(t, n.done, 1)
	assert.Equal(t, id, n.done[0].ID)
	assert.Equal(t, 2, n.eps[0])
	assert.Contains(t, n.feeds[0], "/holzweg/s3cr3t/books.xml")
	assert.Empty(t, n.failed)
}

func TestDrainDoneRejectsIncompleteUpload(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	q := NewActivationQueue(svc.AudioDir)
	writeWork(t, svc, "books", "Broken", "01 - One.mp3", "02 - Two.mp3")
	fake.seed("a/s3cr3t/books/Broken/01 - One.mp3", 10) // second part missing in R2

	id, err := q.Enqueue(ActivationRequest{Op: OpActivate, Category: "books", Work: "Broken"})
	require.NoError(t, err)
	_, err = q.Claim(id)
	require.NoError(t, err)
	require.NoError(t, q.Complete(id))

	n := &recNotifier{}
	svc.drainDone(t.Context(), q, n)

	// not applied, moved to failed with a reason, owner told
	active, _ := svc.ActiveWork("books")
	assert.Empty(t, active)
	doneLeft, _ := q.ListDone()
	assert.Empty(t, doneLeft)
	assert.Contains(t, q.FailReason(id), "upload incomplete")
	require.Len(t, n.failed, 1)
	assert.Equal(t, id, n.failed[0].ID)
	assert.Empty(t, n.done)
}

func TestDrainDoneDeactivates(t *testing.T) {
	svc, fake := newFakeR2Service(t)
	q := NewActivationQueue(svc.AudioDir)
	writeWork(t, svc, "books", "Live", "01 - One.mp3")
	fake.seed("a/s3cr3t/books/Live/01 - One.mp3", 10)
	require.NoError(t, svc.Finalize(t.Context(), "books", "Live"))

	id, err := q.Enqueue(ActivationRequest{Op: OpDeactivate, Category: "books"})
	require.NoError(t, err)
	_, err = q.Claim(id)
	require.NoError(t, err)
	require.NoError(t, q.Complete(id))

	n := &recNotifier{}
	svc.drainDone(t.Context(), q, n)

	active, _ := svc.ActiveWork("books")
	assert.Empty(t, active, "category deactivated")
	assert.False(t, fake.has("a/s3cr3t/books/Live/01 - One.mp3"), "R2 freed")
	require.Len(t, n.done, 1)
	assert.Equal(t, 0, n.eps[0], "empty feed")
}

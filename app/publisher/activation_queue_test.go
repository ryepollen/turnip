package publisher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivationQueueRoundTrip(t *testing.T) {
	q := NewActivationQueue(t.TempDir())

	id, err := q.Enqueue(ActivationRequest{
		Op: OpActivate, Category: "books", Work: "Гэри Стивенсон — Бешеные деньги",
		ChatID: 5504926420, StatusMsgID: 42,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// visible to the Mac agent as pending
	pend, err := q.ListPending()
	require.NoError(t, err)
	require.Len(t, pend, 1)
	assert.Equal(t, "Гэри Стивенсон — Бешеные деньги", pend[0].Work)
	assert.Equal(t, OpActivate, pend[0].Op)
	assert.EqualValues(t, 5504926420, pend[0].ChatID)

	// claim removes it from pending and returns the payload
	claimed, err := q.Claim(id)
	require.NoError(t, err)
	assert.Equal(t, "books", claimed.Category)
	pend, err = q.ListPending()
	require.NoError(t, err)
	assert.Empty(t, pend)

	// complete → visible to the VM watcher as done
	require.NoError(t, q.Complete(id))
	done, err := q.ListDone()
	require.NoError(t, err)
	require.Len(t, done, 1)
	assert.Equal(t, id, done[0].ID)

	// archive retires it from the done worklist
	require.NoError(t, q.Archive(id))
	done, err = q.ListDone()
	require.NoError(t, err)
	assert.Empty(t, done)
}

func TestActivationQueueClaimOnce(t *testing.T) {
	q := NewActivationQueue(t.TempDir())
	id, err := q.Enqueue(ActivationRequest{Op: OpActivate, Category: "podcasts", Work: "Витю видел"})
	require.NoError(t, err)

	_, err = q.Claim(id)
	require.NoError(t, err)

	// second claim of the same request must fail — the double-work guarantee
	_, err = q.Claim(id)
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err), "second claim should see a missing source, got %v", err)
}

func TestActivationQueueFailSidecar(t *testing.T) {
	q := NewActivationQueue(t.TempDir())
	id, err := q.Enqueue(ActivationRequest{Op: OpActivate, Category: "books", Work: "Missing"})
	require.NoError(t, err)
	_, err = q.Claim(id)
	require.NoError(t, err)

	require.NoError(t, q.Fail(id, "work folder not found"))

	// not delivered to the watcher, reason recoverable from the sidecar
	done, err := q.ListDone()
	require.NoError(t, err)
	assert.Empty(t, done)
	assert.Equal(t, "work folder not found", q.FailReason(id))
}

func TestActivationQueueOldestFirst(t *testing.T) {
	q := NewActivationQueue(t.TempDir())
	base := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	// enqueue out of order; ids embed UnixNano so listing must reorder by time
	_, err := q.Enqueue(ActivationRequest{Op: OpActivate, Category: "books", Work: "second", RequestedAt: base.Add(time.Minute)})
	require.NoError(t, err)
	_, err = q.Enqueue(ActivationRequest{Op: OpActivate, Category: "books", Work: "first", RequestedAt: base})
	require.NoError(t, err)

	pend, err := q.ListPending()
	require.NoError(t, err)
	require.Len(t, pend, 2)
	assert.Equal(t, "first", pend[0].Work)
	assert.Equal(t, "second", pend[1].Work)
}

func TestActivationQueueEnqueueIsAtomic(t *testing.T) {
	// no stray .tmp files should survive a successful enqueue
	dir := t.TempDir()
	q := NewActivationQueue(dir)
	_, err := q.Enqueue(ActivationRequest{Op: OpActivate, Category: "books", Work: "W"})
	require.NoError(t, err)

	var tmps []string
	_ = filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error {
		if filepath.Ext(p) == ".tmp" {
			tmps = append(tmps, p)
		}
		return nil
	})
	assert.Empty(t, tmps, "temp files must be renamed away")
}

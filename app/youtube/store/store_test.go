package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"

	"github.com/umputun/feed-master/app/youtube/feed"
)

func TestStore_SaveAndLoad(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "test.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	require.NoError(t, err)

	s := BoltDB{DB: db}

	entry := feed.Entry{
		ChannelID: "chan1",
		VideoID:   "vid1",
		Title:     "title1",
		Published: time.Date(2022, time.March, 21, 16, 45, 22, 0, time.UTC),
	}

	created, err := s.Save(entry)
	require.NoError(t, err)
	assert.True(t, created)

	created, err = s.Save(entry)
	require.NoError(t, err)
	assert.False(t, created)

	res, err := s.Load("chan1", 100)
	require.NoError(t, err)
	assert.Equal(t, 1, len(res))
	assert.Equal(t, "vid1", res[0].VideoID)

	entry2 := feed.Entry{
		ChannelID: "chan1",
		VideoID:   "vid2",
		Title:     "title2",
		Published: time.Date(2022, time.March, 21, 17, 45, 22, 0, time.UTC),
	}
	created, err = s.Save(entry2)
	require.NoError(t, err)
	assert.True(t, created)

	res, err = s.Load("chan1", 100)
	require.NoError(t, err)
	assert.Equal(t, 2, len(res))
	assert.Equal(t, "vid2", res[0].VideoID)

	res, err = s.Load("chan1", 1)
	require.NoError(t, err)
	assert.Equal(t, 1, len(res))
	assert.Equal(t, "vid2", res[0].VideoID)
}

func TestStore_Remove(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "test.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	require.NoError(t, err)

	s := BoltDB{DB: db}

	entry := feed.Entry{
		ChannelID: "chan1",
		VideoID:   "vid1",
		Title:     "title1",
		Published: time.Date(2022, time.March, 21, 16, 45, 22, 0, time.UTC),
	}

	created, err := s.Save(entry)
	require.NoError(t, err)
	assert.True(t, created)

	entry2 := feed.Entry{
		ChannelID: "chan1",
		VideoID:   "vid2",
		Title:     "title2",
		Published: time.Date(2022, time.March, 21, 17, 45, 22, 0, time.UTC),
	}
	created, err = s.Save(entry2)
	require.NoError(t, err)
	assert.True(t, created)

	res, err := s.Load("chan1", 100)
	require.NoError(t, err)
	assert.Equal(t, 2, len(res))
	assert.Equal(t, "vid2", res[0].VideoID)

	err = s.Remove(feed.Entry{ChannelID: "chan1", VideoID: "vid2"})
	require.NoError(t, err)
	res, err = s.Load("chan1", 10)
	require.NoError(t, err)
	assert.Equal(t, 1, len(res))
	assert.Equal(t, "vid1", res[0].VideoID)
}

func TestStore_Exist(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "test.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	require.NoError(t, err)

	s := BoltDB{DB: db}

	entry := feed.Entry{
		ChannelID: "chan1",
		VideoID:   "vid1",
		Title:     "title1",
		Published: time.Date(2022, time.March, 21, 16, 45, 22, 0, time.UTC),
	}

	created, err := s.Save(entry)
	require.NoError(t, err)
	assert.True(t, created)

	ok, err := s.Exist(entry)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = s.Exist(feed.Entry{ChannelID: "chan2", VideoID: "vid2"})
	require.NoError(t, err)
	assert.False(t, ok)

}

func TestBoldDB_RemoveOld(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "test.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	require.NoError(t, err)

	s := BoltDB{DB: db}
	{
		entry := feed.Entry{
			ChannelID: "chan1",
			VideoID:   "vid1",
			Title:     "title1",
			Published: time.Date(2022, time.March, 21, 16, 45, 22, 0, time.UTC),
			File:      "f1",
		}
		created, e := s.Save(entry)
		require.NoError(t, e)
		assert.True(t, created)
	}
	{
		entry := feed.Entry{
			ChannelID: "chan1",
			VideoID:   "vid2",
			Title:     "title2",
			Published: time.Date(2022, time.March, 21, 17, 45, 22, 0, time.UTC),
			File:      "f2",
		}
		created, e := s.Save(entry)
		require.NoError(t, e)
		assert.True(t, created)
	}
	{
		entry := feed.Entry{
			ChannelID: "chan1",
			VideoID:   "vid3",
			Title:     "title3",
			Published: time.Date(2022, time.March, 21, 18, 45, 22, 0, time.UTC),
			File:      "f3",
		}
		created, e := s.Save(entry)
		require.NoError(t, e)
		assert.True(t, created)
	}

	res, err := s.RemoveOld("chan1", 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"f2", "f1"}, res)
}

func TestBoltDB_SetProcessed(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "test.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	require.NoError(t, err)

	s := BoltDB{DB: db}

	{
		entry := feed.Entry{
			ChannelID: "chan1",
			VideoID:   "vid1",
			Title:     "title1",
			Published: time.Date(2022, time.March, 21, 16, 45, 22, 0, time.UTC),
			File:      "f1",
		}
		e := s.SetProcessed(entry)
		require.NoError(t, e)
	}
	{
		entry := feed.Entry{
			ChannelID: "chan1",
			VideoID:   "vid2",
			Title:     "title2",
			Published: time.Date(2022, time.March, 21, 17, 45, 22, 0, time.UTC),
			File:      "f2",
		}
		e := s.SetProcessed(entry)
		require.NoError(t, e)
	}
	{
		entry := feed.Entry{
			ChannelID: "chan1",
			VideoID:   "vid3",
			Title:     "title3",
			Published: time.Date(2022, time.March, 21, 18, 45, 22, 0, time.UTC),
			File:      "f3",
		}
		e := s.SetProcessed(entry)
		require.NoError(t, e)
	}

	count := s.CountProcessed()
	assert.Equal(t, 3, count)

	found, ts, err := s.CheckProcessed(feed.Entry{ChannelID: "chan1", VideoID: "vid2"})
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, time.Date(2022, time.March, 21, 17, 45, 22, 0, time.UTC), ts)

	found, ts, err = s.CheckProcessed(feed.Entry{ChannelID: "chan1", VideoID: "vid3"})
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, time.Date(2022, time.March, 21, 18, 45, 22, 0, time.UTC), ts)

	found, _, err = s.CheckProcessed(feed.Entry{ChannelID: "chan1", VideoID: "vidXXX"})
	require.NoError(t, err)
	assert.False(t, found)

	lst, err := s.ListProcessed()
	require.NoError(t, err)
	assert.Equal(t, 3, len(lst))
	assert.Equal(t, "dbff863dbc922f727afb93e949704da777739489 / 2022-03-21T16:45:22Z", lst[0])
	assert.Equal(t, "ae9a2ed8bbf505091e35b9d54ccb9dc58e35c205 / 2022-03-21T18:45:22Z", lst[1])
	assert.Equal(t, "a8fd9875c236fb27e26183b6df87f0cecb7a683f / 2022-03-21T17:45:22Z", lst[2])

	err = s.ResetProcessed(feed.Entry{ChannelID: "chan1", VideoID: "vid2"})
	require.NoError(t, err)
	found, _, err = s.CheckProcessed(feed.Entry{ChannelID: "chan1", VideoID: "vid2"})
	require.NoError(t, err)
	assert.False(t, found)

}

func TestBoltDB_Last(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "test.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	require.NoError(t, err)

	s := BoltDB{DB: db, Channels: []string{"chan1", "chan2"}}
	_, err = s.Last()
	assert.EqualError(t, err, "can't load last entry for chan1: no bucket for chan1")

	{
		entry := feed.Entry{
			ChannelID: "chan1",
			VideoID:   "vid1",
			Title:     "title1",
			Published: time.Date(2022, time.March, 21, 16, 45, 22, 0, time.UTC),
			File:      "f1",
		}
		created, e := s.Save(entry)
		require.NoError(t, e)
		assert.True(t, created)
	}
	{
		entry := feed.Entry{
			ChannelID: "chan1",
			VideoID:   "vid2",
			Title:     "title2",
			Published: time.Date(2022, time.March, 21, 17, 45, 22, 0, time.UTC),
			File:      "f2",
		}
		created, e := s.Save(entry)
		require.NoError(t, e)
		assert.True(t, created)
	}
	{
		entry := feed.Entry{
			ChannelID: "chan2",
			VideoID:   "vid3",
			Title:     "title3",
			Published: time.Date(2022, time.March, 21, 17, 46, 22, 0, time.UTC),
			File:      "f3",
		}
		created, e := s.Save(entry)
		require.NoError(t, e)
		assert.True(t, created)
	}

	res, err := s.Last()
	require.NoError(t, err)
	assert.Equal(t, "vid3", res.VideoID)
}

func TestStore_HistoryLogAndLoad(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "history-test.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	require.NoError(t, err)
	s := BoltDB{DB: db}

	// Empty bucket returns zero state.
	entries, total, err := s.LoadHistory("manual", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Len(t, entries, 0)

	t0 := time.Date(2026, time.June, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(t, s.LogHistory(HistoryEntry{
		Timestamp: t0,
		URL:       "https://youtube.com/watch?v=aaa",
		Title:     "First",
		Action:    "audio",
		VideoID:   "aaa",
		FeedName:  "manual",
	}))
	require.NoError(t, s.LogHistory(HistoryEntry{
		Timestamp: t0.Add(time.Hour),
		URL:       "https://youtube.com/watch?v=bbb",
		Title:     "Second",
		Action:    "voiceover",
		VideoID:   "bbb",
		FeedName:  "manual",
	}))
	require.NoError(t, s.LogHistory(HistoryEntry{
		Timestamp: t0.Add(2 * time.Hour),
		URL:       "https://nytimes.com/article/x",
		Title:     "Third",
		Action:    "article",
		FeedName:  "manual",
	}))
	// Different feed — should be isolated.
	require.NoError(t, s.LogHistory(HistoryEntry{
		Timestamp: t0,
		URL:       "https://youtube.com/watch?v=zzz",
		Title:     "Other feed",
		Action:    "audio",
		VideoID:   "zzz",
		FeedName:  "other",
	}))

	entries, total, err = s.LoadHistory("manual", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	require.Len(t, entries, 3)
	// Newest-first
	assert.Equal(t, "Third", entries[0].Title)
	assert.Equal(t, "Second", entries[1].Title)
	assert.Equal(t, "First", entries[2].Title)
	assert.Equal(t, "manual", entries[0].FeedName)
}

func TestStore_HistoryPagination(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "history-paginate.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	require.NoError(t, err)
	s := BoltDB{DB: db}

	t0 := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		require.NoError(t, s.LogHistory(HistoryEntry{
			Timestamp: t0.Add(time.Duration(i) * time.Minute),
			Title:     "n",
			VideoID:   "id-" + time.Duration(i).String(),
			FeedName:  "manual",
		}))
	}

	page0, total, err := s.LoadHistory("manual", 0, 10)
	require.NoError(t, err)
	assert.Equal(t, 25, total)
	assert.Len(t, page0, 10)

	page1, _, err := s.LoadHistory("manual", 10, 10)
	require.NoError(t, err)
	assert.Len(t, page1, 10)
	// No overlap between pages
	for _, a := range page0 {
		for _, b := range page1 {
			assert.NotEqual(t, a.VideoID, b.VideoID)
		}
	}

	page2, _, err := s.LoadHistory("manual", 20, 10)
	require.NoError(t, err)
	assert.Len(t, page2, 5)
}

func TestStore_MarkHistoryDeleted(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "history-delete.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	require.NoError(t, err)
	s := BoltDB{DB: db}

	t0 := time.Date(2026, time.June, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(t, s.LogHistory(HistoryEntry{
		Timestamp: t0, Title: "A", VideoID: "aaa", URL: "u1", FeedName: "manual",
	}))
	require.NoError(t, s.LogHistory(HistoryEntry{
		Timestamp: t0.Add(time.Hour), Title: "B", VideoID: "bbb", URL: "u2", FeedName: "manual",
	}))

	// Mark by videoID
	require.NoError(t, s.MarkHistoryDeleted("manual", "aaa", ""))
	entries, _, err := s.LoadHistory("manual", 0, 10)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	// Find each
	var foundA, foundB *HistoryEntry
	for i := range entries {
		if entries[i].VideoID == "aaa" {
			foundA = &entries[i]
		}
		if entries[i].VideoID == "bbb" {
			foundB = &entries[i]
		}
	}
	require.NotNil(t, foundA)
	require.NotNil(t, foundB)
	assert.True(t, foundA.Deleted)
	assert.False(t, foundA.DeletedAt.IsZero())
	assert.False(t, foundB.Deleted)

	// Idempotent: marking twice doesn't break (finds another candidate or no-op)
	require.NoError(t, s.MarkHistoryDeleted("manual", "aaa", ""))

	// Mark by URL when videoID empty
	require.NoError(t, s.LogHistory(HistoryEntry{
		Timestamp: t0.Add(2 * time.Hour), Title: "article", URL: "https://x.com/y", Action: "article", FeedName: "manual",
	}))
	require.NoError(t, s.MarkHistoryDeleted("manual", "", "https://x.com/y"))
	entries, _, err = s.LoadHistory("manual", 0, 10)
	require.NoError(t, err)
	for _, e := range entries {
		if e.URL == "https://x.com/y" {
			assert.True(t, e.Deleted)
		}
	}
}

func TestStore_UpdateEntry(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "test-update.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	require.NoError(t, err)

	s := BoltDB{DB: db}

	entry := feed.Entry{
		ChannelID: "chan1",
		VideoID:   "vid1",
		Title:     "original title",
		Published: time.Date(2022, time.March, 21, 16, 45, 22, 0, time.UTC),
	}
	_, err = s.Save(entry)
	require.NoError(t, err)

	entry.Title = "updated title"
	require.NoError(t, s.UpdateEntry(entry))

	res, err := s.Load("chan1", 100)
	require.NoError(t, err)
	require.Equal(t, 1, len(res), "update must not create a second record")
	assert.Equal(t, "updated title", res[0].Title)

	missing := feed.Entry{ChannelID: "chan1", VideoID: "nope"}
	assert.Error(t, s.UpdateEntry(missing))

	noBucket := feed.Entry{ChannelID: "nochan", VideoID: "vid1"}
	assert.Error(t, s.UpdateEntry(noBucket))
}

func TestStore_NotionMeta(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "test-notion.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	require.NoError(t, err)

	s := BoltDB{DB: db}

	res, err := s.LoadNotionMeta("bootstrap")
	require.NoError(t, err)
	assert.Nil(t, res, "absent key returns nil")

	require.NoError(t, s.SaveNotionMeta("bootstrap", []byte(`{"episodes_db":"x"}`)))
	res, err = s.LoadNotionMeta("bootstrap")
	require.NoError(t, err)
	assert.Equal(t, `{"episodes_db":"x"}`, string(res))

	require.NoError(t, s.SaveNotionMeta("bootstrap", []byte(`v2`)))
	res, err = s.LoadNotionMeta("bootstrap")
	require.NoError(t, err)
	assert.Equal(t, "v2", string(res), "overwrite works")
}

func TestStore_NotesJobs(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "test-notes-jobs.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	require.NoError(t, err)

	s := BoltDB{DB: db}

	// empty queue
	_, ok, err := s.ClaimNextNotesJob()
	require.NoError(t, err)
	assert.False(t, ok)

	mkJob := func(n int, sourceID string) NotesJobRecord {
		return NotesJobRecord{
			ID: fmt.Sprintf("%020d-%s", n, sourceID), SourceID: sourceID,
			URL: "https://youtu.be/" + sourceID, Level: "md",
			Status: NotesJobQueued, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
	}
	require.NoError(t, s.SaveNotesJob(mkJob(1, "aaa")))
	require.NoError(t, s.SaveNotesJob(mkJob(2, "bbb")))

	assert.Error(t, s.SaveNotesJob(NotesJobRecord{}), "empty id rejected")

	// FIFO claim order + status change
	job1, ok, err := s.ClaimNextNotesJob()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "aaa", job1.SourceID)
	assert.Equal(t, NotesJobProcessing, job1.Status)

	job2, ok, err := s.ClaimNextNotesJob()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "bbb", job2.SourceID)

	_, ok, err = s.ClaimNextNotesJob()
	require.NoError(t, err)
	assert.False(t, ok, "nothing left to claim")

	// active detection covers processing too
	active, err := s.HasActiveNotesJob("aaa")
	require.NoError(t, err)
	assert.True(t, active)
	active, err = s.HasActiveNotesJob("ccc")
	require.NoError(t, err)
	assert.False(t, active)

	// finish one, fail one
	job1.Status = NotesJobDone
	job1.UpdatedAt = time.Now().Add(-30 * 24 * time.Hour) // old, prunable
	require.NoError(t, s.SaveNotesJob(job1))
	job2.Status, job2.Error = NotesJobFailed, "boom"
	require.NoError(t, s.SaveNotesJob(job2))

	active, err = s.HasActiveNotesJob("aaa")
	require.NoError(t, err)
	assert.False(t, active, "done job is not active")

	count, err := s.CountNotesJobs(NotesJobFailed)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// newest-first listing
	jobs, err := s.LoadNotesJobs("", 0)
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	assert.Equal(t, "bbb", jobs[0].SourceID)

	// requeue processing (none right now)
	n, err := s.ResetProcessingNotesJobs()
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	// prune removes only old finished records
	n, err = s.DeleteOldNotesJobs(time.Now().Add(-14 * 24 * time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	jobs, err = s.LoadNotesJobs("", 0)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "bbb", jobs[0].SourceID)
}

func TestStore_NotesJobs_Priority(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "test-notes-jobs-prio.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	require.NoError(t, err)
	s := BoltDB{DB: db}

	mkJob := func(n int, sourceID string, prio int) NotesJobRecord {
		return NotesJobRecord{
			ID: fmt.Sprintf("%020d-%s", n, sourceID), SourceID: sourceID,
			Level: "md", Priority: prio, Status: NotesJobQueued,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
	}
	// three background (playlist) jobs queued first, then two user links arrive.
	// store only compares the int; the bulk=0/user=1 policy lives in proc.
	require.NoError(t, s.SaveNotesJob(mkJob(1, "bulk1", 0)))
	require.NoError(t, s.SaveNotesJob(mkJob(2, "bulk2", 0)))
	require.NoError(t, s.SaveNotesJob(mkJob(3, "bulk3", 0)))
	require.NoError(t, s.SaveNotesJob(mkJob(4, "user1", 1)))
	require.NoError(t, s.SaveNotesJob(mkJob(5, "user2", 1)))

	// user links jump ahead of the bulk backlog, FIFO among themselves
	claim := func() string {
		j, ok, cerr := s.ClaimNextNotesJob()
		require.NoError(t, cerr)
		require.True(t, ok)
		return j.SourceID
	}
	assert.Equal(t, "user1", claim(), "higher priority first")
	assert.Equal(t, "user2", claim(), "then the other user link (FIFO)")
	assert.Equal(t, "bulk1", claim(), "then oldest bulk")
	assert.Equal(t, "bulk2", claim())
	assert.Equal(t, "bulk3", claim())

	_, ok, err := s.ClaimNextNotesJob()
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestStore_PendingActions(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "test-pending-actions.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	require.NoError(t, err)

	s := BoltDB{DB: db}

	// empty bucket: unknown token, no error
	_, ok, err := s.TakePendingAction("nope")
	require.NoError(t, err)
	assert.False(t, ok, "unknown token before any save")

	require.NoError(t, s.SavePendingAction(PendingActionRecord{
		Token: "tok1", Kind: "yt", VideoIDs: []string{"aaaaaaaaaaa", "bbbbbbbbbbb"},
		ChatID: 42, OrigMsgID: 7, CreatedAt: time.Now(),
	}))
	require.NoError(t, s.SavePendingAction(PendingActionRecord{
		Token: "tok2", Kind: "article", URL: "https://example.com", ChatID: 42, CreatedAt: time.Now(),
	}))

	rec, ok, err := s.TakePendingAction("tok1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "yt", rec.Kind)
	assert.Equal(t, []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}, rec.VideoIDs)
	assert.EqualValues(t, 42, rec.ChatID)
	assert.Equal(t, 7, rec.OrigMsgID)

	// take is one-shot: second take of the same token misses (double-tap guard)
	_, ok, err = s.TakePendingAction("tok1")
	require.NoError(t, err)
	assert.False(t, ok, "token consumed on first take")

	// empty token rejected
	require.Error(t, s.SavePendingAction(PendingActionRecord{Token: ""}))

	// GC removes only records older than cutoff
	require.NoError(t, s.SavePendingAction(PendingActionRecord{
		Token: "old", Kind: "yt", CreatedAt: time.Now().Add(-time.Hour),
	}))
	n, err := s.DeleteOldPendingActions(time.Now().Add(-30 * time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the hour-old record is gc'd")

	_, ok, err = s.TakePendingAction("old")
	require.NoError(t, err)
	assert.False(t, ok, "gc'd token gone")

	_, ok, err = s.TakePendingAction("tok2")
	require.NoError(t, err)
	assert.True(t, ok, "fresh record survives gc")
}

func TestStore_TriageActions(t *testing.T) {
	tmpfile := filepath.Join(os.TempDir(), "test-triage-actions.db")
	defer os.Remove(tmpfile)

	db, err := bolt.Open(tmpfile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	require.NoError(t, err)
	s := BoltDB{DB: db}

	// GetPendingAction on empty bucket: miss, no error
	_, ok, err := s.GetPendingAction("nope")
	require.NoError(t, err)
	assert.False(t, ok)

	created := time.Now().Add(-time.Minute).UTC()
	require.NoError(t, s.SavePendingAction(PendingActionRecord{
		Token: "tri1", Kind: "triage",
		VideoIDs: []string{"aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc"},
		Titles:   []string{"One", "Two", "Three"},
		ChatID:   9, CreatedAt: created,
	}))

	// non-destructive read returns titles and survives repeated reads
	rec, ok, err := s.GetPendingAction("tri1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "triage", rec.Kind)
	assert.Equal(t, []string{"One", "Two", "Three"}, rec.Titles)
	assert.Empty(t, rec.Done)
	_, ok, _ = s.GetPendingAction("tri1")
	assert.True(t, ok, "GetPendingAction does not consume the record")

	// mark one done: refreshes CreatedAt and records the id
	now := time.Now().UTC()
	rec, ok, err = s.TouchPendingActionDone("tri1", "bbbbbbbbbbb", now)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, []string{"bbbbbbbbbbb"}, rec.Done)
	assert.True(t, rec.CreatedAt.After(created), "touch refreshes TTL clock")

	// marking the same id again is idempotent (no duplicate in Done)
	rec, _, err = s.TouchPendingActionDone("tri1", "bbbbbbbbbbb", now)
	require.NoError(t, err)
	assert.Equal(t, []string{"bbbbbbbbbbb"}, rec.Done)

	// a second id appends
	rec, _, err = s.TouchPendingActionDone("tri1", "aaaaaaaaaaa", now)
	require.NoError(t, err)
	assert.Equal(t, []string{"bbbbbbbbbbb", "aaaaaaaaaaa"}, rec.Done)

	// touch with empty videoID only refreshes the clock, leaves Done intact
	rec, _, err = s.TouchPendingActionDone("tri1", "", now.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, []string{"bbbbbbbbbbb", "aaaaaaaaaaa"}, rec.Done)

	// touch on unknown token: miss, no error
	_, ok, err = s.TouchPendingActionDone("ghost", "x", now)
	require.NoError(t, err)
	assert.False(t, ok)
}

// Package store provides a store for the youtube service metadata
package store

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/hashicorp/go-multierror"
	bolt "go.etcd.io/bbolt"

	"github.com/umputun/feed-master/app/youtube/feed"
)

var processedBkt = []byte("processed")
var historyLogBkt = []byte("history_log")
var notionMetaBkt = []byte("notion_meta")
var notesJobsBkt = []byte("notes_jobs")
var notesBatchesBkt = []byte("notes_batches")
var pendingActionsBkt = []byte("pending_actions")
var refsIndexBkt = []byte("refs_index")
var queueCtrlBkt = []byte("queue_control")

// notes job statuses
const (
	NotesJobQueued     = "queued"
	NotesJobProcessing = "processing"
	NotesJobDone       = "done"
	NotesJobFailed     = "failed"
)

// NotesJobRecord is one persisted transcription/notes task. The queue lives in
// bolt (not in memory) so jobs survive container restarts; telegram message ids
// are stored so the worker can keep editing the same status message afterwards.
type NotesJobRecord struct {
	ID         string `json:"id"` // {unix_nanos padded}-{sourceID}, key order = FIFO
	URL        string `json:"url"`
	Title      string `json:"title,omitempty"` // human label for the queue view; from triage titles or backfilled from transcript frontmatter
	SourceID   string `json:"source_id"`
	Source     string `json:"source"`                   // "youtube" | "article"
	Level      string `json:"level"`                    // "md" | "notes"
	SumLength  string `json:"summary_length,omitempty"` // "" (normal) | "short" | "long", L2 only
	ReuseAudio string `json:"reuse_audio,omitempty"`
	// playlist origin: set when the job came from an expanded YouTube playlist, so
	// the resulting note's frontmatter (and the Mini App) can badge/group items
	// that arrived together. Empty PlaylistID = not from a playlist.
	PlaylistID    string    `json:"playlist_id,omitempty"`
	PlaylistTitle string    `json:"playlist_title,omitempty"`
	PlaylistIndex int       `json:"playlist_index,omitempty"` // 1-based position within the playlist
	Priority      int       `json:"priority,omitempty"`       // higher = claimed first; bulk=0, user=1
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	ChatID        int64     `json:"chat_id,omitempty"`
	StatusMsgID   int       `json:"status_msg_id,omitempty"` // for batch jobs this is the shared counter message
	OrigMsgID     int       `json:"orig_msg_id,omitempty"`
	BatchID       string    `json:"batch_id,omitempty"` // non-empty = member of a page-all batch reporting into one counter
}

// NotesBatchRecord tracks a "page-all" group of notes/audio jobs that report
// progress into a single shared status message with a live counter, instead of
// one "⏳ Queued…" message per item. It lives in bolt so the counter survives a
// deploy: the durable jobs keep completing and each terminal event atomically
// bumps Done/Failed here, then re-renders the shared message.
type NotesBatchRecord struct {
	ID          string    `json:"id"`
	ChatID      int64     `json:"chat_id"`
	StatusMsgID int       `json:"status_msg_id"`
	Label       string    `json:"label"` // e.g. "📓 стр 1"
	Total       int       `json:"total"`
	Done        int       `json:"done"`
	Failed      int       `json:"failed"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// HistoryEntry is an append-only record of a user-submitted item. Unlike
// the feed bucket (which is bounded by max_items and pruned by /del), the
// history log is permanent — entries are only marked Deleted, never removed.
type HistoryEntry struct {
	Timestamp time.Time `json:"ts"`
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Action    string    `json:"action"`             // "audio", "voiceover", "article"
	VideoID   string    `json:"video_id,omitempty"` // for YouTube actions
	Duration  string    `json:"duration,omitempty"`
	FeedName  string    `json:"feed"`
	Deleted   bool      `json:"deleted,omitempty"`
	DeletedAt time.Time `json:"deleted_at,omitempty"`
}

// ReferenceEntry is one indexed reference (a book/person/tool/... mentioned in an
// episode) in the lightweight cross-episode index. Populated by the notes (L2)
// pipeline alongside the Notion page write; lets /refs list every mention of a
// type across all episodes without re-reading Notion.
type ReferenceEntry struct {
	Type         string    `json:"type"` // книга/человек/инструмент/...
	Name         string    `json:"name"`
	Timecode     string    `json:"timecode,omitempty"`
	Quote        string    `json:"quote,omitempty"`
	EpisodeTitle string    `json:"episode_title"`
	EpisodeURL   string    `json:"episode_url,omitempty"` // original source link
	NotionURL    string    `json:"notion_url,omitempty"`  // episode page in Notion
	SourceID     string    `json:"source_id"`
	CreatedAt    time.Time `json:"created_at"`
}

// BoltDB store for metadata related to downloaded YouTube audio.
type BoltDB struct {
	*bolt.DB
	Channels []string // the list of configured channels ids
}

// Save to bolt, skip if found
func (s *BoltDB) Save(entry feed.Entry) (bool, error) {
	var created bool

	key, keyErr := s.key(entry)
	if keyErr != nil {
		return created, fmt.Errorf("failed to generate key for %s: %w", entry.VideoID, keyErr)
	}

	err := s.Update(func(tx *bolt.Tx) error {
		bucket, e := tx.CreateBucketIfNotExists([]byte(entry.ChannelID))
		if e != nil {
			return fmt.Errorf("create bucket %s: %w", entry.ChannelID, e)
		}
		if bucket.Get(key) != nil {
			return nil
		}

		jdata, jerr := json.Marshal(&entry)
		if jerr != nil {
			return fmt.Errorf("marshal entry %s: %w", entry.VideoID, jerr)
		}

		log.Printf("[INFO] save %s - %s", string(key), entry.String())

		e = bucket.Put(key, jdata)
		if e != nil {
			return fmt.Errorf("save entry %s: %w", entry.VideoID, e)
		}

		created = true
		return e
	})

	return created, err
}

// Exist checks if entry exists
func (s *BoltDB) Exist(entry feed.Entry) (bool, error) {
	var found bool

	key, keyErr := s.key(entry)
	if keyErr != nil {
		return false, fmt.Errorf("failed to generate key for %s: %w", entry.VideoID, keyErr)
	}

	err := s.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(entry.ChannelID))
		if bucket == nil {
			return nil
		}

		if bucket.Get(key) != nil {
			found = true
		}

		return nil
	})

	return found, err
}

// Load entries from bolt for a given channel, up to max in reverse order (from newest to oldest)
func (s *BoltDB) Load(channelID string, maximum int) ([]feed.Entry, error) {
	var result []feed.Entry

	err := s.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(channelID))
		if bucket == nil {
			return fmt.Errorf("no bucket for %s", channelID)
		}
		c := bucket.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var item feed.Entry
			if err := json.Unmarshal(v, &item); err != nil {
				log.Printf("[WARN] failed to unmarshal %s, %q: %v", channelID, string(v), err)
				continue
			}
			result = append(result, item)
			if maximum > 0 && len(result) >= maximum {
				break
			}
		}
		return nil
	})
	return result, err
}

// Last returns last (newest) entry across all channels
func (s *BoltDB) Last() (feed.Entry, error) {
	entries := []feed.Entry{}
	for _, channel := range s.Channels {
		last, err := s.Load(channel, 1)
		if err != nil {
			return feed.Entry{}, fmt.Errorf("can't load last entry for %s: %w", channel, err)
		}
		if len(last) > 0 {
			entries = append(entries, last[0])
		}
	}
	if len(entries) == 0 {
		return feed.Entry{}, errors.New("no entries")
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Published.After(entries[j].Published)
	})
	return entries[0], nil
}

// RemoveOld removes old entries from bolt and returns the list of removed entry.File
// the caller should delete the files
// important: this method returns the list of removed keys even if there was an error
func (s *BoltDB) RemoveOld(channelID string, keep int) ([]string, error) {
	deleted := 0
	var res []string

	err := s.Update(func(tx *bolt.Tx) (e error) {
		errs := new(multierror.Error)
		bucket := tx.Bucket([]byte(channelID))
		if bucket == nil {
			return fmt.Errorf("no bucket for %s", channelID)
		}
		recs := 0
		c := bucket.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			recs++
			if recs > keep {
				var item feed.Entry
				if err := json.Unmarshal(v, &item); err != nil {
					log.Printf("[WARN] failed to unmarshal, %v", err)
					continue
				}
				if err := bucket.Delete(k); err != nil {
					errs = multierror.Append(errs, fmt.Errorf("failed to delete %s (%s): %w", string(k), item.File, err))
					continue
				}
				res = append(res, item.File)
				deleted++
			}
		}
		return errs.ErrorOrNil()
	})

	return res, err
}

// Remove entry matched by vidoID and channelID
func (s *BoltDB) Remove(entry feed.Entry) error {

	err := s.Update(func(tx *bolt.Tx) (e error) {
		bucket := tx.Bucket([]byte(entry.ChannelID))
		if bucket == nil {
			return fmt.Errorf("no bucket for %s", entry.ChannelID)
		}
		c := bucket.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var item feed.Entry
			if err := json.Unmarshal(v, &item); err != nil {
				log.Printf("[WARN] failed to unmarshal, %v", err)
				continue
			}
			if item.VideoID == entry.VideoID {
				if err := bucket.Delete(k); err != nil {
					return fmt.Errorf("failed to delete %s (%s): %w", string(k), item.VideoID, err)
				}
				log.Printf("[INFO] delete %s - %s", string(k), item.String())
				return nil
			}
		}
		return nil
	})

	return err
}

// UpdateEntry replaces the stored entry matched by VideoID and ChannelID,
// keeping its original key. Returns an error if the entry is not found.
func (s *BoltDB) UpdateEntry(entry feed.Entry) error {
	return s.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(entry.ChannelID))
		if bucket == nil {
			return fmt.Errorf("no bucket for %s", entry.ChannelID)
		}
		c := bucket.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var item feed.Entry
			if err := json.Unmarshal(v, &item); err != nil {
				log.Printf("[WARN] failed to unmarshal, %v", err)
				continue
			}
			if item.VideoID == entry.VideoID {
				jdata, jerr := json.Marshal(&entry)
				if jerr != nil {
					return fmt.Errorf("marshal entry %s: %w", entry.VideoID, jerr)
				}
				if err := bucket.Put(k, jdata); err != nil {
					return fmt.Errorf("failed to update %s (%s): %w", string(k), item.VideoID, err)
				}
				log.Printf("[INFO] update %s - %s", string(k), entry.String())
				return nil
			}
		}
		return fmt.Errorf("entry %s not found in %s", entry.VideoID, entry.ChannelID)
	})
}

// SetProcessed sets processed status with ts for a given channel+video
func (s *BoltDB) SetProcessed(entry feed.Entry) error {

	key, keyErr := s.procKey(entry)
	if keyErr != nil {
		return fmt.Errorf("failed to generate key for %s: %w", entry.VideoID, keyErr)
	}

	err := s.Update(func(tx *bolt.Tx) error {
		bucket, e := tx.CreateBucketIfNotExists(processedBkt)
		if e != nil {
			return fmt.Errorf("create bucket %s: %w", processedBkt, e)
		}
		if bucket.Get(key) != nil {
			return nil
		}

		log.Printf("[INFO] set processed %s - %s", string(key), entry.String())

		e = bucket.Put(key, []byte(entry.Published.Format(time.RFC3339)))
		if e != nil {
			return fmt.Errorf("save processed %s: %w", entry.VideoID, e)
		}
		return e
	})

	return err
}

// ResetProcessed resets processed status for a given channel+video
func (s *BoltDB) ResetProcessed(entry feed.Entry) error {

	key, keyErr := s.procKey(entry)
	if keyErr != nil {
		return fmt.Errorf("failed to generate key for %s: %w", entry.VideoID, keyErr)
	}

	err := s.Update(func(tx *bolt.Tx) error {
		bucket, e := tx.CreateBucketIfNotExists(processedBkt)
		if e != nil {
			return fmt.Errorf("create bucket %s: %w", processedBkt, e)
		}
		if bucket.Get(key) == nil {
			return nil
		}

		log.Printf("[INFO] reset processed %s - %s", string(key), entry.String())

		e = bucket.Delete(key)
		if e != nil {
			return fmt.Errorf("reset processed %s: %w", entry.VideoID, e)
		}
		return e
	})

	return err
}

// CheckProcessed get processed status and returns timestamp for a given channel+video
// returns found=true if was set before and also the timestamp from stored entry.Published
func (s *BoltDB) CheckProcessed(entry feed.Entry) (found bool, ts time.Time, err error) {

	key, keyErr := s.procKey(entry)
	if keyErr != nil {
		return false, time.Time{}, fmt.Errorf("failed to generate key for %s: %w", entry.VideoID, keyErr)
	}

	err = s.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(processedBkt)
		if bucket == nil {
			return nil
		}

		res := bucket.Get(key)
		if res == nil {
			found = false
			return nil
		}
		found = true
		var tsErr error
		ts, tsErr = time.Parse(time.RFC3339, string(res))
		return tsErr
	})

	return found, ts, err
}

// CountProcessed returns the number of processed entries stored in processedBkt
func (s *BoltDB) CountProcessed() (count int) {

	_ = s.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(processedBkt)
		if bucket == nil {
			return nil
		}

		count = bucket.Stats().KeyN
		return nil
	})
	return count
}

// ListProcessed returns processed entries stored in processedBkt
func (s *BoltDB) ListProcessed() (res []string, err error) {

	err = s.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(processedBkt)
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			res = append(res, string(k)+" / "+string(v))
		}
		return nil
	})
	return res, err
}

// LogHistory appends an entry to the per-feed history log. Key is
// "{unix_nanos}-{videoID|hash}" so cursor.Prev() yields newest-first.
// Duplicate calls for the same (feed,videoID) within the same nanosecond
// are deduped by the timestamp prefix, but distinct submissions of the
// same URL are intentionally separate entries (each is a user action).
func (s *BoltDB) LogHistory(entry HistoryEntry) error {
	if entry.FeedName == "" {
		return fmt.Errorf("LogHistory: empty FeedName")
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}

	disc := entry.VideoID
	if disc == "" {
		h := sha1.New()
		_, _ = h.Write([]byte(entry.URL))
		disc = fmt.Sprintf("%x", h.Sum(nil))[:12]
	}
	key := []byte(fmt.Sprintf("%020d-%s", entry.Timestamp.UnixNano(), disc))

	return s.Update(func(tx *bolt.Tx) error {
		root, e := tx.CreateBucketIfNotExists(historyLogBkt)
		if e != nil {
			return fmt.Errorf("create history_log bucket: %w", e)
		}
		feedBucket, e := root.CreateBucketIfNotExists([]byte(entry.FeedName))
		if e != nil {
			return fmt.Errorf("create history feed bucket %s: %w", entry.FeedName, e)
		}
		jdata, jerr := json.Marshal(&entry)
		if jerr != nil {
			return fmt.Errorf("marshal history entry: %w", jerr)
		}
		log.Printf("[INFO] history log %s - %s - %s", entry.Action, entry.Title, entry.URL)
		return feedBucket.Put(key, jdata)
	})
}

// MarkHistoryDeleted finds the most recent non-deleted history entry for
// the given feed+videoID and flips it to Deleted=true (preserving the
// rest of the record). Matches by VideoID when present, otherwise by URL.
func (s *BoltDB) MarkHistoryDeleted(feedName, videoID, url string) error {
	return s.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(historyLogBkt)
		if root == nil {
			return nil
		}
		feedBucket := root.Bucket([]byte(feedName))
		if feedBucket == nil {
			return nil
		}
		c := feedBucket.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var item HistoryEntry
			if err := json.Unmarshal(v, &item); err != nil {
				continue
			}
			if item.Deleted {
				continue
			}
			match := (videoID != "" && item.VideoID == videoID) ||
				(videoID == "" && url != "" && item.URL == url)
			if !match {
				continue
			}
			item.Deleted = true
			item.DeletedAt = time.Now().UTC()
			jdata, jerr := json.Marshal(&item)
			if jerr != nil {
				return fmt.Errorf("marshal history entry: %w", jerr)
			}
			return feedBucket.Put(k, jdata)
		}
		return nil
	})
}

// LoadHistory returns up to `limit` entries newest-first starting at `offset`
// for the given feed, plus the total count.
func (s *BoltDB) LoadHistory(feedName string, offset, limit int) (entries []HistoryEntry, total int, err error) {
	err = s.View(func(tx *bolt.Tx) error {
		root := tx.Bucket(historyLogBkt)
		if root == nil {
			return nil
		}
		feedBucket := root.Bucket([]byte(feedName))
		if feedBucket == nil {
			return nil
		}
		total = feedBucket.Stats().KeyN

		c := feedBucket.Cursor()
		skipped := 0
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			if skipped < offset {
				skipped++
				continue
			}
			if len(entries) >= limit {
				break
			}
			var item HistoryEntry
			if jerr := json.Unmarshal(v, &item); jerr != nil {
				log.Printf("[WARN] history unmarshal: %v", jerr)
				continue
			}
			entries = append(entries, item)
		}
		return nil
	})
	return entries, total, err
}

// SaveReferences replaces every indexed reference for one episode (keyed by
// sourceID). Re-running /notes for the same source overwrites its references
// rather than duplicating them. Empty refs clears the episode's sub-bucket.
func (s *BoltDB) SaveReferences(sourceID string, refs []ReferenceEntry) error {
	if sourceID == "" {
		return fmt.Errorf("SaveReferences: empty sourceID")
	}
	return s.Update(func(tx *bolt.Tx) error {
		root, e := tx.CreateBucketIfNotExists(refsIndexBkt)
		if e != nil {
			return fmt.Errorf("create refs_index bucket: %w", e)
		}
		if root.Bucket([]byte(sourceID)) != nil {
			if e := root.DeleteBucket([]byte(sourceID)); e != nil {
				return fmt.Errorf("reset refs sub-bucket %s: %w", sourceID, e)
			}
		}
		if len(refs) == 0 {
			return nil
		}
		sub, e := root.CreateBucket([]byte(sourceID))
		if e != nil {
			return fmt.Errorf("create refs sub-bucket %s: %w", sourceID, e)
		}
		for i, r := range refs {
			r.SourceID = sourceID
			if r.CreatedAt.IsZero() {
				r.CreatedAt = time.Now().UTC()
			}
			jdata, jerr := json.Marshal(&r)
			if jerr != nil {
				return fmt.Errorf("marshal reference: %w", jerr)
			}
			if e := sub.Put([]byte(fmt.Sprintf("%04d", i)), jdata); e != nil {
				return e
			}
		}
		return nil
	})
}

// ListReferences returns every indexed reference across all episodes, optionally
// filtered to one type (empty = all). The caller groups and paginates.
func (s *BoltDB) ListReferences(typeFilter string) (refs []ReferenceEntry, err error) {
	err = s.View(func(tx *bolt.Tx) error {
		root := tx.Bucket(refsIndexBkt)
		if root == nil {
			return nil
		}
		return root.ForEach(func(name, v []byte) error {
			if v != nil { // leaf key, not a sub-bucket — none expected at root
				return nil
			}
			sub := root.Bucket(name)
			if sub == nil {
				return nil
			}
			return sub.ForEach(func(_, jv []byte) error {
				var r ReferenceEntry
				if jerr := json.Unmarshal(jv, &r); jerr != nil {
					return nil
				}
				if typeFilter != "" && r.Type != typeFilter {
					return nil
				}
				refs = append(refs, r)
				return nil
			})
		})
	})
	return refs, err
}

// ReferenceTypeCounts returns the number of indexed references per type, plus
// the grand total. Backs the argument-less /refs overview.
func (s *BoltDB) ReferenceTypeCounts() (counts map[string]int, total int, err error) {
	counts = map[string]int{}
	err = s.View(func(tx *bolt.Tx) error {
		root := tx.Bucket(refsIndexBkt)
		if root == nil {
			return nil
		}
		return root.ForEach(func(name, v []byte) error {
			if v != nil {
				return nil
			}
			sub := root.Bucket(name)
			if sub == nil {
				return nil
			}
			return sub.ForEach(func(_, jv []byte) error {
				var r ReferenceEntry
				if jerr := json.Unmarshal(jv, &r); jerr != nil {
					return nil
				}
				counts[r.Type]++
				total++
				return nil
			})
		})
	})
	return counts, total, err
}

// SaveNotesJob creates or updates a notes job record keyed by its ID
func (s *BoltDB) SaveNotesJob(job NotesJobRecord) error {
	if job.ID == "" {
		return errors.New("notes job id is empty")
	}
	return s.Update(func(tx *bolt.Tx) error {
		bucket, e := tx.CreateBucketIfNotExists(notesJobsBkt)
		if e != nil {
			return fmt.Errorf("create bucket %s: %w", notesJobsBkt, e)
		}
		jdata, jerr := json.Marshal(&job)
		if jerr != nil {
			return fmt.Errorf("marshal notes job %s: %w", job.ID, jerr)
		}
		return bucket.Put([]byte(job.ID), jdata)
	})
}

// ClaimNextNotesJob atomically takes the highest-priority queued job and marks
// it processing; jobs of equal priority are taken oldest-first (FIFO). This lets
// a single user link (priority 1) jump ahead of a background playlist-triage
// batch (priority 0) without interrupting the job already in flight. ok is false
// when the queue is empty. Safe with multiple workers: the whole selection +
// claim happens inside a single bolt update transaction.
func (s *BoltDB) ClaimNextNotesJob() (job NotesJobRecord, ok bool, err error) {
	err = s.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(notesJobsBkt)
		if bucket == nil {
			return nil
		}
		var bestKey []byte
		var best NotesJobRecord
		c := bucket.Cursor()
		// cursor is key-ascending = oldest-first (keys are zero-padded nanos), so
		// replacing only on strictly higher priority keeps FIFO within a priority
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var item NotesJobRecord
			if jerr := json.Unmarshal(v, &item); jerr != nil {
				log.Printf("[WARN] notes job unmarshal %s: %v", string(k), jerr)
				continue
			}
			if item.Status != NotesJobQueued {
				continue
			}
			if bestKey == nil || item.Priority > best.Priority {
				bestKey = append([]byte(nil), k...) // copy: cursor reuses k's memory
				best = item
			}
		}
		if bestKey == nil {
			return nil
		}
		best.Status = NotesJobProcessing
		best.UpdatedAt = time.Now().UTC()
		jdata, jerr := json.Marshal(&best)
		if jerr != nil {
			return fmt.Errorf("marshal notes job %s: %w", best.ID, jerr)
		}
		if perr := bucket.Put(bestKey, jdata); perr != nil {
			return fmt.Errorf("claim notes job %s: %w", best.ID, perr)
		}
		job, ok = best, true
		return nil
	})
	return job, ok, err
}

// LoadNotesJobs returns jobs newest-first, filtered by status ("" = all),
// up to limit (0 = no limit)
func (s *BoltDB) LoadNotesJobs(status string, limit int) (jobs []NotesJobRecord, err error) {
	err = s.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(notesJobsBkt)
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var item NotesJobRecord
			if jerr := json.Unmarshal(v, &item); jerr != nil {
				continue
			}
			if status != "" && item.Status != status {
				continue
			}
			jobs = append(jobs, item)
			if limit > 0 && len(jobs) >= limit {
				break
			}
		}
		return nil
	})
	return jobs, err
}

// CountNotesJobs counts jobs with the given status
func (s *BoltDB) CountNotesJobs(status string) (count int, err error) {
	jobs, err := s.LoadNotesJobs(status, 0)
	return len(jobs), err
}

// HasActiveNotesJob reports whether a queued or processing job exists for sourceID
func (s *BoltDB) HasActiveNotesJob(sourceID string) (found bool, err error) {
	err = s.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(notesJobsBkt)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var item NotesJobRecord
			if jerr := json.Unmarshal(v, &item); jerr != nil {
				return nil //nolint:nilerr // skip broken record
			}
			if item.SourceID == sourceID && (item.Status == NotesJobQueued || item.Status == NotesJobProcessing) {
				found = true
			}
			return nil
		})
	})
	return found, err
}

// SetNotesPaused persists the queue pause flag. Paused workers finish the
// in-flight job but claim no new ones. Survives restarts.
func (s *BoltDB) SetNotesPaused(paused bool) error {
	return s.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(queueCtrlBkt)
		if err != nil {
			return fmt.Errorf("failed to create queue_control bucket: %w", err)
		}
		if paused {
			return bucket.Put([]byte("paused"), []byte("1"))
		}
		return bucket.Delete([]byte("paused"))
	})
}

// NotesPaused reports whether the notes queue is currently paused.
func (s *BoltDB) NotesPaused() (paused bool, err error) {
	err = s.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(queueCtrlBkt)
		if bucket == nil {
			return nil
		}
		paused = bucket.Get([]byte("paused")) != nil
		return nil
	})
	return paused, err
}

// ResetProcessingNotesJobs returns interrupted processing jobs back to queued.
// Called on startup: a processing record with no live worker means the previous
// run died mid-job.
func (s *BoltDB) ResetProcessingNotesJobs() (count int, err error) {
	err = s.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(notesJobsBkt)
		if bucket == nil {
			return nil
		}
		// collect first: mutating a bucket while iterating its cursor is unsafe
		var stuck []NotesJobRecord
		if ferr := bucket.ForEach(func(_, v []byte) error {
			var item NotesJobRecord
			if jerr := json.Unmarshal(v, &item); jerr == nil && item.Status == NotesJobProcessing {
				stuck = append(stuck, item)
			}
			return nil
		}); ferr != nil {
			return ferr
		}
		for _, item := range stuck {
			item.Status = NotesJobQueued
			item.UpdatedAt = time.Now().UTC()
			jdata, jerr := json.Marshal(&item)
			if jerr != nil {
				return fmt.Errorf("marshal notes job %s: %w", item.ID, jerr)
			}
			if perr := bucket.Put([]byte(item.ID), jdata); perr != nil {
				return fmt.Errorf("requeue notes job %s: %w", item.ID, perr)
			}
			count++
		}
		return nil
	})
	return count, err
}

// DeleteOldNotesJobs removes done/failed records updated before cutoff,
// keeping the bucket from growing forever
func (s *BoltDB) DeleteOldNotesJobs(cutoff time.Time) (count int, err error) {
	err = s.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(notesJobsBkt)
		if bucket == nil {
			return nil
		}
		// collect first: deleting while iterating a cursor skips entries
		var old [][]byte
		if ferr := bucket.ForEach(func(k, v []byte) error {
			var item NotesJobRecord
			if jerr := json.Unmarshal(v, &item); jerr != nil {
				return nil //nolint:nilerr // skip broken record
			}
			if (item.Status == NotesJobDone || item.Status == NotesJobFailed) && !item.UpdatedAt.After(cutoff) {
				old = append(old, append([]byte(nil), k...))
			}
			return nil
		}); ferr != nil {
			return ferr
		}
		for _, k := range old {
			if derr := bucket.Delete(k); derr != nil {
				return fmt.Errorf("delete notes job %s: %w", string(k), derr)
			}
			count++
		}
		return nil
	})
	return count, err
}

// SaveNotesBatch creates or updates a batch counter record keyed by its ID.
func (s *BoltDB) SaveNotesBatch(rec NotesBatchRecord) error {
	if rec.ID == "" {
		return errors.New("notes batch id is empty")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	rec.UpdatedAt = time.Now().UTC()
	return s.Update(func(tx *bolt.Tx) error {
		bucket, e := tx.CreateBucketIfNotExists(notesBatchesBkt)
		if e != nil {
			return fmt.Errorf("create bucket %s: %w", notesBatchesBkt, e)
		}
		data, jerr := json.Marshal(&rec)
		if jerr != nil {
			return fmt.Errorf("marshal notes batch %s: %w", rec.ID, jerr)
		}
		return bucket.Put([]byte(rec.ID), data)
	})
}

// IncrNotesBatch atomically bumps Done/Failed for a batch and returns the updated
// record. ok is false when the batch id is unknown (GC'd or never created).
func (s *BoltDB) IncrNotesBatch(id string, doneDelta, failedDelta int) (rec NotesBatchRecord, ok bool, err error) {
	err = s.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(notesBatchesBkt)
		if bucket == nil {
			return nil
		}
		v := bucket.Get([]byte(id))
		if v == nil {
			return nil
		}
		if jerr := json.Unmarshal(v, &rec); jerr != nil {
			return fmt.Errorf("unmarshal notes batch %s: %w", id, jerr)
		}
		rec.Done += doneDelta
		rec.Failed += failedDelta
		rec.UpdatedAt = time.Now().UTC()
		data, jerr := json.Marshal(&rec)
		if jerr != nil {
			return fmt.Errorf("marshal notes batch %s: %w", id, jerr)
		}
		if perr := bucket.Put([]byte(id), data); perr != nil {
			return perr
		}
		ok = true
		return nil
	})
	return rec, ok, err
}

// DeleteOldNotesBatches removes batch records older than cutoff, keeping the
// bucket bounded (a batch is short-lived — its jobs finish within hours).
func (s *BoltDB) DeleteOldNotesBatches(cutoff time.Time) (count int, err error) {
	err = s.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(notesBatchesBkt)
		if bucket == nil {
			return nil
		}
		var old [][]byte
		if ferr := bucket.ForEach(func(k, v []byte) error {
			var item NotesBatchRecord
			if jerr := json.Unmarshal(v, &item); jerr != nil {
				return nil //nolint:nilerr // skip broken record
			}
			if !item.UpdatedAt.After(cutoff) {
				old = append(old, append([]byte(nil), k...))
			}
			return nil
		}); ferr != nil {
			return ferr
		}
		for _, k := range old {
			if derr := bucket.Delete(k); derr != nil {
				return fmt.Errorf("delete notes batch %s: %w", string(k), derr)
			}
			count++
		}
		return nil
	})
	return count, err
}

// PendingActionRecord is one inline-menu choice waiting for the user to tap a
// button. It lives in bolt (not in memory) so the menu survives container
// restarts — Watchtower recreates the container on every deploy, which used to
// drop every open menu ("⏱ Меню просрочено"). Only primitives are stored: the
// telebot message is reconstructed from ChatID/OrigMsgID on take.
type PendingActionRecord struct {
	Token    string   `json:"token"`
	Kind     string   `json:"kind"` // "yt" | "article" | "podcast" | "podcast_show" | "triage"
	VideoIDs []string `json:"video_ids,omitempty"`
	Titles   []string `json:"titles,omitempty"` // parallel to VideoIDs, for the triage list
	Done     []string `json:"done,omitempty"`   // video IDs already queued from a triage list
	URL      string   `json:"url,omitempty"`
	// playlist origin (set when this menu/triage came from an expanded playlist);
	// VideoIDs order = playlist order, so a video's index is its slice position.
	PlaylistID    string    `json:"playlist_id,omitempty"`
	PlaylistTitle string    `json:"playlist_title,omitempty"`
	ChatID        int64     `json:"chat_id"`
	OrigMsgID     int       `json:"orig_msg_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// SavePendingAction stores a pending inline-menu action keyed by its token
func (s *BoltDB) SavePendingAction(rec PendingActionRecord) error {
	if rec.Token == "" {
		return errors.New("pending action token is empty")
	}
	return s.Update(func(tx *bolt.Tx) error {
		bucket, e := tx.CreateBucketIfNotExists(pendingActionsBkt)
		if e != nil {
			return fmt.Errorf("create bucket %s: %w", pendingActionsBkt, e)
		}
		jdata, jerr := json.Marshal(&rec)
		if jerr != nil {
			return fmt.Errorf("marshal pending action %s: %w", rec.Token, jerr)
		}
		return bucket.Put([]byte(rec.Token), jdata)
	})
}

// TakePendingAction atomically loads and deletes the action for token in one
// transaction, so a double-tap can't run the same action twice. ok is false
// when the token is unknown (expired, already used, or lost on an old deploy).
func (s *BoltDB) TakePendingAction(token string) (rec PendingActionRecord, ok bool, err error) {
	err = s.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(pendingActionsBkt)
		if bucket == nil {
			return nil
		}
		v := bucket.Get([]byte(token))
		if v == nil {
			return nil
		}
		if jerr := json.Unmarshal(v, &rec); jerr != nil {
			// broken record: drop it so it can't wedge the button forever
			return bucket.Delete([]byte(token))
		}
		ok = true
		return bucket.Delete([]byte(token))
	})
	return rec, ok, err
}

// GetPendingAction loads the action for token without deleting it. Unlike
// TakePendingAction (one-shot menus), a triage list must survive many taps and
// page flips, so it is read non-destructively. ok is false when the token is
// unknown or the record is broken.
func (s *BoltDB) GetPendingAction(token string) (rec PendingActionRecord, ok bool, err error) {
	err = s.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(pendingActionsBkt)
		if bucket == nil {
			return nil
		}
		v := bucket.Get([]byte(token))
		if v == nil {
			return nil
		}
		if jerr := json.Unmarshal(v, &rec); jerr != nil {
			return nil // broken record: treated as a miss
		}
		ok = true
		return nil
	})
	return rec, ok, err
}

// TouchPendingActionDone refreshes the record's CreatedAt (so an actively used
// triage list is not garbage-collected mid-session) and, when videoID is
// non-empty, records it in Done so the same item is not queued twice. It is a
// no-op miss when the token is unknown. Returns the updated record.
func (s *BoltDB) TouchPendingActionDone(token, videoID string, now time.Time) (rec PendingActionRecord, ok bool, err error) {
	err = s.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(pendingActionsBkt)
		if bucket == nil {
			return nil
		}
		v := bucket.Get([]byte(token))
		if v == nil {
			return nil
		}
		if jerr := json.Unmarshal(v, &rec); jerr != nil {
			return nil
		}
		ok = true
		rec.CreatedAt = now
		if videoID != "" {
			seen := false
			for _, d := range rec.Done {
				if d == videoID {
					seen = true
					break
				}
			}
			if !seen {
				rec.Done = append(rec.Done, videoID)
			}
		}
		jdata, jerr := json.Marshal(&rec)
		if jerr != nil {
			return fmt.Errorf("marshal pending action %s: %w", token, jerr)
		}
		return bucket.Put([]byte(token), jdata)
	})
	return rec, ok, err
}

// DeleteOldPendingActions removes menus older than cutoff (the user never tapped
// a button), keeping the bucket from growing forever
func (s *BoltDB) DeleteOldPendingActions(cutoff time.Time) (count int, err error) {
	err = s.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(pendingActionsBkt)
		if bucket == nil {
			return nil
		}
		// collect first: deleting while iterating a cursor skips entries
		var old [][]byte
		if ferr := bucket.ForEach(func(k, v []byte) error {
			var item PendingActionRecord
			if jerr := json.Unmarshal(v, &item); jerr != nil {
				old = append(old, append([]byte(nil), k...)) // broken record: prune it too
				return nil                                   //nolint:nilerr
			}
			if !item.CreatedAt.After(cutoff) {
				old = append(old, append([]byte(nil), k...))
			}
			return nil
		}); ferr != nil {
			return ferr
		}
		for _, k := range old {
			if derr := bucket.Delete(k); derr != nil {
				return fmt.Errorf("delete pending action %s: %w", string(k), derr)
			}
			count++
		}
		return nil
	})
	return count, err
}

// SaveNotionMeta stores an opaque value in the notion metadata bucket.
// Used for Notion database IDs and episode page mappings; the store stays
// unaware of the payload structure.
func (s *BoltDB) SaveNotionMeta(key string, data []byte) error {
	return s.Update(func(tx *bolt.Tx) error {
		bucket, e := tx.CreateBucketIfNotExists(notionMetaBkt)
		if e != nil {
			return fmt.Errorf("create bucket %s: %w", notionMetaBkt, e)
		}
		return bucket.Put([]byte(key), data)
	})
}

// DeleteNotionMetaPrefix removes all notion metadata keys with the prefix.
// Used when databases are re-bootstrapped: old page mappings point to pages
// in deleted databases and must not short-circuit new writes.
func (s *BoltDB) DeleteNotionMetaPrefix(prefix string) (count int, err error) {
	err = s.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(notionMetaBkt)
		if bucket == nil {
			return nil
		}
		var keys [][]byte
		if ferr := bucket.ForEach(func(k, _ []byte) error {
			if len(k) >= len(prefix) && string(k[:len(prefix)]) == prefix {
				keys = append(keys, append([]byte(nil), k...))
			}
			return nil
		}); ferr != nil {
			return ferr
		}
		for _, k := range keys {
			if derr := bucket.Delete(k); derr != nil {
				return fmt.Errorf("delete notion meta %s: %w", string(k), derr)
			}
			count++
		}
		return nil
	})
	return count, err
}

// LoadNotionMeta returns the stored value for key, nil if absent
func (s *BoltDB) LoadNotionMeta(key string) (res []byte, err error) {
	err = s.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(notionMetaBkt)
		if bucket == nil {
			return nil
		}
		if v := bucket.Get([]byte(key)); v != nil {
			res = append([]byte(nil), v...)
		}
		return nil
	})
	return res, err
}

func (s *BoltDB) key(entry feed.Entry) ([]byte, error) {
	h := sha1.New()
	if _, err := h.Write([]byte(entry.VideoID)); err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("%d-%x", entry.Published.Unix(), h.Sum(nil))), nil
}

func (s *BoltDB) procKey(entry feed.Entry) ([]byte, error) {
	h := sha1.New()
	if _, err := h.Write([]byte(entry.ChannelID + "::" + entry.VideoID)); err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("%x", h.Sum(nil))), nil
}

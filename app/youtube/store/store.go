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

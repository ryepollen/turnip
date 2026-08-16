package proc

import (
	"fmt"
	"time"

	ytfeed "github.com/umputun/feed-master/app/youtube/feed"
)

// dupState classifies whether an incoming video is already known to us, read
// cheaply from bolt (feed + notes queue) without touching yt-dlp. It lets the
// menu / triage list warn about duplicates *before* any expensive work — so a
// re-sent playlist (e.g. after a cookie hiccup dropped part of it) shows which
// items are already handled and which are genuinely new.
type dupState int

const (
	dupNone       dupState = iota // never seen
	dupInFeed                     // already an episode in the feed
	dupInQueue                    // a notes job (queued/processing) exists for it
	dupNotesReady                 // an L1 transcript/notes file already exists
)

// videoDupStatus reports the most relevant "already known" state for videoID,
// read cheaply from bolt + a file stat, no network. Priority: feed episode →
// in-flight notes job → finished transcript. when is the feed-add time for
// dupInFeed, zero otherwise. nil-safe: returns dupNone when the store is absent.
func (t *TelegramBot) videoDupStatus(videoID string) (dupState, time.Time) {
	if t.Store == nil || videoID == "" {
		return dupNone, time.Time{}
	}
	entry := ytfeed.Entry{ChannelID: t.FeedName, VideoID: videoID}
	if found, ts, err := t.Store.CheckProcessed(entry); err == nil && found {
		return dupInFeed, ts
	}
	if found, err := t.Store.HasActiveNotesJob(videoID); err == nil && found {
		return dupInQueue, time.Time{}
	}
	if t.NotesSvc != nil && t.NotesSvc.HasNotes(videoID) {
		return dupNotesReady, time.Time{}
	}
	return dupNone, time.Time{}
}

// videoHandledFor reports whether the specific action is already satisfied for a
// video, so page-all skips only what that action would redo: 🎵 (audio) skips
// videos already in the feed; 📓/📄 (notes/md) skip a finished transcript or an
// in-flight notes job. Feed presence does not block notes and vice versa.
func (t *TelegramBot) videoHandledFor(videoID, action string) bool {
	if t.Store == nil || videoID == "" {
		return false
	}
	switch action {
	case "au":
		if found, _, err := t.Store.CheckProcessed(ytfeed.Entry{ChannelID: t.FeedName, VideoID: videoID}); err == nil && found {
			return true
		}
	case "md", "nt":
		if found, err := t.Store.HasActiveNotesJob(videoID); err == nil && found {
			return true
		}
		if t.NotesSvc != nil && t.NotesSvc.HasNotes(videoID) {
			return true
		}
	}
	return false
}

// countKnown returns how many of the ids are already in the feed or the queue.
func (t *TelegramBot) countKnown(ids []string) int {
	n := 0
	for _, id := range ids {
		if st, _ := t.videoDupStatus(id); st != dupNone {
			n++
		}
	}
	return n
}

// dupMarker is the compact suffix appended to a triage row for a known video.
func dupMarker(st dupState, when time.Time) string {
	switch st {
	case dupInFeed:
		if when.IsZero() {
			return "✅ в ленте"
		}
		return "✅ в ленте (" + humanAgo(when, time.Now()) + ")"
	case dupInQueue:
		return "⏳ в очереди"
	case dupNotesReady:
		return "✅ конспект готов"
	default:
		return ""
	}
}

// dupNote is the one-line hint shown under the single-link action menu; the menu
// buttons themselves are the "and what to do about it" — tapping still works.
func dupNote(st dupState, when time.Time) string {
	switch st {
	case dupInFeed:
		if when.IsZero() {
			return "⚠️ Уже в ленте. Жми действие, если нужно заново."
		}
		return fmt.Sprintf("⚠️ Уже в ленте (%s). Жми действие, если нужно заново.", humanAgo(when, time.Now()))
	case dupInQueue:
		return "⏳ Уже в очереди на конспект."
	case dupNotesReady:
		return "✅ Конспект уже готов. Жми действие, если нужно заново."
	default:
		return ""
	}
}

// humanAgo renders a coarse Russian "N дней назад" for dedup hints.
func humanAgo(when, now time.Time) string {
	d := now.Sub(when)
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		return fmt.Sprintf("%d мин назад", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d ч назад", int(d.Hours()))
	default:
		return pluralDays(int(d.Hours()/24)) + " назад"
	}
}

// pluralDays renders the Russian plural for a day count: 1 день, 2 дня, 5 дней.
func pluralDays(n int) string {
	if nn := n % 100; nn >= 11 && nn <= 14 {
		return fmt.Sprintf("%d дней", n)
	}
	switch n % 10 {
	case 1:
		return fmt.Sprintf("%d день", n)
	case 2, 3, 4:
		return fmt.Sprintf("%d дня", n)
	default:
		return fmt.Sprintf("%d дней", n)
	}
}

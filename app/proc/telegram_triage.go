package proc

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/go-pkgz/lgr"
	tb "gopkg.in/tucnak/telebot.v2"

	ytfeed "github.com/umputun/feed-master/app/youtube/feed"
)

// triagePageSize is small: every video carries a row of three action buttons,
// more rows would not fit a phone screen (mirrors mdListPageSize)
const triagePageSize = 5

// triageThreshold is the batch size above which a playlist / multi-link message
// opens a triage list instead of blindly fanning one action out to every video.
// At or below it the quick 4-button menu is fine; beyond it a single tap used to
// spam one "queued" message per video (a 78-video playlist → 50 messages).
const triageThreshold = 5

// triageItemTitle returns the display title for the i-th video, falling back to
// the id when titles are missing (a pasted multi-link batch has no titles).
func triageItemTitle(rec pendingTriage, i int) string {
	if i < len(rec.titles) && rec.titles[i] != "" {
		return rec.titles[i]
	}
	return rec.videoIDs[i]
}

// pendingTriage is the subset of a stored triage record the renderer needs.
type pendingTriage struct {
	token         string
	videoIDs      []string
	titles        []string
	done          map[string]bool
	playlistID    string // set when the list came from an expanded playlist
	playlistTitle string
}

// playlistRef returns the playlist origin for the i-th video (nil when the list
// isn't a playlist — e.g. a pasted multi-link batch). videoIDs order = playlist
// order, so the 1-based index is i+1.
func (r pendingTriage) playlistRef(i int) *playlistRef {
	if r.playlistID == "" {
		return nil
	}
	return &playlistRef{ID: r.playlistID, Title: r.playlistTitle, Index: i + 1}
}

// loadTriage reads a triage pending action from bolt (non-destructive — the list
// must survive many taps and page flips). ok is false when the token is unknown
// (expired, GC'd, or lost on a very old deploy).
func (t *TelegramBot) loadTriage(token string) (pendingTriage, bool) {
	rec, ok, err := t.Store.GetPendingAction(token)
	if err != nil {
		log.Printf("[WARN] load triage %s: %v", token, err)
		return pendingTriage{}, false
	}
	if !ok || rec.Kind != "triage" {
		return pendingTriage{}, false
	}
	done := make(map[string]bool, len(rec.Done))
	for _, d := range rec.Done {
		done[d] = true
	}
	return pendingTriage{
		token: token, videoIDs: rec.VideoIDs, titles: rec.Titles, done: done,
		playlistID: rec.PlaylistID, playlistTitle: rec.PlaylistTitle,
	}, true
}

// buildTriageMessage renders one page of the triage list: a numbered list with a
// per-video row of [🎵 audio] [📄 md] [📓 notes] buttons, a page-wide row, and
// navigation. Videos queued this session are marked "✓ поставил"; videos
// already in the feed or the notes queue from before carry a dedup marker.
func (t *TelegramBot) buildTriageMessage(rec pendingTriage, page int) (string, *tb.ReplyMarkup) {
	total := len(rec.videoIDs)
	pages := (total + triagePageSize - 1) / triagePageSize
	if pages == 0 {
		pages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start := page * triagePageSize
	end := start + triagePageSize
	if end > total {
		end = total
	}

	markup := &tb.ReplyMarkup{}
	notes := t.NotesSvc != nil

	var b strings.Builder
	fmt.Fprintf(&b, "📺 %d видео — стр %d/%d\n\n", total, page+1, pages)
	for i := start; i < end; i++ {
		id := rec.videoIDs[i]
		title := triageItemTitle(rec, i)
		mark := ""
		if rec.done[id] {
			mark = " ✓ поставил"
		} else if st, when := t.videoDupStatus(id); st != dupNone {
			mark = " · " + dupMarker(st, when)
		}
		fmt.Fprintf(&b, "%d. %s%s\n", i+1, truncateRunes(title, 60), mark)

		num := i + 1
		row := []tb.InlineButton{
			*markup.Data(fmt.Sprintf("🎵 %d", num), "tri_act", fmt.Sprintf("a=au|t=%s|i=%d|p=%d", rec.token, i, page)).Inline(),
		}
		if notes {
			row = append(row,
				*markup.Data(fmt.Sprintf("📄 %d", num), "tri_act", fmt.Sprintf("a=md|t=%s|i=%d|p=%d", rec.token, i, page)).Inline(),
				*markup.Data(fmt.Sprintf("📓 %d", num), "tri_act", fmt.Sprintf("a=nt|t=%s|i=%d|p=%d", rec.token, i, page)).Inline(),
			)
		}
		markup.InlineKeyboard = append(markup.InlineKeyboard, row)
	}
	b.WriteString("\n🎵 аудио · 📄 MD · 📓 Notion — по одному, или всю страницу ниже")

	// page-wide row: apply one action to every not-yet-queued video on this page
	pageRow := []tb.InlineButton{
		*markup.Data("🎵 стр", "tri_act", fmt.Sprintf("a=pgau|t=%s|p=%d", rec.token, page)).Inline(),
	}
	if notes {
		pageRow = append(pageRow,
			*markup.Data("📓 стр", "tri_act", fmt.Sprintf("a=pgnt|t=%s|p=%d", rec.token, page)).Inline(),
		)
	}
	markup.InlineKeyboard = append(markup.InlineKeyboard, pageRow)

	if pages > 1 {
		prev, next := page-1, page+1
		if prev < 0 {
			prev = pages - 1
		}
		if next >= pages {
			next = 0
		}
		btnPrev := markup.Data("◀︎", "tri_page", fmt.Sprintf("t=%s|p=%d", rec.token, prev))
		btnNext := markup.Data("▶︎", "tri_page", fmt.Sprintf("t=%s|p=%d", rec.token, next))
		markup.InlineKeyboard = append(markup.InlineKeyboard, []tb.InlineButton{*btnPrev.Inline(), *btnNext.Inline()})
	}
	return b.String(), markup
}

// handleTriagePageCallback flips list pages
func (t *TelegramBot) handleTriagePageCallback(c *tb.Callback) {
	token, page := parseTriageData(c.Data)
	rec, ok := t.loadTriage(token)
	if !ok {
		_, _ = t.Bot.Edit(c.Message, "⏱ Список просрочен — пришли ссылку снова")
		_ = t.Bot.Respond(c)
		return
	}
	msg, markup := t.buildTriageMessage(rec, page)
	_, _ = t.Bot.Edit(c.Message, msg, markup)
	_ = t.Bot.Respond(c)
}

// handleTriageActionCallback runs a per-item or per-page action, marks the
// touched videos ✓, and re-renders the same page in place (no fan-out of status
// messages beyond the ones the user deliberately triggered).
func (t *TelegramBot) handleTriageActionCallback(c *tb.Callback) {
	action, token, idx, page := "", "", -1, 0
	for _, part := range strings.Split(c.Data, "|") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "a":
			action = kv[1]
		case "t":
			token = kv[1]
		case "i":
			_, _ = fmt.Sscanf(kv[1], "%d", &idx)
		case "p":
			_, _ = fmt.Sscanf(kv[1], "%d", &page)
		}
	}

	rec, ok := t.loadTriage(token)
	if !ok {
		_, _ = t.Bot.Edit(c.Message, "⏱ Список просрочен — пришли ссылку снова")
		_ = t.Bot.Respond(c)
		return
	}
	chat := c.Message.Chat

	switch action {
	case "au", "md", "nt":
		if idx < 0 || idx >= len(rec.videoIDs) {
			_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Bad item"})
			return
		}
		id := rec.videoIDs[idx]
		if rec.done[id] {
			_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Уже в очереди"})
			return
		}
		t.triageQueueOne(chat, token, action, id, triageItemTitle(rec, idx), rec.playlistRef(idx))
	case "pgau", "pgnt":
		itemAction, emoji := "au", "🎵"
		if action == "pgnt" {
			itemAction, emoji = "nt", "📓"
		}
		start := page * triagePageSize
		end := start + triagePageSize
		if end > len(rec.videoIDs) {
			end = len(rec.videoIDs)
		}
		var ids, titles []string
		var refs []*playlistRef
		for i := start; i < end; i++ {
			id := rec.videoIDs[i]
			if rec.done[id] {
				continue
			}
			// page-all means "queue everything genuinely new here" (the point of
			// re-sending a partially-done playlist). Skip is action-aware: 🎵
			// skips only feed dups, 📓 skips finished-or-queued notes — so a
			// playlist done as notes isn't blocked from audio and vice versa.
			if t.videoHandledFor(id, itemAction) {
				continue
			}
			ids = append(ids, id)
			titles = append(titles, triageItemTitle(rec, i))
			refs = append(refs, rec.playlistRef(i))
		}
		if len(ids) == 0 {
			_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Всё на странице уже в ленте/очереди"})
			return
		}
		// one shared status message with a live counter, not N "⏳ Queued…"
		t.triageQueueBatch(chat, token, itemAction, fmt.Sprintf("%s стр %d", emoji, page+1), ids, titles, refs)
	default:
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Bad action"})
		return
	}

	// re-read to reflect the freshly marked ✓ (triageQueueOne touched bolt)
	rec, ok = t.loadTriage(token)
	if !ok {
		_ = t.Bot.Respond(c)
		return
	}
	msg, markup := t.buildTriageMessage(rec, page)
	_, _ = t.Bot.Edit(c.Message, msg, markup)
	_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Поставил в очередь"})
}

// triageQueueOne enqueues one video's action with its own status message, then
// marks it done in bolt (which also refreshes the list's TTL so an active triage
// session is not garbage-collected mid-work). Audio uses the existing download
// flow; md/nt go through the notes queue.
func (t *TelegramBot) triageQueueOne(chat *tb.Chat, token, action, videoID, title string, pl *playlistRef) {
	videoURL := "https://www.youtube.com/watch?v=" + videoID
	switch action {
	case "au":
		st, _ := t.Bot.Send(chat, "⏳ Queued…")
		go func() {
			if err := t.processVideo(context.Background(), chat, st, nil, videoID); err != nil {
				log.Printf("[ERROR] triage audio %s: %v", videoID, err)
				if ytfeed.IsCookieError(err.Error()) {
					_, _ = t.Bot.Edit(st, "❌ YouTube cookies expired. Run update-cookies.sh to fix.")
				} else {
					_, _ = t.Bot.Edit(st, fmt.Sprintf("❌ Error: %v", err))
				}
			}
		}()
	case "md", "nt":
		level := "md"
		if action == "nt" {
			level = "notes"
		}
		st, _ := t.Bot.Send(chat, "⏳ Queued…")
		// triage items come from a big playlist/batch: background priority so a
		// single user link queued meanwhile jumps ahead of the remaining items
		t.enqueueNotesJob(st, nil, videoURL, level, "", notesPriorityBulk, pl, title)
	}
	if _, _, err := t.Store.TouchPendingActionDone(token, videoID, time.Now().UTC()); err != nil {
		log.Printf("[WARN] mark triage done %s/%s: %v", token, videoID, err)
	}
}

// parseTriageData pulls token and page out of "t=<tok>|p=<n>" callback data.
func parseTriageData(data string) (token string, page int) {
	for _, part := range strings.Split(data, "|") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			token = kv[1]
		case "p":
			_, _ = fmt.Sscanf(kv[1], "%d", &page)
		}
	}
	return token, page
}

// truncateRunes shortens s to at most max runes, appending an ellipsis when cut.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

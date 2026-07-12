package proc

import (
	"context"
	"fmt"
	"strings"
	"time"

	tb "gopkg.in/tucnak/telebot.v2"
)

// pubListPageSize keeps the /archive view phone-friendly: every item carries
// a row of two action buttons
const pubListPageSize = 5

// handleFeeds lists the publishing platform's category feeds with their
// subscription URLs
func (t *TelegramBot) handleFeeds(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		return
	}
	if t.Pub == nil {
		_, _ = t.Bot.Send(m.Chat, "Издательская платформа не настроена (R2_* + FEED_SECRET)")
		return
	}
	cats, err := t.Pub.Categories()
	if err != nil {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("Error: %v", err))
		return
	}
	if len(cats) == 0 {
		_, _ = t.Bot.Send(m.Chat, "Пока нет ни одной ленты. Закинь файл в Inbox — появится.")
		return
	}

	var b strings.Builder
	b.WriteString("📻 Ленты платформы:\n\n")
	for _, cat := range cats {
		eps, lerr := t.Pub.EpisodeList(cat)
		if lerr != nil {
			continue
		}
		var totalSec int
		for _, ep := range eps {
			totalSec += ep.DurationSec
		}
		fmt.Fprintf(&b, "• %s — %d эп., %s\n%s\n\n", cat, len(eps),
			t.formatDuration(time.Duration(totalSec)*time.Second), t.Pub.FeedURL(cat))
	}
	b.WriteString("Управление эпизодами: /archive <категория>")
	_, _ = t.Bot.Send(m.Chat, b.String())
}

// handleArchive shows a category's episodes with per-item archive/requeue buttons
func (t *TelegramBot) handleArchive(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		return
	}
	if t.Pub == nil {
		_, _ = t.Bot.Send(m.Chat, "Издательская платформа не настроена (R2_* + FEED_SECRET)")
		return
	}
	parts := strings.Fields(m.Text)
	if len(parts) < 2 {
		cats, _ := t.Pub.Categories()
		_, _ = t.Bot.Send(m.Chat, "Usage: /archive <категория>\nЕсть: "+strings.Join(cats, ", "))
		return
	}
	category := strings.TrimSpace(parts[1])
	msg, markup, err := t.buildArchiveList(category, 0)
	if err != nil {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("Error: %v", err))
		return
	}
	_, _ = t.Bot.Send(m.Chat, msg, markup)
}

// buildArchiveList renders one page of a category with 🗄 (archive) and
// 🔁 (requeue) buttons per episode
func (t *TelegramBot) buildArchiveList(category string, page int) (string, *tb.ReplyMarkup, error) {
	eps, err := t.Pub.EpisodeList(category)
	if err != nil {
		return "", nil, err
	}
	if len(eps) == 0 {
		return fmt.Sprintf("В «%s» нет опубликованных эпизодов", category), &tb.ReplyMarkup{}, nil
	}

	total := len(eps)
	pages := (total + pubListPageSize - 1) / pubListPageSize
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start, end := page*pubListPageSize, (page+1)*pubListPageSize
	if end > total {
		end = total
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🗄 %s (%d) — стр %d/%d:\n\n", category, total, page+1, pages)
	for i := start; i < end; i++ {
		ep := eps[i]
		fmt.Fprintf(&b, "%d. %s (%s, %d МБ)\n", i+1, ep.Title,
			t.formatDuration(time.Duration(ep.DurationSec)*time.Second), ep.SizeBytes/1024/1024)
	}
	b.WriteString("\n🗄 в архив (из ленты и R2) · 🔁 переобработать")

	markup := &tb.ReplyMarkup{}
	if pages > 1 {
		prev, next := page-1, page+1
		if prev < 0 {
			prev = pages - 1
		}
		if next >= pages {
			next = 0
		}
		btnPrev := markup.Data("◀︎", "pub_pg", fmt.Sprintf("c=%s|p=%d", category, prev))
		btnNext := markup.Data("▶︎", "pub_pg", fmt.Sprintf("c=%s|p=%d", category, next))
		markup.InlineKeyboard = append(markup.InlineKeyboard, []tb.InlineButton{*btnPrev.Inline(), *btnNext.Inline()})
	}
	for i := start; i < end; i++ {
		btnAr := markup.Data(fmt.Sprintf("🗄 %d", i+1), "pub_act", fmt.Sprintf("a=ar|c=%s|i=%d|p=%d", category, i, page))
		btnRq := markup.Data(fmt.Sprintf("🔁 %d", i+1), "pub_act", fmt.Sprintf("a=rq|c=%s|i=%d|p=%d", category, i, page))
		markup.InlineKeyboard = append(markup.InlineKeyboard, []tb.InlineButton{*btnAr.Inline(), *btnRq.Inline()})
	}
	return b.String(), markup, nil
}

// handlePubPageCallback flips /archive pages
func (t *TelegramBot) handlePubPageCallback(c *tb.Callback) {
	if t.Pub == nil {
		_ = t.Bot.Respond(c)
		return
	}
	category, page, _ := parsePubCallback(c.Data)
	msg, markup, err := t.buildArchiveList(category, page)
	if err != nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Ошибка"})
		return
	}
	_, _ = t.Bot.Edit(c.Message, msg, markup)
	_ = t.Bot.Respond(c)
}

// handlePubActionCallback runs archive/requeue for one episode
func (t *TelegramBot) handlePubActionCallback(c *tb.Callback) {
	if t.Pub == nil {
		_ = t.Bot.Respond(c)
		return
	}
	category, page, rest := parsePubCallback(c.Data)
	action, idx := rest["a"], -1
	if v, ok := rest["i"]; ok {
		_, _ = fmt.Sscanf(v, "%d", &idx)
	}

	eps, err := t.Pub.EpisodeList(category)
	if err != nil || idx < 0 || idx >= len(eps) {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Список устарел, открой /archive заново"})
		return
	}
	file := eps[idx].File

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch action {
	case "ar":
		err = t.Pub.Archive(ctx, category, file)
	case "rq":
		err = t.Pub.Requeue(ctx, category, file)
	default:
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Bad action"})
		return
	}
	if err != nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: fmt.Sprintf("Ошибка: %v", err)})
		return
	}

	toast := "В архиве"
	if action == "rq" {
		toast = "Забыт — watcher переобработает"
	}
	msg, markup, berr := t.buildArchiveList(category, page)
	if berr == nil {
		_, _ = t.Bot.Edit(c.Message, msg, markup)
	}
	_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: toast})
}

// parsePubCallback extracts category/page plus the raw kv map
func parsePubCallback(data string) (category string, page int, kv map[string]string) {
	kv = map[string]string{}
	for _, part := range strings.Split(data, "|") {
		if k, v, ok := strings.Cut(part, "="); ok {
			kv[k] = v
		}
	}
	category = kv["c"]
	if v, ok := kv["p"]; ok {
		_, _ = fmt.Sscanf(v, "%d", &page)
	}
	return category, page, kv
}

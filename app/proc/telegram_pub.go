package proc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/umputun/feed-master/app/publisher"
	tb "gopkg.in/tucnak/telebot.v2"
)

// libPageSize keeps the /library catalog phone-friendly: each work is a row
// with a single action button
const libPageSize = 5

// handleFeeds lists the publishing platform's category feeds: what work is
// active in each and the subscription URL
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
		_, _ = t.Bot.Send(m.Chat, "Пока нет ни одной ленты. Выбери работу: /library")
		return
	}

	var b strings.Builder
	b.WriteString("📻 Ленты платформы:\n\n")
	for _, cat := range cats {
		eps, lerr := t.Pub.EpisodeList(cat)
		if lerr != nil {
			continue
		}
		active, _ := t.Pub.ActiveWork(cat)
		var totalSec int
		for _, ep := range eps {
			totalSec += ep.DurationSec
		}
		if active == "" {
			fmt.Fprintf(&b, "• %s — пусто\n%s\n\n", cat, t.Pub.FeedURL(cat))
			continue
		}
		fmt.Fprintf(&b, "• %s — ▶ «%s» (%d эп., %s)\n%s\n\n", cat, active, len(eps),
			t.formatDuration(time.Duration(totalSec)*time.Second), t.Pub.FeedURL(cat))
	}
	b.WriteString("Выбрать/сменить работу: /library")
	_, _ = t.Bot.Send(m.Chat, b.String())
}

// handleLibrary shows the library catalog (all works, sleeping + active) with a
// per-work ▶ Слушать / ⏹ Снять button and a category filter.
func (t *TelegramBot) handleLibrary(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		return
	}
	if t.Pub == nil {
		_, _ = t.Bot.Send(m.Chat, "Издательская платформа не настроена (R2_* + FEED_SECRET)")
		return
	}
	filter := "all"
	if parts := strings.Fields(m.Text); len(parts) > 1 {
		filter = strings.TrimSpace(parts[1])
	}
	msg, markup, err := t.buildLibraryList(filter, 0)
	if err != nil {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("Error: %v", err))
		return
	}
	_, _ = t.Bot.Send(m.Chat, msg, markup)
}

// filterCatalog narrows the catalog to one category ("" / "all" = everything)
func filterCatalog(all []publisher.CatWork, filter string) []publisher.CatWork {
	if filter == "" || filter == "all" {
		return all
	}
	var out []publisher.CatWork
	for _, w := range all {
		if w.Category == filter {
			out = append(out, w)
		}
	}
	return out
}

// catalogCategories returns the distinct categories present in the catalog, in
// first-seen (already sorted) order
func catalogCategories(all []publisher.CatWork) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range all {
		if !seen[w.Category] {
			seen[w.Category] = true
			out = append(out, w.Category)
		}
	}
	return out
}

// buildLibraryList renders one page of the (optionally filtered) catalog. The
// action buttons index into the FILTERED list, so render and callback agree.
func (t *TelegramBot) buildLibraryList(filter string, page int) (string, *tb.ReplyMarkup, error) {
	all, err := t.Pub.Catalog()
	if err != nil {
		return "", nil, err
	}
	if len(all) == 0 {
		return "Библиотека пуста. Положи работу в originals/{категория}/{работа}/", &tb.ReplyMarkup{}, nil
	}
	works := filterCatalog(all, filter)
	if len(works) == 0 {
		return fmt.Sprintf("В «%s» нет работ", filter), &tb.ReplyMarkup{}, nil
	}

	total := len(works)
	pages := (total + libPageSize - 1) / libPageSize
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start, end := page*libPageSize, (page+1)*libPageSize
	if end > total {
		end = total
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🗂 Библиотека — %s (%d) стр %d/%d:\n\n", filter, total, page+1, pages)
	for i := start; i < end; i++ {
		w := works[i]
		mark := "· "
		if w.Active {
			mark = "▶ "
		}
		meta := fmt.Sprintf("%d частей", w.Parts)
		if w.Seasons > 0 {
			meta += fmt.Sprintf(" · %d сез.", w.Seasons)
		}
		if !w.Finite {
			meta += " · ∞"
		}
		fmt.Fprintf(&b, "%s%d. %s [%s] — %s\n", mark, i+1, w.Title, w.Category, meta)
	}
	b.WriteString("\n▶ активна в ленте · тап «Слушать» = залить в R2 и в ленту (вытолкнет прежнюю)")

	markup := &tb.ReplyMarkup{}

	// category filter tabs (only when more than one category exists)
	cats := catalogCategories(all)
	if len(cats) > 1 {
		row := []tb.InlineButton{*markup.Data("Все", "lib_pg", "f=all|p=0").Inline()}
		for _, c := range cats {
			row = append(row, *markup.Data(c, "lib_pg", fmt.Sprintf("f=%s|p=0", c)).Inline())
		}
		markup.InlineKeyboard = append(markup.InlineKeyboard, row)
	}

	if pages > 1 {
		prev, next := page-1, page+1
		if prev < 0 {
			prev = pages - 1
		}
		if next >= pages {
			next = 0
		}
		btnPrev := markup.Data("◀︎", "lib_pg", fmt.Sprintf("f=%s|p=%d", filter, prev))
		btnNext := markup.Data("▶︎", "lib_pg", fmt.Sprintf("f=%s|p=%d", filter, next))
		markup.InlineKeyboard = append(markup.InlineKeyboard, []tb.InlineButton{*btnPrev.Inline(), *btnNext.Inline()})
	}

	for i := start; i < end; i++ {
		w := works[i]
		if w.Active {
			btn := markup.Data(fmt.Sprintf("⏹ Снять %d", i+1), "lib_act", fmt.Sprintf("a=off|i=%d|f=%s|p=%d", i, filter, page))
			markup.InlineKeyboard = append(markup.InlineKeyboard, []tb.InlineButton{*btn.Inline()})
			continue
		}
		btn := markup.Data(fmt.Sprintf("▶ Слушать %d", i+1), "lib_act", fmt.Sprintf("a=on|i=%d|f=%s|p=%d", i, filter, page))
		markup.InlineKeyboard = append(markup.InlineKeyboard, []tb.InlineButton{*btn.Inline()})
	}
	return b.String(), markup, nil
}

// handleLibPageCallback flips catalog pages / switches the category filter
func (t *TelegramBot) handleLibPageCallback(c *tb.Callback) {
	if t.Pub == nil {
		_ = t.Bot.Respond(c)
		return
	}
	kv := parseKVCallback(c.Data)
	msg, markup, err := t.buildLibraryList(kv["f"], atoiOr(kv["p"], 0))
	if err != nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Ошибка"})
		return
	}
	_, _ = t.Bot.Edit(c.Message, msg, markup)
	_ = t.Bot.Respond(c)
}

// handleLibActionCallback activates or deactivates a work. Both are potentially
// long (uploading/deleting a whole work in R2), so they run in a goroutine that
// edits a dedicated status message; the catalog view is refreshed when done.
func (t *TelegramBot) handleLibActionCallback(c *tb.Callback) {
	if t.Pub == nil {
		_ = t.Bot.Respond(c)
		return
	}
	kv := parseKVCallback(c.Data)
	filter, page := kv["f"], atoiOr(kv["p"], 0)
	idx := atoiOr(kv["i"], -1)

	all, err := t.Pub.Catalog()
	if err != nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Ошибка каталога"})
		return
	}
	works := filterCatalog(all, filter)
	if idx < 0 || idx >= len(works) {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Список устарел, открой /library заново"})
		return
	}
	w := works[idx]

	switch kv["a"] {
	case "on":
		if w.Active {
			_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Уже в ленте"})
			return
		}
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "⏳ Ставлю в ленту…"})
		go t.runActivate(c, w, filter, page)
	case "off":
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "⏹ Снимаю…"})
		go t.runDeactivate(c, w, filter, page)
	default:
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Bad action"})
	}
}

// runActivate uploads a work into its category feed with a live progress
// message, then refreshes the catalog view. In variant A remote mode the upload
// is delegated to the Mac agent via the queue instead.
func (t *TelegramBot) runActivate(c *tb.Callback, w publisher.CatWork, filter string, page int) {
	if t.ActQueue != nil {
		t.enqueueActivation(c, publisher.OpActivate, w, filter, page)
		return
	}
	status, _ := t.Bot.Send(c.Message.Chat, fmt.Sprintf("⏳ Активирую «%s» (0/%d)…", w.Title, w.Parts))
	step := w.Parts / 10
	if step < 1 {
		step = 1
	}
	progress := func(done, total int) {
		if status == nil {
			return
		}
		if done == total || done%step == 0 {
			_, _ = t.Bot.Edit(status, fmt.Sprintf("⏳ «%s»: залито %d/%d…", w.Title, done, total))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour) // big works are gigabytes
	defer cancel()
	err := t.Pub.Activate(ctx, w.Category, w.Slug, progress)
	if err != nil {
		if status != nil {
			_, _ = t.Bot.Edit(status, fmt.Sprintf("⚠️ «%s»: %v", w.Title, err))
		}
		return
	}
	if status != nil {
		_, _ = t.Bot.Edit(status, fmt.Sprintf("▶ «%s» в ленте (%d частей)\nЛента: %s", w.Title, w.Parts, t.Pub.FeedURL(w.Category)))
	}
	t.refreshLibraryView(c, filter, page)
}

// runDeactivate empties a category feed and refreshes the catalog view. In
// variant A remote mode the R2 eviction is done by the VM finalize watcher after
// the queued request is picked up.
func (t *TelegramBot) runDeactivate(c *tb.Callback, w publisher.CatWork, filter string, page int) {
	if t.ActQueue != nil {
		t.enqueueActivation(c, publisher.OpDeactivate, w, filter, page)
		return
	}
	status, _ := t.Bot.Send(c.Message.Chat, fmt.Sprintf("⏹ Снимаю «%s»…", w.Title))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := t.Pub.Deactivate(ctx, w.Category); err != nil {
		if status != nil {
			_, _ = t.Bot.Edit(status, fmt.Sprintf("⚠️ %s: %v", w.Category, err))
		}
		return
	}
	if status != nil {
		_, _ = t.Bot.Edit(status, fmt.Sprintf("⏹ «%s» снята — лента «%s» пуста", w.Title, w.Category))
	}
	t.refreshLibraryView(c, filter, page)
}

// refreshLibraryView re-renders the catalog message in place (best-effort)
func (t *TelegramBot) refreshLibraryView(c *tb.Callback, filter string, page int) {
	msg, markup, err := t.buildLibraryList(filter, page)
	if err != nil {
		return
	}
	_, _ = t.Bot.Edit(c.Message, msg, markup)
}

// enqueueActivation is the variant A remote path: it drops an activate/deactivate
// request in the queue for the Mac agent and pins a status message that the VM
// finalize watcher edits (via ActivationDone/Failed) once the work is uploaded
// and the feed regenerated. No upload happens in-process here.
func (t *TelegramBot) enqueueActivation(c *tb.Callback, op publisher.ActivationOp, w publisher.CatWork, filter string, page int) {
	wait := fmt.Sprintf("⏳ «%s» → в очередь на заливку (жду мак)…", w.Title)
	if op == publisher.OpDeactivate {
		wait = fmt.Sprintf("⏹ Снимаю «%s»… (жду мак)", w.Title)
	}
	status, _ := t.Bot.Send(c.Message.Chat, wait)
	req := publisher.ActivationRequest{Op: op, Category: w.Category, Work: w.Slug}
	if status != nil {
		req.ChatID = status.Chat.ID
		req.StatusMsgID = status.ID
	}
	if _, err := t.ActQueue.Enqueue(req); err != nil {
		if status != nil {
			_, _ = t.Bot.Edit(status, fmt.Sprintf("⚠️ не поставил «%s» в очередь: %v", w.Title, err))
		}
		return
	}
	t.refreshLibraryView(c, filter, page)
}

// statusMsg reconstructs the pinned status message from a request's chat+message
// ids (nil when the request carried none — e.g. a queue entry from the CLI).
func (t *TelegramBot) statusMsg(req publisher.ActivationRequest) *tb.Message {
	if req.ChatID == 0 || req.StatusMsgID == 0 {
		return nil
	}
	return &tb.Message{ID: req.StatusMsgID, Chat: &tb.Chat{ID: req.ChatID}}
}

// ActivationDone implements publisher.FinalizeNotifier: the VM finalize watcher
// calls it after a remote activation/deactivation lands, editing the status
// message the bot pinned when it enqueued the request.
func (t *TelegramBot) ActivationDone(req publisher.ActivationRequest, feedURL string, episodes int) {
	msg := t.statusMsg(req)
	if msg == nil {
		return
	}
	if req.Op == publisher.OpDeactivate {
		_, _ = t.Bot.Edit(msg, fmt.Sprintf("⏹ «%s» снята — лента «%s» пуста", req.Work, req.Category))
		return
	}
	_, _ = t.Bot.Edit(msg, fmt.Sprintf("▶ «%s» в ленте (%d частей)\nЛента: %s", req.Work, episodes, feedURL))
}

// ActivationFailed implements publisher.FinalizeNotifier for the error path.
func (t *TelegramBot) ActivationFailed(req publisher.ActivationRequest, reason string) {
	msg := t.statusMsg(req)
	if msg == nil {
		return
	}
	_, _ = t.Bot.Edit(msg, fmt.Sprintf("⚠️ «%s»: %s", req.Work, reason))
}

// parseKVCallback parses "k=v|k=v" callback data into a map
func parseKVCallback(data string) map[string]string {
	kv := map[string]string{}
	for _, part := range strings.Split(data, "|") {
		if k, v, ok := strings.Cut(part, "="); ok {
			kv[k] = v
		}
	}
	return kv
}

// atoiOr parses an int, returning def on failure
func atoiOr(s string, def int) int {
	n := def
	if s == "" {
		return def
	}
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return def
	}
	return n
}

package proc

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tb "gopkg.in/tucnak/telebot.v2"
)

// readListPageSize matches the transcript list: every item carries a row of
// action buttons, so a small page keeps the keyboard phone-sized
const readListPageSize = 5

// handleRead handles /read: with a URL it saves the article to the reading
// layer; bare /read shows the paginated list of saved articles.
func (t *TelegramBot) handleRead(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		_, _ = t.Bot.Send(m.Chat, "Unauthorized. This bot is private.")
		return
	}
	if t.ReadSvc == nil {
		_, _ = t.Bot.Send(m.Chat, "❌ Читалка не настроена (read.enabled).")
		return
	}

	rawURL := t.extractURL(m.Text)
	if rawURL == "" {
		t.handleReadList(m)
		return
	}
	if !IsArticleURL(rawURL) {
		_, _ = t.Bot.Send(m.Chat, "❌ Это не похоже на ссылку на статью.")
		return
	}

	statusMsg, _ := t.Bot.Send(m.Chat, "⏳ Добавляю в читалку...")
	go t.processRead(context.Background(), m.Chat, statusMsg, m, rawURL)
}

// processRead is the UI-bearing core: extract the article, save it, hand the
// resulting .md back to the chat as a document
func (t *TelegramBot) processRead(ctx context.Context, chat *tb.Chat, statusMsg, originalMsg *tb.Message, rawURL string) {
	if t.ReadSvc == nil {
		_, _ = t.Bot.Edit(statusMsg, "❌ Читалка не настроена.")
		return
	}
	res, err := t.ReadSvc.Save(ctx, rawURL)
	if err != nil {
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("❌ Error: %v", err))
		return
	}

	t.sendReadDocument(chat, res)

	prefix := "📖 В читалке"
	if res.Reused {
		prefix = "♻️ Уже в читалке"
	}
	if caption := readCaption(res.Meta); caption != "" {
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("%s: %s\n%s", prefix, res.Title, caption))
	} else {
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("%s: %s", prefix, res.Title))
	}
	t.deleteMessageAfterDelay(originalMsg, 5*time.Second)
}

// sendReadDocument sends the saved article's markdown to the chat with a
// human-readable filename and a short caption
func (t *TelegramBot) sendReadDocument(chat *tb.Chat, res ReadResult) {
	doc := &tb.Document{
		File:     tb.FromDisk(res.MDPath),
		FileName: sanitizeFileName(res.Title) + ".md",
		Caption:  readCaption(res.Meta),
	}
	if _, err := t.Bot.Send(chat, doc); err != nil {
		_, _ = t.Bot.Send(chat, fmt.Sprintf("⚠️ Не смог отправить файл: %v", err))
	}
}

// readCaption builds the stats line: reading time · tags
func readCaption(meta ReadMeta) string {
	var parts []string
	if meta.ReadingMin > 0 {
		parts = append(parts, fmt.Sprintf("📖 %d мин", meta.ReadingMin))
	}
	if len(meta.Tags) > 0 {
		parts = append(parts, "🏷 "+strings.Join(meta.Tags, ", "))
	}
	return strings.Join(parts, " · ")
}

// handleReadList shows the paginated reading list (bare /read)
func (t *TelegramBot) handleReadList(m *tb.Message) {
	items, err := t.ReadSvc.List()
	if err != nil {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("❌ %v", err))
		return
	}
	if len(items) == 0 {
		_, _ = t.Bot.Send(m.Chat, "В читалке пусто. Пришли ссылку и выбери 📖 В читалку.")
		return
	}
	msg, markup := t.buildReadListMessage(items, 0)
	_, _ = t.Bot.Send(m.Chat, msg, markup)
}

// buildReadListMessage renders one page with per-item buttons:
// download the .md, open the source, delete
func (t *TelegramBot) buildReadListMessage(items []ReadItem, page int) (string, *tb.ReplyMarkup) {
	total := len(items)
	pages := (total + readListPageSize - 1) / readListPageSize
	if pages == 0 {
		pages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start := page * readListPageSize
	end := start + readListPageSize
	if end > total {
		end = total
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📖 Читалка (%d) — стр %d/%d:\n\n", total, page+1, pages)
	for i := start; i < end; i++ {
		item := items[i]
		fmt.Fprintf(&b, "%d. %s\n", i+1, item.Meta.Title)
		var chips []string
		if item.Meta.DateAdded != "" {
			chips = append(chips, item.Meta.DateAdded)
		}
		if item.Meta.ReadingMin > 0 {
			chips = append(chips, fmt.Sprintf("%d мин", item.Meta.ReadingMin))
		}
		if item.Meta.Site != "" {
			chips = append(chips, item.Meta.Site)
		}
		if len(item.Meta.Tags) > 0 {
			chips = append(chips, strings.Join(item.Meta.Tags, ", "))
		}
		if len(chips) > 0 {
			fmt.Fprintf(&b, "    %s\n", strings.Join(chips, " · "))
		}
	}
	b.WriteString("\n⬇️ скачать · 🔗 источник · 🗑 удалить")

	markup := &tb.ReplyMarkup{}
	if pages > 1 {
		prev, next := page-1, page+1
		if prev < 0 {
			prev = pages - 1
		}
		if next >= pages {
			next = 0
		}
		btnPrev := markup.Data("◀︎", "rd_page", fmt.Sprintf("p=%d", prev))
		btnNext := markup.Data("▶︎", "rd_page", fmt.Sprintf("p=%d", next))
		markup.InlineKeyboard = append(markup.InlineKeyboard, []tb.InlineButton{*btnPrev.Inline(), *btnNext.Inline()})
	}
	for i := start; i < end; i++ {
		item := items[i]
		num := i + 1
		btnDL := markup.Data(fmt.Sprintf("⬇️ %d", num), "rd_act", fmt.Sprintf("a=dl|id=%s|p=%d", item.SourceID, page))
		btnSrc := markup.Data(fmt.Sprintf("🔗 %d", num), "rd_act", fmt.Sprintf("a=src|id=%s|p=%d", item.SourceID, page))
		btnDel := markup.Data(fmt.Sprintf("🗑 %d", num), "rd_act", fmt.Sprintf("a=rm|id=%s|p=%d", item.SourceID, page))
		markup.InlineKeyboard = append(markup.InlineKeyboard,
			[]tb.InlineButton{*btnDL.Inline(), *btnSrc.Inline(), *btnDel.Inline()})
	}
	return b.String(), markup
}

// handleReadListPageCallback flips reading-list pages
func (t *TelegramBot) handleReadListPageCallback(c *tb.Callback) {
	if c == nil || c.Message == nil || !t.isAuthorized(c.Sender) || t.ReadSvc == nil {
		return
	}
	page := 0
	_, _ = fmt.Sscanf(strings.TrimPrefix(c.Data, "p="), "%d", &page)
	items, err := t.ReadSvc.List()
	if err != nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Ошибка"})
		return
	}
	msg, markup := t.buildReadListMessage(items, page)
	_, _ = t.Bot.Edit(c.Message, msg, markup)
	_ = t.Bot.Respond(c)
}

// handleReadListActionCallback runs a per-item action: dl / src / rm
func (t *TelegramBot) handleReadListActionCallback(c *tb.Callback) {
	if c == nil || c.Message == nil || !t.isAuthorized(c.Sender) || t.ReadSvc == nil {
		return
	}
	action, sourceID, page := "", "", 0
	for _, part := range strings.Split(c.Data, "|") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "a":
			action = kv[1]
		case "id":
			sourceID = kv[1]
		case "p":
			_, _ = fmt.Sscanf(kv[1], "%d", &page)
		}
	}
	if sourceID == "" || strings.ContainsAny(sourceID, "/\\") || strings.Contains(sourceID, "..") {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Bad data"})
		return
	}

	path := filepath.Join(t.ReadSvc.Location, sourceID+".md")
	meta, body, err := readReadFile(path)
	if err != nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Файл не найден"})
		return
	}

	switch action {
	case "dl":
		t.sendReadDocument(c.Message.Chat, ReadResult{
			MDPath: path, Title: meta.Title, Meta: meta, WordCount: len(strings.Fields(body)),
		})
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Отправил файл"})
	case "src":
		if meta.SourceURL == "" {
			_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "У статьи нет URL источника"})
			return
		}
		_, _ = t.Bot.Send(c.Message.Chat, fmt.Sprintf("🔗 %s\n%s", meta.Title, meta.SourceURL))
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Прислал ссылку"})
	case "rm":
		if err := t.ReadSvc.Delete(sourceID); err != nil {
			_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: fmt.Sprintf("Ошибка: %v", err)})
			return
		}
		items, lerr := t.ReadSvc.List()
		if lerr != nil || len(items) == 0 {
			_, _ = t.Bot.Edit(c.Message, "📖 В читалке больше ничего нет")
		} else {
			msg, markup := t.buildReadListMessage(items, page)
			_, _ = t.Bot.Edit(c.Message, msg, markup)
		}
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Удалён"})
	default:
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Bad action"})
	}
}

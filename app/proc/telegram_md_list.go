package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tb "gopkg.in/tucnak/telebot.v2"
)

// mdListPageSize is smaller than the feed list: every item carries a row of
// three action buttons, ten rows of buttons would not fit a phone screen
const mdListPageSize = 5

// noteListItem is one L1 transcript on disk
type noteListItem struct {
	SourceID string
	Path     string
	Meta     NoteMeta
	ModTime  time.Time
}

// loadNotesList reads all L1 files' frontmatter, newest first
func (t *TelegramBot) loadNotesList() ([]noteListItem, error) {
	if t.NotesSvc == nil {
		return nil, fmt.Errorf("конспекты не настроены")
	}
	files, err := filepath.Glob(filepath.Join(t.NotesSvc.MDLocation, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("failed to list md files: %w", err)
	}

	items := make([]noteListItem, 0, len(files))
	for _, path := range files {
		fi, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		item := noteListItem{
			SourceID: strings.TrimSuffix(filepath.Base(path), ".md"),
			Path:     path,
			ModTime:  fi.ModTime(),
		}
		if meta, _, rerr := readNoteFile(path); rerr == nil {
			item.Meta = meta
		} else {
			item.Meta = NoteMeta{Title: item.SourceID} // unreadable frontmatter: still listed
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ModTime.After(items[j].ModTime) })
	return items, nil
}

// handleMDList shows the paginated transcript list (triggered by bare /md)
func (t *TelegramBot) handleMDList(m *tb.Message) {
	items, err := t.loadNotesList()
	if err != nil {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("❌ %v", err))
		return
	}
	if len(items) == 0 {
		_, _ = t.Bot.Send(m.Chat, "Пока нет ни одного транскрипта. Пришли ссылку и выбери 📄 MD-файл.")
		return
	}
	msg, markup := t.buildMDListMessage(items, 0)
	_, _ = t.Bot.Send(m.Chat, msg, markup)
}

// buildMDListMessage renders one page of the transcript list with per-item
// action buttons: download, send to Notion, delete
func (t *TelegramBot) buildMDListMessage(items []noteListItem, page int) (string, *tb.ReplyMarkup) {
	total := len(items)
	pages := (total + mdListPageSize - 1) / mdListPageSize
	if pages == 0 {
		pages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start := page * mdListPageSize
	end := start + mdListPageSize
	if end > total {
		end = total
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📄 Транскрипты (%d) — стр %d/%d:\n\n", total, page+1, pages)
	for i := start; i < end; i++ {
		item := items[i]
		fmt.Fprintf(&b, "%d. %s\n", i+1, item.Meta.Title)
		var chips []string
		if item.Meta.Date != "" {
			chips = append(chips, item.Meta.Date)
		}
		if item.Meta.DurationMin > 0 {
			chips = append(chips, fmt.Sprintf("%dм", item.Meta.DurationMin))
		}
		if item.Meta.hasProcessed("notes") {
			chips = append(chips, "📓")
		}
		if len(item.Meta.Tags) > 0 {
			chips = append(chips, strings.Join(item.Meta.Tags, ", "))
		}
		if len(chips) > 0 {
			fmt.Fprintf(&b, "    %s\n", strings.Join(chips, " · "))
		}
	}
	b.WriteString("\n⬇️ скачать · 📓 в Notion · 🗑 удалить")

	markup := &tb.ReplyMarkup{}
	if pages > 1 {
		prev, next := page-1, page+1
		if prev < 0 {
			prev = pages - 1
		}
		if next >= pages {
			next = 0
		}
		btnPrev := markup.Data("◀︎", "mdl_page", fmt.Sprintf("p=%d", prev))
		btnNext := markup.Data("▶︎", "mdl_page", fmt.Sprintf("p=%d", next))
		markup.InlineKeyboard = append(markup.InlineKeyboard, []tb.InlineButton{*btnPrev.Inline(), *btnNext.Inline()})
	}
	for i := start; i < end; i++ {
		item := items[i]
		num := i + 1
		btnDL := markup.Data(fmt.Sprintf("⬇️ %d", num), "mdl_act", fmt.Sprintf("a=dl|id=%s|p=%d", item.SourceID, page))
		btnNotion := markup.Data(fmt.Sprintf("📓 %d", num), "mdl_act", fmt.Sprintf("a=nt|id=%s|p=%d", item.SourceID, page))
		btnDel := markup.Data(fmt.Sprintf("🗑 %d", num), "mdl_act", fmt.Sprintf("a=rm|id=%s|p=%d", item.SourceID, page))
		markup.InlineKeyboard = append(markup.InlineKeyboard,
			[]tb.InlineButton{*btnDL.Inline(), *btnNotion.Inline(), *btnDel.Inline()})
	}
	return b.String(), markup
}

// handleMDListPageCallback flips list pages
func (t *TelegramBot) handleMDListPageCallback(c *tb.Callback) {
	page := 0
	_, _ = fmt.Sscanf(strings.TrimPrefix(c.Data, "p="), "%d", &page)
	items, err := t.loadNotesList()
	if err != nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Ошибка"})
		return
	}
	msg, markup := t.buildMDListMessage(items, page)
	_, _ = t.Bot.Edit(c.Message, msg, markup)
	_ = t.Bot.Respond(c)
}

// handleMDListActionCallback runs a per-item action: dl / nt (notion) / rm
func (t *TelegramBot) handleMDListActionCallback(c *tb.Callback) {
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
	if t.NotesSvc == nil || sourceID == "" || strings.Contains(sourceID, string(os.PathSeparator)) || strings.Contains(sourceID, "..") {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Bad data"})
		return
	}

	path := filepath.Join(t.NotesSvc.MDLocation, sourceID+".md")
	meta, body, err := readNoteFile(path)
	if err != nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Файл не найден"})
		return
	}

	switch action {
	case "dl":
		t.sendNoteDocument(c.Message.Chat, NotesResult{
			MDPath:    path,
			Title:     meta.Title,
			Meta:      meta,
			WordCount: len(strings.Fields(body)),
		})
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Отправил файл"})
	case "nt":
		if meta.URL == "" {
			_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "У файла нет URL источника"})
			return
		}
		statusMsg, _ := t.Bot.Send(c.Message.Chat, "⏳ В очереди...")
		t.enqueueNotesJob(statusMsg, nil, meta.URL, "notes")
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Поставил в очередь"})
	case "rm":
		if err := os.Remove(path); err != nil {
			_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: fmt.Sprintf("Ошибка: %v", err)})
			return
		}
		items, lerr := t.loadNotesList()
		if lerr != nil || len(items) == 0 {
			_, _ = t.Bot.Edit(c.Message, "📄 Транскриптов больше нет")
		} else {
			msg, markup := t.buildMDListMessage(items, page)
			_, _ = t.Bot.Edit(c.Message, msg, markup)
		}
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Удалён"})
	default:
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Bad action"})
	}
}

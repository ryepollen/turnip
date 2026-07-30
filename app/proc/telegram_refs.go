package proc

import (
	"fmt"
	"sort"
	"strings"

	tb "gopkg.in/tucnak/telebot.v2"
)

// refsPageSize is how many named references fit on one /refs page. References
// carry no per-item action row (this is a read view feeding the future Mini
// App), so the page can be a bit larger than the transcript/reading lists.
const refsPageSize = 8

// handleRefs handles /refs: bare shows the reference-type overview with counts
// and a button per type; /refs <тип> opens the paginated list for that type.
func (t *TelegramBot) handleRefs(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		return
	}
	if t.Store == nil {
		_, _ = t.Bot.Send(m.Chat, "❌ Хранилище недоступно.")
		return
	}

	args := strings.SplitN(strings.TrimSpace(m.Text), " ", 2)
	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		msg, markup := t.buildRefsOverview()
		_, _ = t.Bot.Send(m.Chat, msg, markup)
		return
	}

	typ := normalizeRefType(strings.TrimSpace(args[1]))
	msg, markup := t.buildRefsListMessage(typ, 0)
	_, _ = t.Bot.Send(m.Chat, msg, markup)
}

// buildRefsOverview renders the type counts as a text summary plus one button
// per non-empty type that opens its list.
func (t *TelegramBot) buildRefsOverview() (string, *tb.ReplyMarkup) {
	counts, total, err := t.Store.ReferenceTypeCounts()
	if err != nil {
		return fmt.Sprintf("❌ %v", err), &tb.ReplyMarkup{}
	}
	if total == 0 {
		return "Отсылок пока нет. Сделай /notes по видео — бот вытащит книги, людей, инструменты.", &tb.ReplyMarkup{}
	}

	// order types by count desc, then by the canonical referenceTypes order
	order := map[string]int{}
	for i, rt := range referenceTypes {
		order[rt] = i
	}
	types := make([]string, 0, len(counts))
	for typ := range counts {
		types = append(types, typ)
	}
	sort.Slice(types, func(i, j int) bool {
		if counts[types[i]] != counts[types[j]] {
			return counts[types[i]] > counts[types[j]]
		}
		return order[types[i]] < order[types[j]]
	})

	var b strings.Builder
	fmt.Fprintf(&b, "📎 Отсылки из всех эпизодов (%d):\n\n", total)
	for _, typ := range types {
		fmt.Fprintf(&b, "%s %s — %d\n", refTypeEmoji[typ], typ, counts[typ])
	}
	b.WriteString("\nВыбери тип кнопкой или: /refs <тип>")

	markup := &tb.ReplyMarkup{}
	var row []tb.InlineButton
	for _, typ := range types {
		btn := markup.Data(fmt.Sprintf("%s %s", refTypeEmoji[typ], typ), "refs_pg", fmt.Sprintf("t=%s|p=0", typ))
		row = append(row, *btn.Inline())
		if len(row) == 2 {
			markup.InlineKeyboard = append(markup.InlineKeyboard, row)
			row = nil
		}
	}
	if len(row) > 0 {
		markup.InlineKeyboard = append(markup.InlineKeyboard, row)
	}
	return b.String(), markup
}

// refsGroup is a single named reference aggregated across episodes
type refsGroup struct {
	name     string
	episodes []refsEpisode
}

type refsEpisode struct {
	title    string
	timecode string
	url      string // Notion page if present, else source URL
}

// buildRefsListMessage renders one page of references for a type, grouping
// entries by name so a book cited in three episodes shows once with all three.
func (t *TelegramBot) buildRefsListMessage(typ string, page int) (string, *tb.ReplyMarkup) {
	entries, err := t.Store.ListReferences(typ)
	if err != nil {
		return fmt.Sprintf("❌ %v", err), &tb.ReplyMarkup{}
	}
	if len(entries) == 0 {
		msg, markup := t.buildRefsOverview()
		return "По типу «" + typ + "» пусто.\n\n" + msg, markup
	}

	// group by lowercased name, preserve first-seen display form
	idx := map[string]int{}
	var groups []refsGroup
	for _, e := range entries {
		key := strings.ToLower(strings.TrimSpace(e.Name))
		if key == "" {
			continue
		}
		g, ok := idx[key]
		if !ok {
			idx[key] = len(groups)
			g = len(groups)
			groups = append(groups, refsGroup{name: e.Name})
		}
		url := e.NotionURL
		if url == "" {
			url = e.EpisodeURL
		}
		groups[g].episodes = append(groups[g].episodes, refsEpisode{
			title: e.EpisodeTitle, timecode: e.Timecode, url: url,
		})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if len(groups[i].episodes) != len(groups[j].episodes) {
			return len(groups[i].episodes) > len(groups[j].episodes)
		}
		return strings.ToLower(groups[i].name) < strings.ToLower(groups[j].name)
	})

	total := len(groups)
	pages := (total + refsPageSize - 1) / refsPageSize
	if pages == 0 {
		pages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start := page * refsPageSize
	end := start + refsPageSize
	if end > total {
		end = total
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s (%d) — стр %d/%d:\n\n", refTypeEmoji[typ], typ, total, page+1, pages)
	for i := start; i < end; i++ {
		g := groups[i]
		fmt.Fprintf(&b, "%d. «%s» — %d×\n", i+1, g.name, len(g.episodes))
		for _, ep := range g.episodes {
			line := "    • " + refsEpisodeLabel(ep.title)
			if ep.timecode != "" {
				line += " [" + ep.timecode + "]"
			}
			if ep.url != "" {
				line += "\n      " + ep.url
			}
			b.WriteString(line + "\n")
		}
	}

	markup := &tb.ReplyMarkup{}
	nav := []tb.InlineButton{}
	if pages > 1 {
		prev, next := page-1, page+1
		if prev < 0 {
			prev = pages - 1
		}
		if next >= pages {
			next = 0
		}
		btnPrev := markup.Data("◀︎", "refs_pg", fmt.Sprintf("t=%s|p=%d", typ, prev))
		btnNext := markup.Data("▶︎", "refs_pg", fmt.Sprintf("t=%s|p=%d", typ, next))
		nav = append(nav, *btnPrev.Inline(), *btnNext.Inline())
	}
	btnBack := markup.Data("◀︎ типы", "refs_pg", "t=|p=0")
	nav = append(nav, *btnBack.Inline())
	markup.InlineKeyboard = append(markup.InlineKeyboard, nav)
	return b.String(), markup
}

// refsEpisodeLabel trims an episode title so the reference list stays readable
func refsEpisodeLabel(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return "эпизод"
	}
	const max = 60
	r := []rune(title)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return title
}

// handleRefsPageCallback handles both type buttons and page flips; an empty
// type (t=) means the back-to-overview button.
func (t *TelegramBot) handleRefsPageCallback(c *tb.Callback) {
	if c == nil || c.Message == nil || !t.isAuthorized(c.Sender) || t.Store == nil {
		return
	}
	typ, page := "", 0
	for _, part := range strings.Split(c.Data, "|") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			typ = kv[1]
		case "p":
			_, _ = fmt.Sscanf(kv[1], "%d", &page)
		}
	}

	var msg string
	var markup *tb.ReplyMarkup
	if typ == "" {
		msg, markup = t.buildRefsOverview()
	} else {
		msg, markup = t.buildRefsListMessage(normalizeRefType(typ), page)
	}
	_, _ = t.Bot.Edit(c.Message, msg, markup)
	_ = t.Bot.Respond(c)
}

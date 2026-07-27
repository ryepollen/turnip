package proc

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseTriageData(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		wantToken string
		wantPage  int
	}{
		{"token and page", "t=abc123|p=4", "abc123", 4},
		{"page defaults to zero", "t=abc123", "abc123", 0},
		{"order independent", "p=2|t=zz", "zz", 2},
		{"junk parts ignored", "x|t=q|=|p=1", "q", 1},
		{"empty", "", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok, page := parseTriageData(tt.data)
			assert.Equal(t, tt.wantToken, tok)
			assert.Equal(t, tt.wantPage, page)
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short unchanged", "hello", 10, "hello"},
		{"exact length unchanged", "hello", 5, "hello"},
		{"cut with ellipsis", "hello world", 8, "hello w…"},
		{"multibyte safe", "конспект длинный", 6, "консп…"},
		{"max one", "hello", 1, "h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, truncateRunes(tt.in, tt.max))
		})
	}
}

func TestTriageItemTitle(t *testing.T) {
	rec := pendingTriage{
		videoIDs: []string{"aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc"},
		titles:   []string{"First", ""},
	}
	assert.Equal(t, "First", triageItemTitle(rec, 0))
	assert.Equal(t, "bbbbbbbbbbb", triageItemTitle(rec, 1), "empty title falls back to id")
	assert.Equal(t, "ccccccccccc", triageItemTitle(rec, 2), "missing title index falls back to id")
}

// buildTriageMessage needs t.NotesSvc only to decide whether md/notes buttons
// show; the header/paging math is exercised with a bare bot.
func TestBuildTriageMessage_Paging(t *testing.T) {
	bot := &TelegramBot{} // NotesSvc nil → audio-only rows

	ids := make([]string, 12)
	for i := range ids {
		ids[i] = "id000000" + string(rune('a'+i)) + "00" // 11 chars, unique
	}
	rec := pendingTriage{token: "tok", videoIDs: ids, done: map[string]bool{ids[0]: true}}

	msg, markup := bot.buildTriageMessage(rec, 0)
	assert.Contains(t, msg, "12 видео")
	assert.Contains(t, msg, "стр 1/3", "12 items / 5 per page = 3 pages")
	assert.Contains(t, msg, "1. "+ids[0]+" ✓", "queued item marked")
	assert.NotContains(t, msg, "6. ", "page 1 shows only 5 items")

	// with NotesSvc nil there is one audio button per item (5) + page row + nav
	itemRows := 0
	for _, row := range markup.InlineKeyboard {
		if len(row) == 1 && strings.HasPrefix(row[0].Text, "🎵 ") && row[0].Text != "🎵 стр" {
			itemRows++
		}
	}
	assert.Equal(t, 5, itemRows, "audio-only: one 🎵 button per item on the page")

	// last page clamps and shows the tail
	msg, _ = bot.buildTriageMessage(rec, 5) // out of range → clamped to last
	assert.Contains(t, msg, "стр 3/3")
	assert.Contains(t, msg, "11. ")
	assert.Contains(t, msg, "12. ")
}

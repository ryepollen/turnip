package proc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadNotesListAndBuildMessage(t *testing.T) {
	dir := t.TempDir()
	bot := &TelegramBot{NotesSvc: NewNotesService(NotesParams{MDLocation: dir})}

	// empty dir
	items, err := bot.loadNotesList()
	require.NoError(t, err)
	assert.Empty(t, items)

	for i, meta := range []NoteMeta{
		{Title: "Первый", Source: "youtube", URL: "https://youtu.be/a1", Date: "2026-06-01", DurationMin: 10, Tags: []string{"one"}, Processed: []string{"md"}},
		{Title: "Второй", Source: "podcast", URL: "https://podcasts.apple.com/podcast/id9?i=2", Date: "2026-06-02", DurationMin: 20, Processed: []string{"md", "notes"}},
		{Title: "Третий", Source: "article", URL: "https://example.com/3", Date: "2026-06-03", DurationMin: 0, Processed: []string{"md"}},
	} {
		path := filepath.Join(dir, []string{"a1", "ap_2", "art3"}[i]+".md")
		require.NoError(t, writeNoteFile(path, meta, "тело"))
	}
	// a file with broken frontmatter is still listed by its id
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.md"), []byte("no frontmatter here"), 0o600))

	items, err = bot.loadNotesList()
	require.NoError(t, err)
	require.Len(t, items, 4)

	msg, markup := bot.buildMDListMessage(items, 0)
	assert.Contains(t, msg, "Транскрипты (4)")
	assert.Contains(t, msg, "Первый")
	assert.Contains(t, msg, "📓", "notes marker for Второй")
	require.NotNil(t, markup)
	assert.Len(t, markup.InlineKeyboard, 4, "one action row per item, no nav for single page")
	assert.Len(t, markup.InlineKeyboard[0], 3, "dl/notion/rm buttons")

	// pagination appears when items exceed a page
	for i := 0; i < mdListPageSize; i++ {
		require.NoError(t, writeNoteFile(filepath.Join(dir, "x"+string(rune('a'+i))+".md"), NoteMeta{Title: "x"}, "b"))
	}
	items, err = bot.loadNotesList()
	require.NoError(t, err)
	_, markup = bot.buildMDListMessage(items, 0)
	assert.Len(t, markup.InlineKeyboard, mdListPageSize+1, "nav row + item rows")
	nav := markup.InlineKeyboard[0]
	assert.Equal(t, "◀︎", nav[0].Text)
}

package proc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/umputun/feed-master/app/publisher"
)

func TestLibraryWorkMeta(t *testing.T) {
	shelf := publisher.CatWork{Category: "books", Parts: 13, Finite: true}
	assert.Equal(t, "books · 13 parts · on the shelf", libraryWorkMeta(shelf, 0, true))
	assert.Equal(t, "books · 13 parts · ⚠ 13 missing", libraryWorkMeta(shelf, 13, true))
	assert.Equal(t, "books · 13 parts", libraryWorkMeta(shelf, 0, false), "a failed R2 check omits the shelf badge")

	air := shelf
	air.Active = true
	air.Parts = 1
	air.Seasons = 2
	air.Finite = false
	assert.Equal(t, "books · 1 part · 2 seasons · streaming · ▶ on air", libraryWorkMeta(air, 0, true))
}

func TestLibraryActionLabel(t *testing.T) {
	work := publisher.CatWork{Parts: 13, Finite: true}

	_, _, show := libraryActionLabel(work, 1, 13, true)
	assert.False(t, show, "missing parts have no On air button")

	label, action, show := libraryActionLabel(work, 2, 0, true)
	assert.True(t, show)
	assert.Equal(t, "▶ On air 2", label)
	assert.Equal(t, "on", action)

	label, action, show = libraryActionLabel(work, 2, 0, false)
	assert.True(t, show, "an unknown R2 check still offers On air")
	assert.Equal(t, "on", action)

	work.Active = true
	label, action, show = libraryActionLabel(work, 4, 0, true)
	assert.True(t, show)
	assert.Equal(t, "⏹ Stop 4", label)
	assert.Equal(t, "off", action)
}

func TestMissingOnPageNilPublisher(t *testing.T) {
	var bot TelegramBot
	got := bot.missingOnPage([]publisher.CatWork{{Category: "books", Slug: "x"}})
	assert.Empty(t, got)
}

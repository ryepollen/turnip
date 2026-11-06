package proc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestArticleIDIgnoresTracking(t *testing.T) {
	plain := "https://example.com/post"
	tagged := plain + "?utm_source=tg&utm_medium=social"

	assert.Equal(t, readSourceID(plain), readSourceID(CleanTrackingParams(tagged)))

	idTagged, srcTagged := noteSourceID(CleanTrackingParams(tagged))
	idPlain, srcPlain := noteSourceID(plain)
	assert.Equal(t, idPlain, idTagged)
	assert.Equal(t, "article", srcTagged)
	assert.Equal(t, srcPlain, srcTagged)
}

func TestExtractURLStripsTracking(t *testing.T) {
	var bot TelegramBot
	got := bot.extractURL("see https://example.com/post?utm_source=tg&id=3 please")
	assert.Equal(t, "https://example.com/post?id=3", got)

	yt := bot.extractURL("https://youtu.be/dQw4w9WgXcQ?si=TRACK&t=12")
	assert.Equal(t, "https://youtu.be/dQw4w9WgXcQ?t=12", yt)
	assert.Equal(t, "dQw4w9WgXcQ", extractVideoID(yt))
}

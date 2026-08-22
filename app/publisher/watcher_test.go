package publisher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefreshCatalogAnnouncesNewWorks(t *testing.T) {
	svc, _ := newFakeR2Service(t)

	// a work already present before the first scan
	writeWork(t, svc, "books", "Existing", "01 - A.mp3")

	var notices []string
	notify := func(s string) { notices = append(notices, s) }
	known := map[string]bool{}

	// seed scan (as Watch does at startup) is silent: don't announce the whole library
	svc.refreshCatalog(known, nil)
	assert.Empty(t, notices)

	// nothing new → nothing announced
	svc.refreshCatalog(known, notify)
	assert.Empty(t, notices, "no new work, no message")

	// a new work appears
	writeWork(t, svc, "podcasts", "Fresh", "01 - E1.mp3", "02 - E2.mp3")
	svc.refreshCatalog(known, notify)
	require.Len(t, notices, 1, "exactly one announcement for the new work")
	assert.Contains(t, notices[0], "Fresh")
	assert.Contains(t, notices[0], "/library")

	// already announced → not repeated
	svc.refreshCatalog(known, notify)
	assert.Len(t, notices, 1, "no repeat announcement")
}

func TestRefreshCatalogSeasonsInNotice(t *testing.T) {
	svc, _ := newFakeR2Service(t)
	known := map[string]bool{}
	svc.refreshCatalog(known, nil) // seed empty

	writeWork(t, svc, "podcasts", "Seasoned",
		"Season 01/01 - A.mp3", "Season 02/01 - B.mp3")

	var notices []string
	svc.refreshCatalog(known, func(s string) { notices = append(notices, s) })
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], "сезон")
}

func TestRefreshCatalogNoOriginalsDir(t *testing.T) {
	svc, _ := newFakeR2Service(t)
	// AudioDir exists but no originals/ subtree — must not panic or announce
	require.NoError(t, os.RemoveAll(filepath.Join(svc.AudioDir, "originals")))
	known := map[string]bool{}
	var notices []string
	svc.refreshCatalog(known, func(s string) { notices = append(notices, s) })
	assert.Empty(t, notices)
}

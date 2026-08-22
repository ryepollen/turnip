package publisher

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"
)

// watcher notes: a polling scanner instead of fsnotify — inotify is flaky on
// VMs and mounted volumes, and a minute of latency is irrelevant for a library.
// It no longer publishes: a work goes live only when Activated from the bot /
// Mini App. The watcher just announces works that newly appear in the tree so
// the owner knows something is ready to activate.

// Watch scans the library on an interval and announces newly-added works.
// notify (nil-safe) receives human-readable messages for Telegram.
func (p *Service) Watch(ctx context.Context, interval time.Duration, notify func(string)) {
	if interval <= 0 {
		interval = time.Minute
	}
	log.Printf("[INFO] library watcher started: %s every %s", filepath.Join(p.AudioDir, "originals"), interval)
	known := map[string]bool{}
	p.refreshCatalog(known, nil) // seed silently: don't announce the whole existing library at startup
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refreshCatalog(known, notify)
		}
	}
}

// refreshCatalog diffs the current library against the known set and announces
// works that appeared since the last scan (nothing is published — activation is
// a deliberate choice in the bot).
func (p *Service) refreshCatalog(known map[string]bool, notify func(string)) {
	works, err := p.Catalog()
	if err != nil {
		log.Printf("[WARN] library scan failed: %v", err)
		return
	}
	for _, w := range works {
		id := w.Category + "/" + w.Slug
		if known[id] {
			continue
		}
		known[id] = true
		if notify == nil {
			continue
		}
		parts := fmt.Sprintf("%d частей", w.Parts)
		if w.Seasons > 0 {
			parts = fmt.Sprintf("%d частей · %d сезон(ов)", w.Parts, w.Seasons)
		}
		notify(fmt.Sprintf("📚 новая работа в библиотеке: «%s» (%s, %s)\nАктивировать: /library", w.Title, w.Category, parts))
	}
}

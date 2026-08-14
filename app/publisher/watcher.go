package publisher

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// watcher notes: a polling scanner instead of fsnotify — inotify is flaky on
// VMs and mounted volumes, and a minute of latency is irrelevant for books.
// A file is picked up only after its size+mtime stay unchanged for two
// consecutive scans, so half-copied uploads are never published.

// seenFile tracks stability of a candidate between scans
type seenFile struct {
	size  int64
	mtime time.Time
	scans int
}

// failureCooldown is how long a failed file is left alone before a retry —
// otherwise a permission problem would spam Telegram every other scan
const failureCooldown = 30 * time.Minute

// flushAfterIdleScans is how many consecutive scans a category must publish
// nothing new before its "📚 +N эпизодов" summary is sent. Dropping a 51-part
// book would otherwise fire 51 notifications; instead we coalesce the whole
// burst into one "можно слушать" message once the category goes quiet.
const flushAfterIdleScans = 2

// catBurst accumulates one category's freshly published episodes until the
// burst ends, so the owner gets a single summary instead of per-file spam.
type catBurst struct {
	added int // episodes published since this burst started
	idle  int // consecutive scans with nothing new for this category
}

// Watch scans originals/ on an interval and publishes new stable files.
// notify (nil-safe) receives human-readable progress messages for Telegram.
func (p *Service) Watch(ctx context.Context, interval time.Duration, notify func(string)) {
	if interval <= 0 {
		interval = time.Minute
	}
	log.Printf("[INFO] audio watcher started: %s every %s", filepath.Join(p.AudioDir, "originals"), interval)
	seen := map[string]*seenFile{}
	cooldown := map[string]time.Time{}
	bursts := map[string]*catBurst{}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			added := p.scanOnce(ctx, seen, cooldown, notify)
			p.flushBursts(bursts, added, notify)
		}
	}
}

// flushBursts folds this scan's per-category publish counts into ongoing
// bursts and emits one summary per category once it has been quiet for
// flushAfterIdleScans scans (the burst has clearly ended).
func (p *Service) flushBursts(bursts map[string]*catBurst, added map[string]int, notify func(string)) {
	for cat, n := range added {
		b := bursts[cat]
		if b == nil {
			b = &catBurst{}
			bursts[cat] = b
		}
		b.added += n
		b.idle = 0
	}
	for cat, b := range bursts {
		if added[cat] > 0 {
			continue // still growing — hold the summary
		}
		if b.idle++; b.idle < flushAfterIdleScans {
			continue
		}
		if notify != nil && b.added > 0 {
			total := p.episodeCount(cat)
			if b.added == 1 {
				notify(fmt.Sprintf("📚 «%s»: +1 эпизод (всего %d)\nЛента: %s", cat, total, p.FeedURL(cat)))
			} else {
				notify(fmt.Sprintf("📚 «%s»: +%d эпизодов — можно слушать (всего %d)\nЛента: %s", cat, b.added, total, p.FeedURL(cat)))
			}
		}
		delete(bursts, cat)
	}
}

// episodeCount returns how many episodes a category currently has published
func (p *Service) episodeCount(category string) int {
	eps, err := p.loadEpisodes(category)
	if err != nil {
		return 0
	}
	return len(eps)
}

// scanOnce walks all category dirs and publishes files that became stable.
// It returns how many episodes each category published this scan, so the
// caller can coalesce a burst into a single summary notification.
func (p *Service) scanOnce(ctx context.Context, seen map[string]*seenFile, cooldown map[string]time.Time, notify func(string)) map[string]int {
	added := map[string]int{}
	root := filepath.Join(p.AudioDir, "originals")
	categories, err := os.ReadDir(root)
	if err != nil {
		return added // no originals dir yet — nothing to do
	}

	for _, catDir := range categories {
		if !catDir.IsDir() {
			continue
		}
		category := catDir.Name()
		if validCategory(category) != nil {
			continue
		}

		published, err := p.publishedSet(category)
		if err != nil {
			log.Printf("[WARN] watcher can't read state for %s: %v", category, err)
			continue
		}

		files, _ := os.ReadDir(filepath.Join(root, category))
		for _, f := range files {
			if f.IsDir() || mimeForFile(f.Name()) == "" || published[f.Name()] {
				continue
			}
			path := filepath.Join(root, category, f.Name())
			fi, statErr := os.Stat(path)
			if statErr != nil {
				continue
			}

			s := seen[path]
			if s == nil {
				seen[path] = &seenFile{size: fi.Size(), mtime: fi.ModTime(), scans: 1}
				continue // first sight — wait for stability
			}
			if s.size != fi.Size() || !s.mtime.Equal(fi.ModTime()) {
				*s = seenFile{size: fi.Size(), mtime: fi.ModTime(), scans: 1}
				delete(cooldown, path) // genuinely changed — a failed file gets a fresh chance
				continue               // still changing — wait for stability
			}
			if until, coolingDown := cooldown[path]; coolingDown {
				if time.Now().Before(until) {
					continue // recently failed, leave it alone for a while
				}
				delete(cooldown, path)
			}
			if s.scans++; s.scans < 2 {
				continue
			}

			if err := p.processAndPublish(ctx, path, category, notify); err != nil {
				cooldown[path] = time.Now().Add(failureCooldown)
				continue // keep the seen entry: the file must not look "new" next scan
			}
			added[category]++
			delete(seen, path)
		}
	}
	return added
}

// processAndPublish runs the pipeline for one stable file: loudnorm (when
// enabled and mp3) then publish. A returned error puts the file on cooldown.
func (p *Service) processAndPublish(ctx context.Context, path, category string, notify func(string)) error {
	base := filepath.Base(path)
	say := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		log.Printf("[INFO] watcher: %s", msg)
		if notify != nil {
			notify(msg)
		}
	}

	srcPath, err := p.prepare(ctx, path, category)
	if err != nil {
		say("❌ %s/%s: обработка не удалась (повтор через %s): %v", category, base, failureCooldown, err)
		return err
	}

	ep, err := p.PublishFile(ctx, srcPath, category)
	if err != nil {
		say("❌ %s/%s: публикация не удалась (повтор через %s): %v", category, base, failureCooldown, err)
		return err
	}
	// Per-file success is logged only; the owner-facing "📚 +N эпизодов"
	// summary is emitted once per burst by flushBursts (no 51-message spam).
	log.Printf("[INFO] watcher: published %s → «%s»", category, ep.Title)
	return nil
}

// publishedSet returns the basenames already in the category registry
func (p *Service) publishedSet(category string) (map[string]bool, error) {
	eps, err := p.loadEpisodes(category)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(eps))
	for _, ep := range eps {
		set[ep.File] = true
	}
	return set, nil
}

// prepare normalizes loudness into processed/{category}/ when enabled.
// Only mp3 is re-encoded (books and courses vary wildly in loudness — EBU
// R128 to -16 LUFS is the single biggest UX win); other formats pass through.
func (p *Service) prepare(ctx context.Context, path, category string) (string, error) {
	cfg := LoadFeedConfig(p.categoryDir(category), category)
	if !cfg.NormalizeEnabled() || !strings.EqualFold(filepath.Ext(path), ".mp3") {
		return path, nil
	}

	outDir := filepath.Join(p.AudioDir, "processed", category)
	out := filepath.Join(outDir, filepath.Base(path))
	if fi, err := os.Stat(out); err == nil && fi.Size() > 0 {
		return out, nil // already processed on a previous run
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return "", fmt.Errorf("failed to create processed dir: %w", err)
	}

	ffCtx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()
	tmp := out + ".part.mp3" // ffmpeg needs a recognizable extension
	cmd := exec.CommandContext(ffCtx, "ffmpeg", "-nostdin", "-y", "-i", path,
		"-af", "loudnorm=I=-16:TP=-1.5:LRA=11",
		"-ar", "44100", "-b:a", "128k", tmp)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	log.Printf("[INFO] normalizing loudness: %s", filepath.Base(path))
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("ffmpeg loudnorm failed: %w, stderr tail: %s", err, tailOf(stderr.String(), 300))
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", fmt.Errorf("failed to finalize processed file: %w", err)
	}
	return out, nil
}

// tailOf returns up to n last chars of s
func tailOf(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

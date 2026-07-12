package proc

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	ytfeed "github.com/umputun/feed-master/app/youtube/feed"
)

// MediaOffloader moves feed episode files to R2 so the VM serves redirects
// instead of streaming gigabytes through GCP egress (implemented by
// publisher.FeedMedia). R2 is a transit buffer: deleting an episode deletes
// the object, everything is re-fetchable via the history log.
type MediaOffloader interface {
	UploadMedia(ctx context.Context, localPath, basename string) (string, error)
	DeleteMedia(ctx context.Context, basename string) error
	TotalSize(ctx context.Context) (int64, error)
}

// r2WarnThreshold is where the bot starts nagging: 80% of the 10GB free tier
const r2WarnThreshold = 8 << 30

// r2WarnEvery limits the nagging frequency
const r2WarnEvery = 24 * time.Hour

// offloadMedia uploads a saved episode to R2 in the background and removes
// the local copy on success. On failure the file stays and /yt/media serves
// it from disk — nothing breaks, only egress money leaks.
func (t *TelegramBot) offloadMedia(entry ytfeed.Entry) {
	if t.Media == nil || entry.File == "" {
		return
	}
	file := entry.File
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
		defer cancel()
		if t.offloadMediaSync(ctx, file) {
			t.checkR2Usage(ctx)
		}
	}()
}

// offloadMediaSync is the testable core: returns true when the file made it
// to R2 (regardless of whether the local removal succeeded)
func (t *TelegramBot) offloadMediaSync(ctx context.Context, file string) bool {
	base := filepath.Base(file)
	if _, err := t.Media.UploadMedia(ctx, file, base); err != nil {
		log.Printf("[WARN] failed to offload %s to R2, serving locally: %v", base, err)
		return false
	}
	if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
		log.Printf("[WARN] offloaded %s but failed to remove local copy: %v", base, err)
	} else {
		log.Printf("[INFO] offloaded %s to R2, local copy removed", base)
	}
	return true
}

// deleteMediaObject removes the R2 object of a deleted episode (best-effort)
func (t *TelegramBot) deleteMediaObject(file string) {
	if t.Media == nil || file == "" {
		return
	}
	base := filepath.Base(file)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := t.Media.DeleteMedia(ctx, base); err != nil {
			log.Printf("[WARN] failed to delete %s from R2: %v", base, err)
		}
	}()
}

// checkR2Usage warns the owner (at most once a day) when the bucket
// approaches the free tier, suggesting what to clean
func (t *TelegramBot) checkR2Usage(ctx context.Context) {
	total, err := t.Media.TotalSize(ctx)
	if err != nil {
		log.Printf("[WARN] failed to check R2 usage: %v", err)
		return
	}
	if total < r2WarnThreshold {
		return
	}

	t.r2WarnMu.Lock()
	recentlyWarned := time.Since(t.lastR2Warn) < r2WarnEvery
	if !recentlyWarned {
		t.lastR2Warn = time.Now()
	}
	t.r2WarnMu.Unlock()
	if recentlyWarned {
		return
	}

	t.NotifyOwner(fmt.Sprintf(
		"⚠️ R2 занято %.1f GB из 10 бесплатных.\nПрослушанное можно удалить: /list → 🗑 — файл в R2 удалится вместе с записью, ссылка останется в /history.",
		float64(total)/(1<<30)))
}

// r2UsageLine renders the /status line for bucket usage ("" when disabled)
func (t *TelegramBot) r2UsageLine() string {
	if t.Media == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	total, err := t.Media.TotalSize(ctx)
	if err != nil {
		return "☁️ R2: недоступно"
	}
	return fmt.Sprintf("☁️ R2: %.1f GB / 10 GB", float64(total)/(1<<30))
}

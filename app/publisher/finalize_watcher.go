package publisher

import (
	"context"
	"fmt"
	"log"
	"time"
)

// FinalizeNotifier delivers activation outcomes back to the owner's Telegram
// status message. The bot implements it; it is nil-safe so the watcher runs
// headless in tests and before the bot is wired at cutover.
type FinalizeNotifier interface {
	ActivationDone(req ActivationRequest, feedURL string, episodes int)
	ActivationFailed(req ActivationRequest, reason string)
}

// finalizeTimeout bounds one finalize (R2 HEADs of every part + eviction of the
// previous work); uploads are already done, so this is only metadata + deletes.
const finalizeTimeout = 15 * time.Minute

// WatchDone is the VM half of variant A activation. The Mac agent uploads a
// work's parts to R2 and marks the request done; this watcher polls the done
// stage and applies the state+feed change locally — rebuilding the episode list
// from the parts' R2 sizes, swapping the category's active work, evicting the
// previous one. Keeping the state write on the VM sidesteps the uid trap (the
// server only reads files owned by uid 1001). notify (nil-safe) reports each
// outcome; the queue is empty until the bot starts enqueueing at cutover, so the
// watcher is inert when wired ahead of that.
func (p *Service) WatchDone(ctx context.Context, q *ActivationQueue, interval time.Duration, notify FinalizeNotifier) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	log.Printf("[INFO] activation finalize watcher started: %s every %s", q.dir, interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.drainDone(ctx, q, notify)
		}
	}
}

// drainDone finalizes every request currently in the done stage, oldest first.
func (p *Service) drainDone(ctx context.Context, q *ActivationQueue, notify FinalizeNotifier) {
	done, err := q.ListDone()
	if err != nil {
		log.Printf("[WARN] finalize: cannot list done requests: %v", err)
		return
	}
	for _, req := range done {
		p.finalizeOne(ctx, q, req, notify)
	}
}

// finalizeOne applies one done request: Finalize for an activate, Deactivate for
// a deactivate. Success → archive + notify; failure → move back to failed +
// notify. Both Finalize and Deactivate are idempotent, so a crash between apply
// and archive re-runs harmlessly on the next tick.
func (p *Service) finalizeOne(ctx context.Context, q *ActivationQueue, req ActivationRequest, notify FinalizeNotifier) {
	opCtx, cancel := context.WithTimeout(ctx, finalizeTimeout)
	defer cancel()

	var err error
	switch req.Op {
	case OpActivate:
		err = p.Finalize(opCtx, req.Category, req.Work)
	case OpDeactivate:
		err = p.Deactivate(opCtx, req.Category)
	default:
		err = fmt.Errorf("unknown op %q", req.Op)
	}

	if err != nil {
		log.Printf("[WARN] finalize %s %s/%s failed: %v", req.Op, req.Category, req.Work, err)
		if rerr := q.Reject(req.ID, err.Error()); rerr != nil {
			log.Printf("[WARN] finalize: cannot reject %s: %v", req.ID, rerr)
			return // leave it in done to retry next tick rather than lose it
		}
		if notify != nil {
			notify.ActivationFailed(req, err.Error())
		}
		return
	}

	if aerr := q.Archive(req.ID); aerr != nil {
		// state is already applied; a stuck done entry just re-runs the
		// idempotent finalize next tick, so tolerate and don't notify twice
		log.Printf("[WARN] finalize: cannot archive %s: %v", req.ID, aerr)
		return
	}
	if notify != nil {
		eps, _ := p.EpisodeList(req.Category)
		notify.ActivationDone(req, p.FeedURL(req.Category), len(eps))
	}
}

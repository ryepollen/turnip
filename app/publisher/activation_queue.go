package publisher

import (
	"crypto/sha1" //nolint:gosec // id hashing only, not security
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ActivationOp is what the Mac agent should do with a requested work.
type ActivationOp string

const (
	// OpActivate uploads a work's audio to R2 and makes it the live one.
	OpActivate ActivationOp = "activate"
	// OpDeactivate empties a category (evict R2, dormant feed); Work may be blank.
	OpDeactivate ActivationOp = "deactivate"
)

// activation queue stages — each is a sibling directory under requests/
const (
	reqPending = "pending"
	reqClaimed = "claimed"
	reqDone    = "done"
	reqFailed  = "failed"
	reqArchive = "archive"
)

// ActivationRequest is the trigger handed VM→Mac. In variant A the audio archive
// lives in iCloud (reachable only from the Mac), while the bot runs on the VM; so
// when the owner taps ▶ Слушать / ⏹ Снять the bot drops a request here and the
// Mac agent, which can read iCloud and write R2, fulfils it. The payload stays
// deliberately minimal: the VM re-derives the episode list from local part names
// + deterministic R2 keys, so the channel never carries audio metadata — only
// which work to make live and how to reach the owner's status message.
type ActivationRequest struct {
	ID          string       `json:"id"`
	Op          ActivationOp `json:"op"`
	Category    string       `json:"category"`
	Work        string       `json:"work"` // work slug; blank for a whole-category deactivate
	ChatID      int64        `json:"chat_id,omitempty"`
	StatusMsgID int          `json:"status_msg_id,omitempty"`
	RequestedAt time.Time    `json:"requested_at"`
}

// ActivationQueue is a filesystem handoff both machines poll. The bot (VM) and
// the finalize watcher (VM) drive it in Go; the Mac agent drives the same on-disk
// JSON over SSH. Every stage transition is an atomic rename between sibling dirs,
// so a double claim or a crash mid-flight can never run an activation twice.
type ActivationQueue struct {
	dir string
}

// NewActivationQueue roots the queue at {audioDir}/requests.
func NewActivationQueue(audioDir string) *ActivationQueue {
	return &ActivationQueue{dir: filepath.Join(audioDir, "requests")}
}

func (q *ActivationQueue) stagePath(stage, id string) string {
	return filepath.Join(q.dir, stage, id+".json")
}

// newRequestID is sortable by time (UnixNano prefix → lexical == chronological)
// and filesystem-safe: the work name (spaces/Cyrillic) is folded into a short
// hash rather than embedded in the filename.
func newRequestID(op ActivationOp, category, work string, now time.Time) string {
	sum := sha1.Sum([]byte(category + "/" + work)) //nolint:gosec // id only, not security
	return fmt.Sprintf("%d-%s-%s-%s", now.UnixNano(), op, category, hex.EncodeToString(sum[:])[:8])
}

// Enqueue writes a fresh request to pending/ atomically and returns its id.
func (q *ActivationQueue) Enqueue(req ActivationRequest) (string, error) {
	if req.RequestedAt.IsZero() {
		req.RequestedAt = time.Now()
	}
	if req.ID == "" {
		req.ID = newRequestID(req.Op, req.Category, req.Work, req.RequestedAt)
	}
	if err := q.writeStage(reqPending, req); err != nil {
		return "", err
	}
	return req.ID, nil
}

// writeStage marshals a request into {stage}/{id}.json atomically (tmp+rename).
func (q *ActivationQueue) writeStage(stage string, req ActivationRequest) error {
	path := q.stagePath(stage, req.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("failed to create %s dir: %w", stage, err)
	}
	data, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil { //nolint:gosec
		return fmt.Errorf("failed to write request: %w", err)
	}
	return os.Rename(tmp, path)
}

func (q *ActivationQueue) read(stage, id string) (ActivationRequest, error) {
	data, err := os.ReadFile(q.stagePath(stage, id)) //nolint:gosec
	if err != nil {
		return ActivationRequest{}, err
	}
	var req ActivationRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return ActivationRequest{}, fmt.Errorf("failed to parse request %s: %w", id, err)
	}
	return req, nil
}

// list reads and chronologically sorts every request in a stage, skipping junk
// and unreadable/partial files.
func (q *ActivationQueue) list(stage string) ([]ActivationRequest, error) {
	entries, err := os.ReadDir(filepath.Join(q.dir, stage))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %s dir: %w", stage, err)
	}
	var out []ActivationRequest
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		req, rerr := q.read(stage, strings.TrimSuffix(e.Name(), ".json"))
		if rerr != nil {
			continue
		}
		out = append(out, req)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListPending returns queued requests oldest-first (what the Mac agent drains).
func (q *ActivationQueue) ListPending() ([]ActivationRequest, error) { return q.list(reqPending) }

// ListDone returns completed requests oldest-first (what the VM watcher finalizes).
func (q *ActivationQueue) ListDone() ([]ActivationRequest, error) { return q.list(reqDone) }

// move atomically renames a request between stages. A missing source means the
// transition already happened (a lost race) — the error lets callers skip idempotently.
func (q *ActivationQueue) move(from, to, id string) error {
	dst := q.stagePath(to, id)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("failed to create %s dir: %w", to, err)
	}
	return os.Rename(q.stagePath(from, id), dst)
}

// Claim moves a pending request to claimed and returns it. A second claim of the
// same id fails (its rename source is gone) — the guarantee against double work.
func (q *ActivationQueue) Claim(id string) (ActivationRequest, error) {
	if err := q.move(reqPending, reqClaimed, id); err != nil {
		return ActivationRequest{}, err
	}
	return q.read(reqClaimed, id)
}

// Complete marks a claimed request done (upload to R2 succeeded).
func (q *ActivationQueue) Complete(id string) error { return q.move(reqClaimed, reqDone, id) }

// writeFailReason records a failure reason in a sibling .err sidecar so both Go
// and the shell agent carry it without touching the JSON payload.
func (q *ActivationQueue) writeFailReason(id, reason string) error {
	if reason == "" {
		return nil
	}
	errPath := filepath.Join(q.dir, reqFailed, id+".err")
	if err := os.MkdirAll(filepath.Dir(errPath), 0o750); err != nil {
		return fmt.Errorf("failed to create %s dir: %w", reqFailed, err)
	}
	if err := os.WriteFile(errPath, []byte(reason), 0o640); err != nil { //nolint:gosec
		return fmt.Errorf("failed to write error sidecar: %w", err)
	}
	return nil
}

// Fail marks a claimed request failed (the Mac agent's upload step errored).
func (q *ActivationQueue) Fail(id, reason string) error {
	if err := q.writeFailReason(id, reason); err != nil {
		return err
	}
	return q.move(reqClaimed, reqFailed, id)
}

// Reject moves a done request to failed when the VM's finalize step errors (the
// Mac uploaded, but building state/feed failed — e.g. a part is missing in R2).
func (q *ActivationQueue) Reject(id, reason string) error {
	if err := q.writeFailReason(id, reason); err != nil {
		return err
	}
	return q.move(reqDone, reqFailed, id)
}

// FailReason reads the error sidecar for a failed request ("" if none).
func (q *ActivationQueue) FailReason(id string) string {
	data, err := os.ReadFile(filepath.Join(q.dir, reqFailed, id+".err")) //nolint:gosec
	if err != nil {
		return ""
	}
	return string(data)
}

// Archive retires a done request after the VM has applied its state+feed, keeping
// the done/ stage a live worklist rather than an ever-growing log.
func (q *ActivationQueue) Archive(id string) error { return q.move(reqDone, reqArchive, id) }

package publisher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DurationProvider reads audio duration in seconds (implemented by duration.Service)
type DurationProvider interface {
	File(fname string) int
}

// Service is a library of sleeping works with one active work per subscription.
// The filesystem tree under AudioDir/originals is the source of truth (a work =
// a folder of parts, optionally split into season subfolders); a work goes live
// only when Activated — its parts upload to R2 and fill that category's feed,
// evicting whatever work was active before.
//
// Layout under AudioDir:
//
//	originals/{category}/               feed.yaml — the fixed channel of the type (optional)
//	originals/{category}/{work}/        a work = folder of parts (+ optional work.yaml)
//	originals/{category}/{work}/[Сезон NN/]NN - part.ext
//	processed/{category}/{work}/...     loudnorm-normalized upload copies (cache)
//	state/{category}.json               active work + its published episodes
//	feeds/{category}.xml                generated feed (public /holzweg/{secret}/{cat}.xml)
type Service struct {
	R2       *R2Store
	AudioDir string
	Secret   string // path secret shared by R2 keys and feed URLs
	Duration DurationProvider
	BaseURL  string // VM base url for building feed links
}

// CategoryState is the per-category registry: which work is live and the
// episodes (parts) currently published to R2 for it. Empty ActiveWork = the
// subscription is dormant (valid but empty feed).
type CategoryState struct {
	ActiveWork string    `json:"active_work"` // work slug, "" = nothing active
	Episodes   []Episode `json:"episodes"`    // parts of the active work published in R2
}

// CatWork is one work in the library catalog (a filesystem scan, no persistence)
type CatWork struct {
	Category string `json:"category"`
	Slug     string `json:"slug"`  // folder name = activation/dedup key
	Title    string `json:"title"` // work.yaml title or slug
	Parts    int    `json:"parts"`
	Seasons  int    `json:"seasons"` // distinct season subfolders, 0 = flat
	Finite   bool   `json:"finite"`
	Active   bool   `json:"active"`
}

// partRef is one audio part discovered while walking a work folder
type partRef struct {
	rel    string // path relative to the work dir (may include a season subdir)
	abs    string // absolute source path
	season int
	order  int
	title  string
}

// stateFile is the per-category state path
func (p *Service) stateFile(category string) string {
	return filepath.Join(p.AudioDir, "state", category+".json")
}

// categoryDir is where the category's works and feed.yaml live
func (p *Service) categoryDir(category string) string {
	return filepath.Join(p.AudioDir, "originals", category)
}

// feedFile is the generated feed path
func (p *Service) feedFile(category string) string {
	return filepath.Join(p.AudioDir, "feeds", category+".xml")
}

// FeedURL is the subscription link for a category. The public path is the
// wayfinding alias /holzweg — Caddy rewrites /holzweg/* → the app's /pod/*
// route, so the app never learns the alias and shoulder-surfers can't read it.
func (p *Service) FeedURL(category string) string {
	return fmt.Sprintf("%s/holzweg/%s/%s.xml", strings.TrimRight(p.BaseURL, "/"), p.Secret, category)
}

// loadState reads the category registry (missing file = empty state)
func (p *Service) loadState(category string) (CategoryState, error) {
	data, err := os.ReadFile(p.stateFile(category)) //nolint:gosec
	if os.IsNotExist(err) {
		return CategoryState{}, nil
	}
	if err != nil {
		return CategoryState{}, fmt.Errorf("failed to read state: %w", err)
	}
	var st CategoryState
	if err := json.Unmarshal(data, &st); err != nil {
		return CategoryState{}, fmt.Errorf("failed to parse state: %w", err)
	}
	return st, nil
}

// saveState writes the registry atomically
func (p *Service) saveState(category string, st CategoryState) error {
	path := p.stateFile(category)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("failed to create state dir: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil { //nolint:gosec
		return fmt.Errorf("failed to write state: %w", err)
	}
	return os.Rename(tmp, path)
}

// ActiveWork returns the slug of the work currently live in a category ("" = none)
func (p *Service) ActiveWork(category string) (string, error) {
	if err := validCategory(category); err != nil {
		return "", err
	}
	st, err := p.loadState(category)
	if err != nil {
		return "", err
	}
	return st.ActiveWork, nil
}

// gatherParts walks a work folder and returns its audio parts sorted in listen
// order (season, track number, path). Files directly in the work dir are season
// 0; each subfolder is a season (number parsed from its name).
func (p *Service) gatherParts(category, work string) ([]partRef, error) {
	workDir := filepath.Join(p.categoryDir(category), work)
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return nil, err
	}
	var parts []partRef
	for _, e := range entries {
		if e.IsDir() {
			season := parseSeasonNum(e.Name())
			seasonDir := filepath.Join(workDir, e.Name())
			seasonFiles, rderr := os.ReadDir(seasonDir)
			if rderr != nil {
				continue
			}
			for _, sf := range seasonFiles {
				if sf.IsDir() || mimeForFile(sf.Name()) == "" {
					continue
				}
				order, title := parseTrackName(sf.Name())
				parts = append(parts, partRef{
					rel:    filepath.Join(e.Name(), sf.Name()),
					abs:    filepath.Join(seasonDir, sf.Name()),
					season: season,
					order:  order,
					title:  title,
				})
			}
			continue
		}
		if mimeForFile(e.Name()) == "" {
			continue
		}
		order, title := parseTrackName(e.Name())
		parts = append(parts, partRef{
			rel:    e.Name(),
			abs:    filepath.Join(workDir, e.Name()),
			season: 0,
			order:  order,
			title:  title,
		})
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].season != parts[j].season {
			return parts[i].season < parts[j].season
		}
		if parts[i].order != parts[j].order {
			return parts[i].order < parts[j].order
		}
		return parts[i].rel < parts[j].rel
	})
	return parts, nil
}

// Catalog scans the library tree and returns every work (source of truth = the
// folders, no bolt/persistence). Empty folders and non-audio content are skipped.
func (p *Service) Catalog() ([]CatWork, error) {
	root := filepath.Join(p.AudioDir, "originals")
	catDirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []CatWork
	for _, cd := range catDirs {
		if !cd.IsDir() {
			continue
		}
		category := cd.Name()
		if validCategory(category) != nil {
			continue
		}
		active, _ := p.ActiveWork(category)
		workDirs, rderr := os.ReadDir(filepath.Join(root, category))
		if rderr != nil {
			continue
		}
		for _, wd := range workDirs {
			if !wd.IsDir() {
				continue
			}
			work := wd.Name()
			parts, perr := p.gatherParts(category, work)
			if perr != nil || len(parts) == 0 {
				continue // not a work (empty or no audio)
			}
			seasons := map[int]bool{}
			for _, pt := range parts {
				if pt.season > 0 {
					seasons[pt.season] = true
				}
			}
			wc := LoadWorkConfig(filepath.Join(root, category, work))
			title := strings.TrimSpace(wc.Title)
			if title == "" {
				title = work
			}
			out = append(out, CatWork{
				Category: category,
				Slug:     work,
				Title:    title,
				Parts:    len(parts),
				Seasons:  len(seasons),
				Finite:   wc.Finite(),
				Active:   active == work,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title)
	})
	return out, nil
}

// Activate makes a work live in its category: all its parts are uploaded to R2,
// then the category state + feed swap over, then the previously active work's R2
// objects are freed. The upload-first ordering means a mid-run failure leaves the
// old active work still serving. progress (nil-safe) ticks (done, total).
//
// This is the single-machine path (VM reads the audio bytes and uploads them). In
// variant A the upload moves to the Mac agent and the VM runs Finalize instead;
// both share commitActive so the state/feed/evict half stays identical.
func (p *Service) Activate(ctx context.Context, category, work string, progress func(done, total int)) error {
	parts, err := p.gatherForActivate(category, work)
	if err != nil {
		return err
	}
	old, err := p.loadState(category)
	if err != nil {
		return err
	}

	total := len(parts)
	eps := make([]Episode, 0, total)
	for i, pt := range parts {
		key := p.partKey(category, work, pt)
		relKey := filepath.ToSlash(filepath.Join(work, pt.rel))

		src, perr := p.prepareForUpload(ctx, pt.abs, category, relKey)
		if perr != nil {
			return fmt.Errorf("normalize %s: %w", pt.rel, perr)
		}
		fi, statErr := os.Stat(src)
		if statErr != nil {
			return statErr
		}
		publicURL := p.R2.PublicURL(key)
		if ok, _ := p.R2.Exists(ctx, key); !ok {
			publicURL, err = p.R2.Upload(ctx, src, key, mimeForFile(pt.rel))
			if err != nil {
				return err
			}
		}
		eps = append(eps, p.newEpisode(work, pt, key, publicURL, fi.Size()))
		if progress != nil {
			progress(i+1, total)
		}
	}
	return p.commitActive(ctx, category, work, old, eps)
}

// Finalize is the VM half of variant A: the Mac agent has already uploaded the
// work's parts to R2, so the VM only rebuilds the episode list (sizes read via
// R2 HEAD, not local bytes), swaps the category's active work, and evicts the
// previous one. It errors if any part is missing from R2 — that means the Mac
// upload was incomplete, so the old work must keep serving.
func (p *Service) Finalize(ctx context.Context, category, work string) error {
	parts, err := p.gatherForActivate(category, work)
	if err != nil {
		return err
	}
	old, err := p.loadState(category)
	if err != nil {
		return err
	}

	eps := make([]Episode, 0, len(parts))
	for _, pt := range parts {
		key := p.partKey(category, work, pt)
		size, serr := p.R2.Size(ctx, key)
		if serr != nil {
			return fmt.Errorf("check R2 for %s: %w", pt.rel, serr)
		}
		if size == 0 {
			return fmt.Errorf("part not yet in R2 (upload incomplete): %s", pt.rel)
		}
		eps = append(eps, p.newEpisode(work, pt, key, p.R2.PublicURL(key), size))
	}
	return p.commitActive(ctx, category, work, old, eps)
}

// gatherForActivate validates the category/work names and returns the work's
// parts in listen order, erroring on a missing or empty work.
func (p *Service) gatherForActivate(category, work string) ([]partRef, error) {
	if err := validCategory(category); err != nil {
		return nil, err
	}
	if work == "" || strings.ContainsAny(work, `/\`) || work == ".." {
		return nil, fmt.Errorf("bad work name %q", work)
	}
	parts, err := p.gatherParts(category, work)
	if err != nil {
		return nil, fmt.Errorf("work %q not found in %s: %w", work, category, err)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("work %q has no audio parts", work)
	}
	return parts, nil
}

// partKey is the deterministic R2 key for a part: a/{secret}/{category}/{work}/{rel}.
// Both the VM (Activate/Finalize) and the Mac agent derive keys this way, so they
// always agree on where a part lives.
func (p *Service) partKey(category, work string, pt partRef) string {
	relKey := filepath.ToSlash(filepath.Join(work, pt.rel))
	return fmt.Sprintf("a/%s/%s/%s", p.Secret, category, relKey)
}

// newEpisode builds one feed episode from a part and its known R2 key/URL/size.
func (p *Service) newEpisode(work string, pt partRef, key, publicURL string, size int64) Episode {
	return Episode{
		File:        pt.rel,
		Title:       pt.title,
		Work:        work,
		Season:      pt.season,
		Order:       pt.order,
		R2Key:       key,
		PublicURL:   publicURL,
		MimeType:    mimeForFile(pt.rel),
		SizeBytes:   size,
		DurationSec: p.durationOf(pt.abs),
		PublishedAt: time.Now().UTC(),
	}
}

// commitActive persists a freshly-built episode list as the category's active
// work, regenerates the feed, then frees the previously active work's now-
// unreferenced R2 objects. Callers guarantee every episode key already exists in
// R2 before this runs, so the old work keeps serving until the atomic state swap.
// Re-running with the same work is idempotent (old == new → nothing evicted).
func (p *Service) commitActive(ctx context.Context, category, work string, old CategoryState, eps []Episode) error {
	if err := p.saveState(category, CategoryState{ActiveWork: work, Episodes: eps}); err != nil {
		return err
	}
	if err := p.RegenerateFeed(category); err != nil {
		return err
	}
	if old.ActiveWork != "" && old.ActiveWork != work {
		newKeys := make(map[string]bool, len(eps))
		for _, e := range eps {
			newKeys[e.R2Key] = true
		}
		for _, e := range old.Episodes {
			if !newKeys[e.R2Key] {
				if derr := p.R2.Delete(ctx, e.R2Key); derr != nil {
					log.Printf("[WARN] activate: failed to delete old key %s: %v", e.R2Key, derr)
				}
			}
		}
	}
	log.Printf("[INFO] activated «%s» in %s: %d parts", work, category, len(eps))
	return nil
}

// Deactivate empties a category feed: the active work's R2 objects are deleted
// and the state cleared. Originals on disk are never touched.
func (p *Service) Deactivate(ctx context.Context, category string) error {
	if err := validCategory(category); err != nil {
		return err
	}
	st, err := p.loadState(category)
	if err != nil {
		return err
	}
	for _, e := range st.Episodes {
		if derr := p.R2.Delete(ctx, e.R2Key); derr != nil {
			log.Printf("[WARN] deactivate: failed to delete %s: %v", e.R2Key, derr)
		}
	}
	if err := p.saveState(category, CategoryState{}); err != nil {
		return err
	}
	log.Printf("[INFO] deactivated %s (was «%s»)", category, st.ActiveWork)
	return p.RegenerateFeed(category)
}

// prepareForUpload returns the local path to upload for a part: the loudnorm-
// normalized copy (cached under processed/) when normalization is on for the
// category and the file is an mp3, otherwise the original untouched.
func (p *Service) prepareForUpload(ctx context.Context, srcPath, category, relKey string) (string, error) {
	cfg := LoadFeedConfig(p.categoryDir(category), category)
	if !cfg.NormalizeEnabled() || !strings.EqualFold(filepath.Ext(srcPath), ".mp3") {
		return srcPath, nil
	}
	out := filepath.Join(p.AudioDir, "processed", category, filepath.FromSlash(relKey))
	if fi, err := os.Stat(out); err == nil && fi.Size() > 0 {
		return out, nil // cached
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		return "", fmt.Errorf("failed to create processed dir: %w", err)
	}
	ffCtx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()
	tmp := out + ".part.mp3"
	// loudnorm to -16 LUFS podcast target, 44.1kHz 128kbps
	cmd := exec.CommandContext(ffCtx, "ffmpeg", "-nostdin", "-y", "-i", srcPath,
		"-af", "loudnorm=I=-16:TP=-1.5:LRA=11", "-ar", "44100", "-b:a", "128k", tmp)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	log.Printf("[INFO] normalizing loudness: %s", relKey)
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("ffmpeg loudnorm failed: %w, stderr tail: %s", err, tailOf(stderr.String(), 300))
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", fmt.Errorf("failed to finalize processed file: %w", err)
	}
	return out, nil
}

// tailOf returns the last n characters of s (for trimming ffmpeg stderr in logs)
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// RegenerateFeed rebuilds the category feed.xml from the active work's episodes
func (p *Service) RegenerateFeed(category string) error {
	if err := validCategory(category); err != nil {
		return err
	}
	st, err := p.loadState(category)
	if err != nil {
		return err
	}
	cfg := LoadFeedConfig(p.categoryDir(category), category)

	xmlData, err := BuildFeedXML(cfg, st.Episodes)
	if err != nil {
		return err
	}
	path := p.feedFile(category)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("failed to create feeds dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, xmlData, 0o640); err != nil { //nolint:gosec
		return fmt.Errorf("failed to write feed: %w", err)
	}
	return os.Rename(tmp, path)
}

// EpisodeList returns the active work's episodes in listen order (season, track,
// path) — the parts currently in the feed
func (p *Service) EpisodeList(category string) ([]Episode, error) {
	if err := validCategory(category); err != nil {
		return nil, err
	}
	st, err := p.loadState(category)
	if err != nil {
		return nil, err
	}
	sortSerial(st.Episodes)
	return st.Episodes, nil
}

// Categories lists categories that have a state file (an active or once-active feed)
func (p *Service) Categories() ([]string, error) {
	files, err := filepath.Glob(filepath.Join(p.AudioDir, "state", "*.json"))
	if err != nil {
		return nil, err
	}
	var cats []string
	for _, f := range files {
		cats = append(cats, strings.TrimSuffix(filepath.Base(f), ".json"))
	}
	return cats, nil
}

// durationOf is nil-safe around the duration provider
func (p *Service) durationOf(path string) int {
	if p.Duration == nil {
		return 0
	}
	return p.Duration.File(path)
}

// validCategory guards against path traversal in category names
func validCategory(category string) error {
	if category == "" || strings.ContainsAny(category, "/\\.") {
		return fmt.Errorf("bad category name %q (letters, digits, dashes)", category)
	}
	return nil
}

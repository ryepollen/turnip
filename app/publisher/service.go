package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DurationProvider reads audio duration in seconds (implemented by duration.Service)
type DurationProvider interface {
	File(fname string) int
}

// Service publishes personal audio (bought books, courses) as private podcast
// feeds: local file → R2 (players download from there, zero egress) → per
// category feed.xml served by the VM under a secret path.
//
// Layout under AudioDir:
//
//	originals/{category}/   source files + feed.yaml (never modified)
//	state/{category}.json   published episodes registry
//	feeds/{category}.xml    generated feeds (served via /pod/{secret}/...)
type Service struct {
	R2       *R2Store
	AudioDir string
	Secret   string // path secret shared by R2 keys and feed URLs
	Duration DurationProvider
	BaseURL  string // VM base url for building feed links
}

// stateFile is the per-category episodes registry path
func (p *Service) stateFile(category string) string {
	return filepath.Join(p.AudioDir, "state", category+".json")
}

// categoryDir is where originals and feed.yaml live
func (p *Service) categoryDir(category string) string {
	return filepath.Join(p.AudioDir, "originals", category)
}

// feedFile is the generated feed path
func (p *Service) feedFile(category string) string {
	return filepath.Join(p.AudioDir, "feeds", category+".xml")
}

// FeedURL is the subscription link for a category
func (p *Service) FeedURL(category string) string {
	return fmt.Sprintf("%s/pod/%s/%s.xml", strings.TrimRight(p.BaseURL, "/"), p.Secret, category)
}

// loadEpisodes reads the category registry (missing file = empty)
func (p *Service) loadEpisodes(category string) ([]Episode, error) {
	data, err := os.ReadFile(p.stateFile(category)) //nolint:gosec
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read state: %w", err)
	}
	var eps []Episode
	if err := json.Unmarshal(data, &eps); err != nil {
		return nil, fmt.Errorf("failed to parse state: %w", err)
	}
	return eps, nil
}

// saveEpisodes writes the registry atomically
func (p *Service) saveEpisodes(category string, eps []Episode) error {
	path := p.stateFile(category)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("failed to create state dir: %w", err)
	}
	data, err := json.MarshalIndent(eps, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil { //nolint:gosec
		return fmt.Errorf("failed to write state: %w", err)
	}
	return os.Rename(tmp, path)
}

// PublishFile uploads one local audio file into the category feed. Idempotent
// by filename: re-publishing an already known file just returns its episode.
func (p *Service) PublishFile(ctx context.Context, localPath, category string) (Episode, error) {
	if err := validCategory(category); err != nil {
		return Episode{}, err
	}
	fi, err := os.Stat(localPath)
	if err != nil {
		return Episode{}, fmt.Errorf("file not found: %w", err)
	}

	eps, err := p.loadEpisodes(category)
	if err != nil {
		return Episode{}, err
	}
	base := filepath.Base(localPath)
	for _, ep := range eps {
		if ep.File == base {
			return ep, nil // already published
		}
	}

	order, title := parseTrackName(base)
	contentType := mimeForFile(base)
	if contentType == "" {
		return Episode{}, fmt.Errorf("unsupported audio format: %s", base)
	}
	key := fmt.Sprintf("a/%s/%s/%s", p.Secret, category, base)

	log.Printf("[INFO] publishing %s to %s (%d bytes)", base, key, fi.Size())
	publicURL, err := p.R2.Upload(ctx, localPath, key, contentType)
	if err != nil {
		return Episode{}, err
	}

	ep := Episode{
		File:        base,
		Title:       title,
		Order:       order,
		R2Key:       key,
		PublicURL:   publicURL,
		MimeType:    mimeForFile(base),
		SizeBytes:   fi.Size(),
		DurationSec: p.durationOf(localPath),
		PublishedAt: time.Now().UTC(),
	}
	eps = append(eps, ep)
	if err := p.saveEpisodes(category, eps); err != nil {
		return Episode{}, err
	}
	if err := p.RegenerateFeed(category); err != nil {
		return Episode{}, err
	}
	log.Printf("[INFO] published %s: %s", base, publicURL)
	return ep, nil
}

// RegenerateFeed rebuilds the category feed.xml from the registry
func (p *Service) RegenerateFeed(category string) error {
	if err := validCategory(category); err != nil {
		return err
	}
	eps, err := p.loadEpisodes(category)
	if err != nil {
		return err
	}
	cfg := LoadFeedConfig(p.categoryDir(category), category)

	xmlData, err := BuildFeedXML(cfg, eps)
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

// EpisodeList returns the published episodes of a category, serial order
// first (как в ленте), for the bot's /archive view
func (p *Service) EpisodeList(category string) ([]Episode, error) {
	if err := validCategory(category); err != nil {
		return nil, err
	}
	eps, err := p.loadEpisodes(category)
	if err != nil {
		return nil, err
	}
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].Order != eps[j].Order {
			return eps[i].Order < eps[j].Order
		}
		return eps[i].File < eps[j].File
	})
	return eps, nil
}

// removeFromState drops one episode by filename, returns the removed episode
func (p *Service) removeFromState(category, file string) (Episode, error) {
	eps, err := p.loadEpisodes(category)
	if err != nil {
		return Episode{}, err
	}
	for i, ep := range eps {
		if ep.File != file {
			continue
		}
		eps = append(eps[:i], eps[i+1:]...)
		if err := p.saveEpisodes(category, eps); err != nil {
			return Episode{}, err
		}
		return ep, nil
	}
	return Episode{}, fmt.Errorf("эпизод %q не найден в %s", file, category)
}

// Archive removes an episode from the feed for good: R2 object deleted,
// original moved to archive/{category}/ (players keep downloaded copies)
func (p *Service) Archive(ctx context.Context, category, file string) error {
	ep, err := p.removeFromState(category, file)
	if err != nil {
		return err
	}
	if err := p.R2.Delete(ctx, ep.R2Key); err != nil {
		log.Printf("[WARN] archive: failed to delete %s from R2: %v", ep.R2Key, err)
	}
	_ = os.Remove(filepath.Join(p.AudioDir, "processed", category, file))

	orig := filepath.Join(p.categoryDir(category), file)
	if _, statErr := os.Stat(orig); statErr == nil {
		dst := filepath.Join(p.AudioDir, "archive", category, file)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err == nil {
			if err := os.Rename(orig, dst); err != nil {
				log.Printf("[WARN] archive: failed to move original %s: %v", orig, err)
			}
		}
	}
	return p.RegenerateFeed(category)
}

// Requeue forgets an episode so the watcher re-processes the original from
// scratch (useful after improving normalization or fixing a broken upload)
func (p *Service) Requeue(ctx context.Context, category, file string) error {
	orig := filepath.Join(p.categoryDir(category), file)
	if _, err := os.Stat(orig); err != nil {
		return fmt.Errorf("оригинала нет в originals/%s (в архиве?): %w", category, err)
	}
	ep, err := p.removeFromState(category, file)
	if err != nil {
		return err
	}
	if err := p.R2.Delete(ctx, ep.R2Key); err != nil {
		log.Printf("[WARN] requeue: failed to delete %s from R2: %v", ep.R2Key, err)
	}
	_ = os.Remove(filepath.Join(p.AudioDir, "processed", category, file))
	return p.RegenerateFeed(category)
}

// Categories lists categories that have published episodes
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

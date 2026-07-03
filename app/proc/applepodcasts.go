package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// ApplePodcastEpisode is the resolved metadata of one episode
type ApplePodcastEpisode struct {
	PodcastID   string
	EpisodeID   string
	Title       string
	Show        string
	AudioURL    string // direct enclosure mp3 from the podcast RSS
	Artwork     string
	Date        string // YYYY-MM-DD
	DurationMin int
	Description string
}

// SourceID returns the stable dedup id used across the feed and notes
func (e *ApplePodcastEpisode) SourceID() string { return "ap_" + e.EpisodeID }

// AppleResolver turns podcasts.apple.com links into direct audio URLs and
// metadata via the public iTunes Lookup API (no auth needed): the episode
// page itself hosts no audio, but the lookup returns the RSS enclosure.
type AppleResolver struct {
	BaseURL string // default https://itunes.apple.com, overridable for tests
	client  *http.Client
}

// NewAppleResolver creates a resolver
func NewAppleResolver() *AppleResolver {
	return &AppleResolver{
		BaseURL: "https://itunes.apple.com",
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

var applePodcastRe = regexp.MustCompile(`podcasts\.apple\.com/.*/id(\d+)`)

// IsApplePodcastURL reports whether the link points to podcasts.apple.com
func IsApplePodcastURL(rawURL string) bool {
	return applePodcastRe.MatchString(rawURL)
}

// parseAppleURL extracts the podcast id (idNNN path segment) and the episode
// id (?i=NNN query param, empty when the link is to the show, not an episode)
func parseAppleURL(rawURL string) (podcastID, episodeID string, err error) {
	m := applePodcastRe.FindStringSubmatch(rawURL)
	if m == nil {
		return "", "", fmt.Errorf("not an apple podcasts url: %s", rawURL)
	}
	podcastID = m[1]
	if u, perr := url.Parse(rawURL); perr == nil {
		episodeID = u.Query().Get("i")
	}
	return podcastID, episodeID, nil
}

// lookupResult mirrors the fields we need from the iTunes Lookup API
type lookupResult struct {
	Results []struct {
		WrapperType      string    `json:"wrapperType"` // "track" for podcastEpisode
		Kind             string    `json:"kind"`        // "podcast-episode"
		TrackID          int64     `json:"trackId"`
		TrackName        string    `json:"trackName"`
		CollectionName   string    `json:"collectionName"`
		EpisodeURL       string    `json:"episodeUrl"`
		ReleaseDate      string    `json:"releaseDate"` // RFC3339
		TrackTimeMillis  int64     `json:"trackTimeMillis"`
		ArtworkURL600    string    `json:"artworkUrl600"`
		ArtworkURL160    string    `json:"artworkUrl160"`
		DescriptionBlock *struct { //nolint:staticcheck // apple nests it
			Standard string `json:"standard"`
		} `json:"description"`
		ShortDescription string `json:"shortDescription"`
	} `json:"results"`
}

// Resolve fetches episode metadata and the direct audio URL for an episode link
func (r *AppleResolver) Resolve(ctx context.Context, rawURL string) (*ApplePodcastEpisode, error) {
	podcastID, episodeID, err := parseAppleURL(rawURL)
	if err != nil {
		return nil, err
	}
	if episodeID == "" {
		return nil, fmt.Errorf("это ссылка на подкаст целиком — нужна ссылка на конкретный эпизод (открой эпизод и поделись им)")
	}

	lookupURL := fmt.Sprintf("%s/lookup?id=%s&entity=podcastEpisode&limit=200", r.BaseURL, podcastID)
	req, err := http.NewRequestWithContext(ctx, "GET", lookupURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create lookup request: %w", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lookup request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lookup failed (status %d)", resp.StatusCode)
	}

	var lr lookupResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 5*1024*1024)).Decode(&lr); err != nil {
		return nil, fmt.Errorf("failed to decode lookup response: %w", err)
	}

	for _, item := range lr.Results {
		if fmt.Sprintf("%d", item.TrackID) != episodeID || item.Kind != "podcast-episode" {
			continue
		}
		if item.EpisodeURL == "" {
			return nil, fmt.Errorf("у эпизода нет прямой аудио-ссылки в каталоге Apple")
		}
		ep := &ApplePodcastEpisode{
			PodcastID:   podcastID,
			EpisodeID:   episodeID,
			Title:       item.TrackName,
			Show:        item.CollectionName,
			AudioURL:    item.EpisodeURL,
			DurationMin: int(item.TrackTimeMillis / 60000),
			Description: item.ShortDescription,
		}
		if item.DescriptionBlock != nil && item.DescriptionBlock.Standard != "" {
			ep.Description = item.DescriptionBlock.Standard
		}
		if ep.Artwork = item.ArtworkURL600; ep.Artwork == "" {
			ep.Artwork = item.ArtworkURL160
		}
		if parsed, perr := time.Parse(time.RFC3339, item.ReleaseDate); perr == nil {
			ep.Date = parsed.Format("2006-01-02")
		}
		return ep, nil
	}
	// lookup returns the latest ~200 episodes; very old ones may be missing
	return nil, fmt.Errorf("эпизод %s не найден в каталоге (lookup отдаёт только последние ~200 эпизодов)", episodeID)
}

// DownloadEnclosure downloads the episode audio to destPath (atomic via .part)
func (r *AppleResolver) DownloadEnclosure(ctx context.Context, audioURL, destPath string) error {
	dlCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(dlCtx, "GET", audioURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("failed to create download request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)")

	// long downloads must not hit the resolver's 30s timeout
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed (status %d)", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("failed to create dest dir: %w", err)
	}
	part := destPath + ".part"
	f, err := os.Create(part) //nolint:gosec // path is built from our own hash
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(part)
		return fmt.Errorf("failed to write audio: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("failed to close file: %w", err)
	}
	if err := os.Rename(part, destPath); err != nil {
		return fmt.Errorf("failed to finalize file: %w", err)
	}
	return nil
}

// appleEpisodeIDFromURL is a cheap id extraction for dedup keys (no network)
func appleEpisodeIDFromURL(rawURL string) string {
	_, episodeID, err := parseAppleURL(rawURL)
	if err != nil || episodeID == "" {
		return ""
	}
	return "ap_" + episodeID
}

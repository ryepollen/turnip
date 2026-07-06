package proc

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ApplePodcastEpisode is the resolved metadata of one episode
type ApplePodcastEpisode struct {
	PodcastID   string
	EpisodeID   string
	GUID        string // RSS item guid, key for matching the show feed item
	Title       string
	Show        string
	AudioURL    string // direct enclosure mp3 from the podcast RSS
	FeedURL     string // the show's RSS feed
	Artwork     string
	Date        string // YYYY-MM-DD
	DurationMin int
	Description string
}

// EpisodeExtras is Podcasting 2.0 data from the show's own RSS: the official
// transcript, chapters and the full show notes (the catalog only has a stub)
type EpisodeExtras struct {
	TranscriptURL  string
	TranscriptType string
	ChaptersURL    string
	Description    string
}

// SourceID returns the stable dedup id used across the feed and notes
func (e *ApplePodcastEpisode) SourceID() string { return "ap_" + e.EpisodeID }

// AppleResolver turns podcasts.apple.com links into direct audio URLs and
// metadata via the public iTunes Lookup API (no auth needed): the episode
// page itself hosts no audio, but the lookup returns the RSS enclosure.
type AppleResolver struct {
	BaseURL string // default https://itunes.apple.com, overridable for tests
	client  *http.Client

	feedMu    sync.Mutex
	feedCache map[string]*podcastRSS // show RSS parsed once per process
}

// NewAppleResolver creates a resolver
func NewAppleResolver() *AppleResolver {
	return &AppleResolver{
		BaseURL:   "https://itunes.apple.com",
		client:    &http.Client{Timeout: 30 * time.Second},
		feedCache: map[string]*podcastRSS{},
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

// appleDescription tolerates both shapes Apple uses for episode description:
// a plain string or a nested {"standard": "..."} object, varying per feed
type appleDescription struct {
	Standard string
}

// UnmarshalJSON never fails: description is best-effort metadata and must not
// break the whole lookup response
func (d *appleDescription) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		d.Standard = s
		return nil
	}
	var obj struct {
		Standard string `json:"standard"`
	}
	if json.Unmarshal(data, &obj) == nil {
		d.Standard = obj.Standard
	}
	return nil
}

// lookupItem is one result entry of the iTunes Lookup API
type lookupItem struct {
	WrapperType      string            `json:"wrapperType"` // "track" for podcastEpisode
	Kind             string            `json:"kind"`        // "podcast-episode"
	TrackID          int64             `json:"trackId"`
	TrackName        string            `json:"trackName"`
	CollectionName   string            `json:"collectionName"`
	EpisodeURL       string            `json:"episodeUrl"`
	EpisodeGUID      string            `json:"episodeGuid"`
	FeedURL          string            `json:"feedUrl"`
	ReleaseDate      string            `json:"releaseDate"` // RFC3339
	TrackTimeMillis  int64             `json:"trackTimeMillis"`
	ArtworkURL600    string            `json:"artworkUrl600"`
	ArtworkURL160    string            `json:"artworkUrl160"`
	DescriptionBlock *appleDescription `json:"description"`
	ShortDescription string            `json:"shortDescription"`
}

// lookupResult mirrors the fields we need from the iTunes Lookup API
type lookupResult struct {
	Results []lookupItem `json:"results"`
}

// lookupEpisodes fetches all catalog episodes of a podcast (latest ~200)
func (r *AppleResolver) lookupEpisodes(ctx context.Context, podcastID string) (*lookupResult, error) {
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
	return &lr, nil
}

// episodeFromLookup converts one lookup item into ApplePodcastEpisode
func episodeFromLookup(podcastID string, item lookupItem) *ApplePodcastEpisode {
	ep := &ApplePodcastEpisode{
		PodcastID:   podcastID,
		EpisodeID:   fmt.Sprintf("%d", item.TrackID),
		GUID:        item.EpisodeGUID,
		Title:       item.TrackName,
		Show:        item.CollectionName,
		AudioURL:    item.EpisodeURL,
		FeedURL:     item.FeedURL,
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
	return ep
}

// EpisodeLink rebuilds a canonical apple podcasts episode URL
func (e *ApplePodcastEpisode) EpisodeLink() string {
	return fmt.Sprintf("https://podcasts.apple.com/podcast/id%s?i=%s", e.PodcastID, e.EpisodeID)
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

	lr, err := r.lookupEpisodes(ctx, podcastID)
	if err != nil {
		return nil, err
	}
	for _, item := range lr.Results {
		if fmt.Sprintf("%d", item.TrackID) != episodeID || item.Kind != "podcast-episode" {
			continue
		}
		if item.EpisodeURL == "" {
			return nil, fmt.Errorf("у эпизода нет прямой аудио-ссылки в каталоге Apple")
		}
		return episodeFromLookup(podcastID, item), nil
	}
	// lookup returns the latest ~200 episodes; very old ones may be missing
	return nil, fmt.Errorf("эпизод %s не найден в каталоге (lookup отдаёт только последние ~200 эпизодов)", episodeID)
}

// ResolveShow fetches all catalog episodes of a show link, oldest first —
// adding them in this order keeps the feed chronology right (pubDate = time
// of addition, each next episode lands newer)
func (r *AppleResolver) ResolveShow(ctx context.Context, rawURL string) (show string, eps []*ApplePodcastEpisode, err error) {
	podcastID, _, err := parseAppleURL(rawURL)
	if err != nil {
		return "", nil, err
	}
	lr, err := r.lookupEpisodes(ctx, podcastID)
	if err != nil {
		return "", nil, err
	}
	for _, item := range lr.Results {
		if item.Kind != "podcast-episode" || item.EpisodeURL == "" {
			continue
		}
		ep := episodeFromLookup(podcastID, item)
		if show == "" {
			show = ep.Show
		}
		eps = append(eps, ep)
	}
	if len(eps) == 0 {
		return show, nil, fmt.Errorf("в каталоге не нашлось эпизодов с аудио")
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i].Date < eps[j].Date })
	return show, eps, nil
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

// podcastRSS is the show feed with the Podcasting 2.0 fields we care about.
// encoding/xml matches by local name, so podcast:transcript works untyped.
type podcastRSS struct {
	Channel struct {
		Items []podcastRSSItem `xml:"item"`
	} `xml:"channel"`
}

type podcastRSSItem struct {
	GUID        string `xml:"guid"`
	Title       string `xml:"title"`
	Description string `xml:"description"`
	Enclosure   struct {
		URL string `xml:"url,attr"`
	} `xml:"enclosure"`
	Transcripts []struct {
		URL  string `xml:"url,attr"`
		Type string `xml:"type,attr"`
	} `xml:"transcript"`
	Chapters struct {
		URL string `xml:"url,attr"`
	} `xml:"chapters"`
}

// showRSS fetches and caches the parsed show feed
func (r *AppleResolver) showRSS(ctx context.Context, feedURL string) (*podcastRSS, error) {
	r.feedMu.Lock()
	if cached, ok := r.feedCache[feedURL]; ok {
		r.feedMu.Unlock()
		return cached, nil
	}
	r.feedMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, "GET", feedURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create feed request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feed request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed request failed (status %d)", resp.StatusCode)
	}

	var rss podcastRSS
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 20*1024*1024)).Decode(&rss); err != nil {
		return nil, fmt.Errorf("failed to parse show feed: %w", err)
	}
	r.feedMu.Lock()
	r.feedCache[feedURL] = &rss
	r.feedMu.Unlock()
	return &rss, nil
}

// FetchEpisodeExtras pulls Podcasting 2.0 data for an episode from the show's
// own RSS: official transcript link, chapters, full show notes. Best-effort —
// callers must survive an error (not every show has a usable feed).
func (r *AppleResolver) FetchEpisodeExtras(ctx context.Context, ep *ApplePodcastEpisode) (*EpisodeExtras, error) {
	if ep.FeedURL == "" {
		return nil, fmt.Errorf("show has no feed url in the catalog")
	}
	rss, err := r.showRSS(ctx, ep.FeedURL)
	if err != nil {
		return nil, err
	}

	for i := range rss.Channel.Items {
		item := &rss.Channel.Items[i]
		if !matchesEpisode(item, ep) {
			continue
		}
		extras := &EpisodeExtras{
			ChaptersURL: item.Chapters.URL,
			Description: strings.TrimSpace(item.Description),
		}
		extras.TranscriptURL, extras.TranscriptType = pickTranscript(item)
		return extras, nil
	}
	return nil, fmt.Errorf("episode not found in the show feed")
}

// matchesEpisode matches a feed item to a catalog episode: guid first,
// enclosure url second, exact title as a last resort
func matchesEpisode(item *podcastRSSItem, ep *ApplePodcastEpisode) bool {
	if ep.GUID != "" && strings.TrimSpace(item.GUID) == ep.GUID {
		return true
	}
	if ep.AudioURL != "" && item.Enclosure.URL == ep.AudioURL {
		return true
	}
	return ep.Title != "" && strings.TrimSpace(item.Title) == ep.Title
}

// pickTranscript chooses the most parseable transcript variant:
// timed formats (srt/vtt) first, plain text second, html/json skipped
func pickTranscript(item *podcastRSSItem) (url, typ string) {
	best := -1
	for _, tr := range item.Transcripts {
		if tr.URL == "" {
			continue
		}
		t := strings.ToLower(tr.Type)
		rank := -1
		switch {
		case strings.Contains(t, "srt") || strings.Contains(t, "vtt"):
			rank = 2
		case strings.Contains(t, "plain"):
			rank = 1
		}
		if rank > best {
			best, url, typ = rank, tr.URL, t
		}
	}
	return url, typ
}

// FetchTranscript downloads a transcript file (small text, 5MB cap)
func (r *AppleResolver) FetchTranscript(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("failed to create transcript request: %w", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("transcript request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("transcript request failed (status %d)", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return "", fmt.Errorf("failed to read transcript: %w", err)
	}
	return string(data), nil
}

// OfficialTranscript returns the publisher's transcript when the show ships
// one in its RSS: timed segments for srt/vtt, plain text otherwise.
// (tr == nil && plain == "") means there is none — fall back to Whisper.
func (r *AppleResolver) OfficialTranscript(ctx context.Context, ep *ApplePodcastEpisode) (tr *Transcript, plain string) {
	extras, err := r.FetchEpisodeExtras(ctx, ep)
	if err != nil || extras.TranscriptURL == "" {
		return nil, ""
	}
	content, err := r.FetchTranscript(ctx, extras.TranscriptURL)
	if err != nil {
		log.Printf("[WARN] failed to fetch official transcript %s: %v", extras.TranscriptURL, err)
		return nil, ""
	}
	if strings.Contains(extras.TranscriptType, "srt") || strings.Contains(extras.TranscriptType, "vtt") {
		if t := segmentsToTranscript(ParseSubtitleSegments(content)); t != nil {
			return t, ""
		}
	}
	if text := strings.TrimSpace(content); text != "" {
		return nil, text
	}
	return nil, ""
}

// podcastChapter is one entry of a Podcasting 2.0 chapters JSON
type podcastChapter struct {
	StartTime float64 `json:"startTime"`
	Title     string  `json:"title"`
}

// FetchChapters downloads and formats the chapters list as "MM:SS Title" lines
func (r *AppleResolver) FetchChapters(ctx context.Context, url string) string {
	if url == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, http.NoBody)
	if err != nil {
		return ""
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var parsed struct {
		Chapters []podcastChapter `json:"chapters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&parsed); err != nil {
		return ""
	}
	var b strings.Builder
	for _, ch := range parsed.Chapters {
		if strings.TrimSpace(ch.Title) == "" {
			continue
		}
		b.WriteString(strings.Trim(formatTimecode(ch.StartTime), "[]"))
		b.WriteString(" ")
		b.WriteString(ch.Title)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// appleEpisodeIDFromURL is a cheap id extraction for dedup keys (no network)
func appleEpisodeIDFromURL(rawURL string) string {
	_, episodeID, err := parseAppleURL(rawURL)
	if err != nil || episodeID == "" {
		return ""
	}
	return "ap_" + episodeID
}

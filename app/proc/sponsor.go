package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	ytfeed "github.com/umputun/feed-master/app/youtube/feed"
)

const sponsorBlockDefaultBase = "https://sponsor.ajay.app"

// SponsorClient reads sponsor segments from the public SponsorBlock API.
// Chapters are description lines only: the mp3 is not cut here.
type SponsorClient struct {
	BaseURL string
	HTTP    *http.Client
}

var defaultSponsorClient = &SponsorClient{
	BaseURL: sponsorBlockDefaultBase,
	HTTP:    &http.Client{Timeout: 8 * time.Second},
}

type sponsorSegment struct {
	Category   string    `json:"category"`
	ActionType string    `json:"actionType"`
	Segment    []float64 `json:"segment"`
}

// Chapters returns "MM:SS реклама" lines for sponsor skip segments.
// An id that is not a YouTube id, or HTTP 404, yields an empty string and
// no error: the episode stays as it was. A network or server failure is
// returned so the caller can log it and still save the episode.
func (c *SponsorClient) Chapters(ctx context.Context, videoID string) (string, error) {
	if c == nil || !ytfeed.ValidYouTubeID(videoID) {
		return "", nil
	}
	base := c.BaseURL
	if base == "" {
		base = sponsorBlockDefaultBase
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/skipSegments"
	q := u.Query()
	q.Set("videoID", videoID)
	q.Set("categories", `["sponsor"]`)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "turnip")

	client := c.HTTP
	if client == nil {
		client = defaultSponsorClient.HTTP
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sponsorblock status %d", resp.StatusCode)
	}
	var segs []sponsorSegment
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&segs); err != nil {
		return "", fmt.Errorf("sponsorblock response: %w", err)
	}
	return formatSponsorChapters(segs), nil
}

// formatSponsorChapters renders skip-type sponsor spans the same way
// FetchChapters renders podcast chapters: "MM:SS Title" lines, start time
// only, sorted by start. Other categories and mute/poi spans are dropped.
func formatSponsorChapters(segs []sponsorSegment) string {
	type span struct {
		start float64
	}
	var spans []span
	for _, seg := range segs {
		if seg.Category != "sponsor" || len(seg.Segment) < 2 || seg.Segment[0] < 0 {
			continue
		}
		if seg.ActionType != "" && seg.ActionType != "skip" {
			continue
		}
		spans = append(spans, span{start: seg.Segment[0]})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var b strings.Builder
	for _, sp := range spans {
		b.WriteString(strings.Trim(formatTimecode(sp.start), "[]"))
		b.WriteString(" реклама\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// applySponsorChapters appends chapter lines to a description. An error or
// an empty lookup leaves the description untouched, so a SponsorBlock miss
// cannot fail the episode.
func applySponsorChapters(description, lines string, err error) string {
	if err != nil || lines == "" {
		return description
	}
	return description + "\n\nГлавы:\n" + lines
}

func (t *TelegramBot) sponsorChapters(ctx context.Context, videoID string) (string, error) {
	if t == nil {
		return "", nil
	}
	c := t.Sponsors
	if c == nil {
		c = defaultSponsorClient
	}
	return c.Chapters(ctx, videoID)
}

package publisher

import (
	"crypto/sha1" //nolint:gosec // not used for security
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FeedConfig is the per-category feed.yaml in the category folder
type FeedConfig struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	Cover       string `yaml:"cover"`    // image URL (e.g. an R2 public link)
	Language    string `yaml:"language"` // default ru
	Type        string `yaml:"type"`     // serial | episodic, default serial
	Author      string `yaml:"author"`
	Normalize   *bool  `yaml:"normalize"` // loudnorm to -16 LUFS; default depends on category (see normalizeDefaultFor)
	// EpisodeStrip is a book/show prefix trimmed off each episode title in the
	// feed only. Files keep their full self-describing names on disk/iCloud (the
	// ferry is flat and drops the book folder), while Overcast shows the short
	// tail: filename "NN - Автор. «Книга». Часть N" → episode title "Часть N".
	EpisodeStrip string `yaml:"episode_strip"`
}

// NormalizeEnabled resolves the tri-state normalize flag (nil = on).
// LoadFeedConfig fills nil with a category-aware default, so this only sees nil
// for FeedConfigs built directly (e.g. in tests) — there the safe fallback is on.
func (c FeedConfig) NormalizeEnabled() bool {
	return c.Normalize == nil || *c.Normalize
}

// normalizeDefaultFor decides the loudnorm default when feed.yaml is silent.
// Off for every category (decision B, 2026-09-09). loudnorm needs the raw bytes +
// ffmpeg, but under variant A the VM holds 0-byte stub originals and never reads
// them: the Mac agent uploads parts to R2 and the VM only runs Finalize (sizes/
// duration via R2 HEAD/stream). So prepareForUpload is off the active path — a
// courses/podcasts "on" default would just be a promise the pipeline can't keep.
// An explicit normalize: true in feed.yaml still opts in on the legacy in-process
// Activate path. Fallback if uniform -16 LUFS becomes wanted: path A — move this
// loudnorm step into the Mac activate-agent, gated on a Normalize flag carried in
// the ActivationRequest (see prepareForUpload).
func normalizeDefaultFor(_ string) bool {
	return false
}

// LoadFeedConfig reads dir/feed.yaml; missing file yields defaults with the
// category name as the title
func LoadFeedConfig(dir, category string) FeedConfig {
	cfg := FeedConfig{}
	if data, err := os.ReadFile(filepath.Join(dir, "feed.yaml")); err == nil { //nolint:gosec
		_ = yaml.Unmarshal(data, &cfg)
	}
	if cfg.Title == "" {
		cfg.Title = category
	}
	if cfg.Language == "" {
		cfg.Language = "ru"
	}
	if cfg.Type != "episodic" {
		cfg.Type = "serial"
	}
	if cfg.Normalize == nil { // feed.yaml didn't say — pick the category default
		d := normalizeDefaultFor(category)
		cfg.Normalize = &d
	}
	return cfg
}

// Episode is one published audio part of the active work (state lives in a
// per-category JSON; File is the path relative to the work dir so parts in
// different seasons never collide as a dedup key).
type Episode struct {
	File        string    `json:"file"` // path relative to the work dir, dedup key
	Title       string    `json:"title"`
	Work        string    `json:"work,omitempty"`   // slug of the work this part belongs to
	Season      int       `json:"season,omitempty"` // season number, 0 = no seasons
	Order       int       `json:"order"`            // NN prefix for serial feeds, 0 if none
	R2Key       string    `json:"r2_key"`
	PublicURL   string    `json:"public_url"`
	MimeType    string    `json:"mime_type,omitempty"` // default audio/mpeg
	SizeBytes   int64     `json:"size_bytes"`
	DurationSec int       `json:"duration_sec"`
	PublishedAt time.Time `json:"published_at"`
}

// WorkConfig is the optional per-work work.yaml in a work folder
type WorkConfig struct {
	Title string `yaml:"title"`
	Cover string `yaml:"cover"`
	Type  string `yaml:"type"` // finite (default) | streaming
}

// Finite reports whether the work is a bounded set of parts (default). A
// streaming work is an open-ended archive (rolling window — follow-up #7).
func (w WorkConfig) Finite() bool {
	return !strings.EqualFold(strings.TrimSpace(w.Type), "streaming")
}

// LoadWorkConfig reads dir/work.yaml; missing file yields an empty config
func LoadWorkConfig(dir string) WorkConfig {
	cfg := WorkConfig{}
	if data, err := os.ReadFile(filepath.Join(dir, "work.yaml")); err == nil { //nolint:gosec
		_ = yaml.Unmarshal(data, &cfg)
	}
	return cfg
}

// audioMimeByExt maps supported extensions to enclosure MIME types
var audioMimeByExt = map[string]string{
	".mp3": "audio/mpeg",
	".m4a": "audio/mp4",
	".m4b": "audio/mp4",
	".aac": "audio/aac",
	".ogg": "audio/ogg",
}

// mimeForFile returns the enclosure type for a filename ("" = not audio)
func mimeForFile(name string) string {
	return audioMimeByExt[strings.ToLower(filepath.Ext(name))]
}

// GUID is stable and derived from the R2 key: changing it would make players
// mark everything unplayed and redownload
func (e Episode) GUID() string {
	h := sha1.New() //nolint:gosec // not used for security
	h.Write([]byte(e.R2Key))
	return fmt.Sprintf("turnip-%x", h.Sum(nil))[:23]
}

var trackNameRe = regexp.MustCompile(`^(\d{1,4})\s*[-–—._]\s*(.+)$`)

// parseTrackName splits "NN - Title.mp3" into order and human title
func parseTrackName(filename string) (order int, title string) {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	if m := trackNameRe.FindStringSubmatch(base); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n, strings.TrimSpace(m[2])
		}
	}
	return 0, base
}

var seasonNumRe = regexp.MustCompile(`(\d{1,3})`)

// parseSeasonNum pulls a season number out of a subfolder name ("Сезон 01",
// "Season 3", "S02" → 1/3/2). A season subfolder with no digits falls back to
// 1 so it still sorts before any numbered season is unlikely but stays stable.
func parseSeasonNum(dir string) int {
	if m := seasonNumRe.FindString(dir); m != "" {
		if n, err := strconv.Atoi(m); err == nil {
			return n
		}
	}
	return 1
}

// sortSerial orders parts by season, then track number, then path — the
// natural listen order across a multi-season work.
func sortSerial(eps []Episode) {
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].Season != eps[j].Season {
			return eps[i].Season < eps[j].Season
		}
		if eps[i].Order != eps[j].Order {
			return eps[i].Order < eps[j].Order
		}
		return eps[i].File < eps[j].File
	})
}

// episodeLeadPunct trims separators left after cutting a book/show prefix
// («Книга». Часть 1 → Часть 1), matching tune_get's chapterTitle cleanup.
const episodeLeadPunct = " \t.,:;–—«»\"'()-"

// stripEpisodePrefix removes a configured book/show prefix from an episode
// title for feed display only. Case-insensitive, tolerant of the punctuation
// that separates the prefix from the chapter tail. Falls back to the original
// title if stripping would leave it empty.
func stripEpisodePrefix(title, prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return title
	}
	t := strings.TrimSpace(title)
	if len(t) >= len(prefix) && strings.EqualFold(t[:len(prefix)], prefix) {
		t = strings.TrimLeft(t[len(prefix):], episodeLeadPunct)
		if t != "" {
			return t
		}
	}
	return strings.TrimSpace(title)
}

// serialEpoch is the base for synthetic pubDates of serial feeds: players sort
// by pubDate, so lesson N gets epoch+N minutes and the order always holds
var serialEpoch = time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)

// rss XML structures (RSS 2.0 + itunes namespace)
type rssXML struct {
	XMLName  xml.Name   `xml:"rss"`
	Version  string     `xml:"version,attr"`
	ItunesNS string     `xml:"xmlns:itunes,attr"`
	Channel  rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title        string    `xml:"title"`
	Description  string    `xml:"description,omitempty"`
	Language     string    `xml:"language,omitempty"`
	ItunesAuthor string    `xml:"itunes:author,omitempty"`
	ItunesType   string    `xml:"itunes:type,omitempty"`
	ItunesImage  *rssImage `xml:"itunes:image,omitempty"`
	Items        []rssItem `xml:"item"`
}

type rssImage struct {
	Href string `xml:"href,attr"`
}

type rssItem struct {
	Title          string       `xml:"title"`
	GUID           rssGUID      `xml:"guid"`
	PubDate        string       `xml:"pubDate"`
	Enclosure      rssEnclosure `xml:"enclosure"`
	ItunesDuration string       `xml:"itunes:duration,omitempty"`
	ItunesSeason   int          `xml:"itunes:season,omitempty"`
	ItunesEpisode  int          `xml:"itunes:episode,omitempty"`
}

type rssGUID struct {
	Value       string `xml:",chardata"`
	IsPermaLink string `xml:"isPermaLink,attr"`
}

type rssEnclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

// BuildFeedXML renders the category feed. Serial feeds are ordered by track
// number with synthetic pubDates; episodic feeds by publish time, newest first.
func BuildFeedXML(cfg FeedConfig, episodes []Episode) ([]byte, error) {
	eps := make([]Episode, len(episodes))
	copy(eps, episodes)

	serial := cfg.Type == "serial"
	if serial {
		sortSerial(eps)
	} else {
		sort.Slice(eps, func(i, j int) bool { return eps[i].PublishedAt.After(eps[j].PublishedAt) })
	}

	ch := rssChannel{
		Title:        cfg.Title,
		Description:  cfg.Description,
		Language:     cfg.Language,
		ItunesAuthor: cfg.Author,
		ItunesType:   cfg.Type,
	}
	if cfg.Cover != "" {
		ch.ItunesImage = &rssImage{Href: cfg.Cover}
	}

	for i, ep := range eps {
		pubDate := ep.PublishedAt
		if serial {
			pubDate = serialEpoch.Add(time.Duration(i) * time.Minute)
		}
		mime := ep.MimeType
		if mime == "" {
			mime = "audio/mpeg"
		}
		item := rssItem{
			Title:   stripEpisodePrefix(ep.Title, cfg.EpisodeStrip),
			GUID:    rssGUID{Value: ep.GUID(), IsPermaLink: "false"},
			PubDate: pubDate.Format(time.RFC1123Z),
			Enclosure: rssEnclosure{
				URL:    ep.PublicURL,
				Length: ep.SizeBytes,
				Type:   mime,
			},
		}
		if ep.DurationSec > 0 {
			item.ItunesDuration = formatFeedDuration(ep.DurationSec)
		}
		if serial {
			item.ItunesEpisode = i + 1
			if ep.Season > 0 {
				item.ItunesSeason = ep.Season
			}
		}
		ch.Items = append(ch.Items, item)
	}

	out, err := xml.MarshalIndent(rssXML{
		Version:  "2.0",
		ItunesNS: "http://www.itunes.com/dtds/podcast-1.0.dtd",
		Channel:  ch,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal feed: %w", err)
	}
	return append([]byte(xml.Header), out...), nil
}

// formatFeedDuration renders seconds as HH:MM:SS
func formatFeedDuration(sec int) string {
	return fmt.Sprintf("%02d:%02d:%02d", sec/3600, (sec%3600)/60, sec%60)
}

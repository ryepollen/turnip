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
	return cfg
}

// Episode is one published audio file (state lives in a per-category JSON)
type Episode struct {
	File        string    `json:"file"` // original filename, dedup key
	Title       string    `json:"title"`
	Order       int       `json:"order"` // NN prefix for serial feeds, 0 if none
	R2Key       string    `json:"r2_key"`
	PublicURL   string    `json:"public_url"`
	SizeBytes   int64     `json:"size_bytes"`
	DurationSec int       `json:"duration_sec"`
	PublishedAt time.Time `json:"published_at"`
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
		sort.Slice(eps, func(i, j int) bool {
			if eps[i].Order != eps[j].Order {
				return eps[i].Order < eps[j].Order
			}
			return eps[i].File < eps[j].File
		})
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
		item := rssItem{
			Title:   ep.Title,
			GUID:    rssGUID{Value: ep.GUID(), IsPermaLink: "false"},
			PubDate: pubDate.Format(time.RFC1123Z),
			Enclosure: rssEnclosure{
				URL:    ep.PublicURL,
				Length: ep.SizeBytes,
				Type:   "audio/mpeg",
			},
		}
		if ep.DurationSec > 0 {
			item.ItunesDuration = formatFeedDuration(ep.DurationSec)
		}
		if serial {
			item.ItunesEpisode = i + 1
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

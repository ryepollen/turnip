package proc

import (
	"context"
	"crypto/sha1" //nolint:gosec // not used for security, just a dedup key
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ReadMeta is the YAML frontmatter of a reading-layer article file. Distinct
// from NoteMeta (audio transcripts): a reading artifact has a reading time, a
// site and an author, and never carries processing levels.
type ReadMeta struct {
	Title      string   `yaml:"title"`
	SourceURL  string   `yaml:"source_url"`
	Site       string   `yaml:"site,omitempty"`
	Author     string   `yaml:"author,omitempty"`
	DateAdded  string   `yaml:"date_added"` // YYYY-MM-DD
	ReadingMin int      `yaml:"reading_min,omitempty"`
	Lang       string   `yaml:"lang,omitempty"`
	Tags       []string `yaml:"tags"`
}

// ReadResult is what a finished Save reports back to the bot
type ReadResult struct {
	SourceID   string
	MDPath     string
	HTMLPath   string
	Title      string
	Meta       ReadMeta
	ReadingMin int
	WordCount  int
	Reused     bool // artifact already existed on disk
}

// ReadService is the reading layer: it turns an article URL into a structural
// Markdown file (headings/lists/links/quotes preserved) plus an archived copy
// of the raw HTML, kept under Location/read. It is independent of the notes
// pipeline — it works without Groq/Whisper (LLM tags degrade gracefully).
type ReadService struct {
	Location  string            // directory for {id}.md and {id}.html
	Extractor *ArticleExtractor // structural extraction (readability → markdown)
	Enricher  *EnrichService    // nil → no LLM tags (still saves the article)
}

// NewReadService builds a reading-layer service. enricher may be nil.
func NewReadService(location string, extractor *ArticleExtractor, enricher *EnrichService) *ReadService {
	return &ReadService{Location: location, Extractor: extractor, Enricher: enricher}
}

// mdPath / htmlPath are the artifact paths for a source id
func (s *ReadService) mdPath(id string) string   { return filepath.Join(s.Location, id+".md") }
func (s *ReadService) htmlPath(id string) string { return filepath.Join(s.Location, id+".html") }

// readSourceID is the dedup key for a reading URL (short hash, distinct
// namespace from noteSourceID so a page saved to both layers doesn't collide)
func readSourceID(rawURL string) string {
	h := sha1.New() //nolint:gosec // not used for security
	h.Write([]byte("read::" + rawURL))
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// Save extracts the article and writes the reading artifact. Idempotent by
// source id: an already-saved URL is returned as-is (Reused=true) without a
// second fetch.
func (s *ReadService) Save(ctx context.Context, rawURL string) (ReadResult, error) {
	id := readSourceID(rawURL)
	mdPath := s.mdPath(id)
	if meta, body, err := readReadFile(mdPath); err == nil {
		return ReadResult{
			SourceID: id, MDPath: mdPath, HTMLPath: s.htmlPath(id),
			Title: meta.Title, Meta: meta, ReadingMin: meta.ReadingMin,
			WordCount: len(strings.Fields(body)), Reused: true,
		}, nil
	}

	doc, err := s.Extractor.ExtractStructured(ctx, rawURL)
	if err != nil {
		return ReadResult{}, fmt.Errorf("failed to extract article: %w", err)
	}
	title := doc.Title
	if strings.TrimSpace(title) == "" {
		title = rawURL
	}

	meta := ReadMeta{
		Title:      title,
		SourceURL:  rawURL,
		Site:       doc.SiteName,
		Author:     doc.Author,
		DateAdded:  time.Now().Format("2006-01-02"),
		ReadingMin: doc.ReadingMin,
	}
	// best-effort LLM enrichment: tags + language. A failure (no key, rate
	// limit, bad JSON) must not lose the article — we just save without tags.
	if s.Enricher != nil {
		if tags, terr := s.Enricher.ExtractMeta(ctx, title, doc.SiteName, doc.MD); terr == nil {
			meta.Tags = tags.Tags
			meta.Lang = tags.Lang
		} else {
			log.Printf("[WARN] read: meta extraction failed for %s: %v", rawURL, terr)
		}
	}

	if doc.RawHTML != "" {
		if werr := writeAtomic(s.htmlPath(id), []byte(doc.RawHTML)); werr != nil {
			log.Printf("[WARN] read: failed to archive raw html for %s: %v", rawURL, werr)
		}
	}
	if werr := writeReadFile(mdPath, meta, readBody(meta, doc.MD)); werr != nil {
		return ReadResult{}, werr
	}

	return ReadResult{
		SourceID: id, MDPath: mdPath, HTMLPath: s.htmlPath(id),
		Title: title, Meta: meta, ReadingMin: doc.ReadingMin,
		WordCount: len(strings.Fields(doc.MD)),
	}, nil
}

// readBody prepends a visible title and source link so a standalone .md opened
// in an editor is self-describing; the frontmatter carries the same facts
func readBody(meta ReadMeta, md string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", meta.Title)
	if meta.Site != "" {
		fmt.Fprintf(&b, "[Источник](%s) · %s\n\n", meta.SourceURL, meta.Site)
	} else {
		fmt.Fprintf(&b, "[Источник](%s)\n\n", meta.SourceURL)
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(md))
	return b.String()
}

// ReadItem is one saved article on disk, newest first in List
type ReadItem struct {
	SourceID string
	Path     string
	Meta     ReadMeta
	ModTime  time.Time
}

// List reads every reading artifact's frontmatter, newest first
func (s *ReadService) List() ([]ReadItem, error) {
	files, err := filepath.Glob(filepath.Join(s.Location, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("failed to list read files: %w", err)
	}
	items := make([]ReadItem, 0, len(files))
	for _, path := range files {
		fi, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		item := ReadItem{
			SourceID: strings.TrimSuffix(filepath.Base(path), ".md"),
			Path:     path,
			ModTime:  fi.ModTime(),
		}
		if meta, _, rerr := readReadFile(path); rerr == nil {
			item.Meta = meta
		} else {
			item.Meta = ReadMeta{Title: item.SourceID} // unreadable frontmatter: still listed
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ModTime.After(items[j].ModTime) })
	return items, nil
}

// Delete removes an article's markdown and archived HTML
func (s *ReadService) Delete(sourceID string) error {
	if sourceID == "" || strings.ContainsAny(sourceID, "/\\") || strings.Contains(sourceID, "..") {
		return fmt.Errorf("bad source id %q", sourceID)
	}
	if err := os.Remove(s.mdPath(sourceID)); err != nil {
		return err
	}
	_ = os.Remove(s.htmlPath(sourceID)) // archive copy may be absent (jina fallback)
	return nil
}

// writeReadFile writes frontmatter + body atomically (tmp + rename)
func writeReadFile(path string, meta ReadMeta, body string) error {
	if meta.Tags == nil {
		meta.Tags = []string{}
	}
	fm, err := yaml.Marshal(&meta)
	if err != nil {
		return fmt.Errorf("failed to marshal frontmatter: %w", err)
	}
	content := "---\n" + string(fm) + "---\n\n" + strings.TrimSpace(body) + "\n"
	return writeAtomic(path, []byte(content))
}

// readReadFile parses a reading artifact into frontmatter and body
func readReadFile(path string) (ReadMeta, string, error) {
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return ReadMeta{}, "", fmt.Errorf("failed to read file: %w", err)
	}
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return ReadMeta{}, "", fmt.Errorf("no frontmatter in %s", path)
	}
	rest := content[len("---\n"):]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return ReadMeta{}, "", fmt.Errorf("unterminated frontmatter in %s", path)
	}
	var meta ReadMeta
	if err := yaml.Unmarshal([]byte(rest[:idx+1]), &meta); err != nil {
		return ReadMeta{}, "", fmt.Errorf("failed to parse frontmatter: %w", err)
	}
	body := strings.TrimPrefix(rest[idx+len("\n---"):], "\n")
	return meta, strings.TrimSpace(body), nil
}

// writeAtomic writes bytes to path via a tmp file + rename, creating the dir
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("failed to create dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil { //nolint:gosec
		return fmt.Errorf("failed to write tmp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("failed to rename tmp file: %w", err)
	}
	return nil
}

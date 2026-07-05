package proc

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ytstore "github.com/umputun/feed-master/app/youtube/store"
)

// digestsDirName is the subdirectory of MDLocation holding digest MD files
const digestsDirName = "digests"

// digestSource is one L1 file participating in a digest
type digestSource struct {
	SourceID string
	Path     string
	Meta     NoteMeta
	Body     string
}

// normalizeTag brings a user-supplied tag to the frontmatter form
func normalizeTag(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}

// digestProcessedMark is the frontmatter marker for "included in digest of tag"
func digestProcessedMark(tag string) string { return "digest:" + tag }

// TagStats counts tags across all L1 files (for bare /digest)
func (n *NotesService) TagStats() (map[string]int, error) {
	files, err := filepath.Glob(filepath.Join(n.MDLocation, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("failed to list md files: %w", err)
	}
	stats := map[string]int{}
	for _, path := range files {
		meta, _, rerr := readNoteFile(path)
		if rerr != nil {
			continue
		}
		for _, tag := range meta.Tags {
			stats[normalizeTag(tag)]++
		}
	}
	return stats, nil
}

// collectDigestSources splits L1 files with the tag into new ones and those
// already included in this tag's digest
func (n *NotesService) collectDigestSources(tag string) (newOnes, included []digestSource, err error) {
	files, err := filepath.Glob(filepath.Join(n.MDLocation, "*.md"))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list md files: %w", err)
	}
	for _, path := range files {
		meta, body, rerr := readNoteFile(path)
		if rerr != nil {
			continue
		}
		hasTag := false
		for _, t := range meta.Tags {
			if normalizeTag(t) == tag {
				hasTag = true
				break
			}
		}
		if !hasTag {
			continue
		}
		src := digestSource{
			SourceID: strings.TrimSuffix(filepath.Base(path), ".md"),
			Path:     path,
			Meta:     meta,
			Body:     body,
		}
		if meta.hasProcessed(digestProcessedMark(tag)) {
			included = append(included, src)
		} else {
			newOnes = append(newOnes, src)
		}
	}
	sort.Slice(newOnes, func(i, j int) bool { return newOnes[i].Meta.Date < newOnes[j].Meta.Date })
	sort.Slice(included, func(i, j int) bool { return included[i].Meta.Date < included[j].Meta.Date })
	return newOnes, included, nil
}

// digestMDPath is the local canonical copy of a tag's digest
func (n *NotesService) digestMDPath(tag string) string {
	return filepath.Join(n.MDLocation, digestsDirName, tag+".md")
}

// ExistingDigestURL returns the Notion page URL of the current digest for tag
func (n *NotesService) ExistingDigestURL(tag string) string {
	if n.Notion == nil {
		return ""
	}
	return n.Notion.ExistingDigestURL(tag)
}

// processDigest rebuilds the thematic digest for job.URL (the tag).
// Incremental: only new (not yet included) L1 files are summarized; the
// previous digest text is fed to the LLM alongside, so a rebuild costs
// tokens proportional to the new material, not the whole corpus.
func (n *NotesService) processDigest(ctx context.Context, job ytstore.NotesJobRecord) (NotesResult, error) {
	if n.Notion == nil {
		return NotesResult{}, fmt.Errorf("для /digest нужен Notion (NOTION_TOKEN + notion_parent_page)")
	}
	tag := normalizeTag(job.URL)
	title := "Дайджест: " + tag

	newOnes, included, err := n.collectDigestSources(tag)
	if err != nil {
		return NotesResult{}, err
	}
	if len(newOnes) == 0 && len(included) == 0 {
		return NotesResult{}, fmt.Errorf("нет транскриптов с тегом «%s»", tag)
	}
	if len(newOnes) == 0 {
		return NotesResult{
			MDPath:        n.digestMDPath(tag),
			NotionPageURL: n.Notion.ExistingDigestURL(tag),
			Title:         title,
			Reused:        true,
		}, nil
	}

	// previous digest text rides along for the incremental rebuild
	var parts []string
	if _, prevBody, perr := readNoteFile(n.digestMDPath(tag)); perr == nil && prevBody != "" {
		parts = append(parts, "### Предыдущая версия конспекта\n\n"+prevBody)
	}

	for i, src := range newOnes {
		n.progress(job, fmt.Sprintf("📚 саммари эпизодов %d/%d", i+1, len(newOnes)))
		summary, serr := n.Enricher.Summarize(ctx, src.Body)
		if serr != nil {
			return NotesResult{}, fmt.Errorf("failed to summarize %s: %w", src.SourceID, serr)
		}
		parts = append(parts, fmt.Sprintf("### Эпизод: %s (%s)\n\n%s", src.Meta.Title, src.Meta.Date, summary))
	}

	n.progress(job, "🧵 собираю дайджест")
	digestText, err := n.Enricher.SynthesizeDigest(ctx, tag, strings.Join(parts, "\n\n"))
	if err != nil {
		return NotesResult{}, fmt.Errorf("failed to synthesize digest: %w", err)
	}

	// local canonical copy first: if Notion fails, the work is not lost
	mdPath := n.digestMDPath(tag)
	meta := NoteMeta{
		Title:  title,
		Source: "digest",
		Date:   time.Now().Format("2006-01-02"),
		Lang:   "ru",
		Tags:   []string{tag},
	}
	if err := writeNoteFile(mdPath, meta, digestText); err != nil {
		return NotesResult{}, err
	}
	res := NotesResult{MDPath: mdPath, Title: title}

	n.progress(job, "📓 пишу в Notion")
	episodePages := n.episodePageIDs(append(append([]digestSource{}, included...), newOnes...))
	pageURL, err := n.Notion.WriteDigest(ctx, tag, title, digestText, episodePages)
	if err != nil {
		return res, fmt.Errorf("failed to write digest to notion: %w", err)
	}
	res.NotionPageURL = pageURL

	// mark the new sources as included only after everything succeeded
	for _, src := range newOnes {
		src.Meta.addProcessed(digestProcessedMark(tag))
		if werr := writeNoteFile(src.Path, src.Meta, src.Body); werr != nil {
			log.Printf("[WARN] failed to mark %s as digested: %v", src.Path, werr)
		}
	}
	return res, nil
}

// episodePageIDs maps digest sources to their Notion episode pages (when the
// episode went through /notes); sources without a page are simply not linked
func (n *NotesService) episodePageIDs(sources []digestSource) []string {
	var ids []string
	seen := map[string]bool{}
	for _, src := range sources {
		id := n.Notion.EpisodePageID(src.SourceID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// DigestStatus reports how many sources a tag has and how many are new
func (n *NotesService) DigestStatus(tag string) (total, fresh int, err error) {
	newOnes, included, err := n.collectDigestSources(normalizeTag(tag))
	if err != nil {
		return 0, 0, err
	}
	return len(newOnes) + len(included), len(newOnes), nil
}

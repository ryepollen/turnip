package proc

import (
	"context"
	"crypto/sha1" //nolint:gosec // not used for security
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	ytfeed "github.com/umputun/feed-master/app/youtube/feed"
)

// notesQueueSize is the capacity of the notes job queue
const notesQueueSize = 16

// NoteMeta is the YAML frontmatter of an L1 markdown transcript file
type NoteMeta struct {
	Title       string   `yaml:"title"`
	Source      string   `yaml:"source"` // "youtube" | "article" | "rss" (rss reserved)
	URL         string   `yaml:"url"`
	Channel     string   `yaml:"channel,omitempty"`
	Date        string   `yaml:"date"` // YYYY-MM-DD
	DurationMin int      `yaml:"duration_min,omitempty"`
	Lang        string   `yaml:"lang"`
	Tags        []string `yaml:"tags"`
	Processed   []string `yaml:"processed"` // levels already run: "md", "notes"
}

// hasProcessed reports whether level is already recorded in Processed
func (m *NoteMeta) hasProcessed(level string) bool {
	for _, p := range m.Processed {
		if p == level {
			return true
		}
	}
	return false
}

// addProcessed appends level to Processed if not present
func (m *NoteMeta) addProcessed(level string) {
	if !m.hasProcessed(level) {
		m.Processed = append(m.Processed, level)
	}
}

// writeNoteFile writes frontmatter + body atomically (tmp file + rename)
func writeNoteFile(path string, meta NoteMeta, body string) error {
	if meta.Tags == nil {
		meta.Tags = []string{}
	}
	if meta.Processed == nil {
		meta.Processed = []string{}
	}
	fm, err := yaml.Marshal(&meta)
	if err != nil {
		return fmt.Errorf("failed to marshal frontmatter: %w", err)
	}
	content := "---\n" + string(fm) + "---\n\n" + strings.TrimSpace(body) + "\n"

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("failed to create md dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o640); err != nil { //nolint:gosec
		return fmt.Errorf("failed to write tmp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("failed to rename tmp file: %w", err)
	}
	return nil
}

// readNoteFile parses an L1 markdown file into frontmatter and body
func readNoteFile(path string) (NoteMeta, string, error) {
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return NoteMeta{}, "", fmt.Errorf("failed to read note file: %w", err)
	}
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return NoteMeta{}, "", fmt.Errorf("no frontmatter in %s", path)
	}
	rest := content[len("---\n"):]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return NoteMeta{}, "", fmt.Errorf("unterminated frontmatter in %s", path)
	}
	var meta NoteMeta
	if err := yaml.Unmarshal([]byte(rest[:idx+1]), &meta); err != nil {
		return NoteMeta{}, "", fmt.Errorf("failed to parse frontmatter: %w", err)
	}
	body := rest[idx+len("\n---"):]
	body = strings.TrimPrefix(body, "\n")
	return meta, strings.TrimSpace(body), nil
}

// noteSourceID derives the dedup key for a link: YouTube video ID, or a short
// hash of the URL for everything else
func noteSourceID(rawURL string) (id, source string) {
	if vid := extractVideoID(normalizeYouTubeURL(rawURL)); vid != "" {
		return vid, "youtube"
	}
	h := sha1.New() //nolint:gosec // not used for security
	h.Write([]byte("note::" + rawURL))
	return fmt.Sprintf("%x", h.Sum(nil))[:16], "article"
}

// seedMeta is source metadata gathered before the pipeline runs (yt-dlp info or URL)
type seedMeta struct {
	Title       string
	Channel     string
	Date        string // YYYY-MM-DD
	DurationMin int
}

// NotesResult is what a finished job reports back
type NotesResult struct {
	MDPath        string
	NotionPageURL string
	Title         string
	Meta          NoteMeta
	WordCount     int
	Reused        bool // transcript came from an existing L1 file
}

// NotesJob is one queued transcription/notes task
type NotesJob struct {
	URL      string
	SourceID string
	Source   string // "youtube" | "article"
	Level    string // "md" | "notes"

	ReuseAudio string                                      // pre-downloaded mp3 to reuse ("" = download)
	SeedInfo   func(ctx context.Context) (seedMeta, error) // lazy metadata fetch, runs in the worker

	Progress func(stage string)
	Done     func(res NotesResult, err error)
}

// queue errors, user-visible via bot messages
var (
	errAlreadyQueued = fmt.Errorf("задача уже в очереди")
	errQueueFull     = fmt.Errorf("очередь конспектов переполнена")
)

// NotesService runs the transcription pipeline: audio -> Groq Whisper -> LLM
// cleanup -> L1 markdown file, and optionally -> summary+references -> Notion (L2).
// Jobs are processed by a small worker pool independent from the listen/voiceover
// flow. The queue is in-memory: jobs are lost on restart, which is acceptable
// because finished L1 files make re-submission cheap.
type NotesService struct {
	MDLocation  string
	Transcriber *TranscribeService
	Enricher    *EnrichService
	Notion      *NotionWriter      // nil -> /notes degrades to /md
	Downloader  *ytfeed.Downloader // separate instance, destination is a tmp dir
	SubtitleSvc *SubtitleService   // fallback transcript source
	Extractor   *ArticleExtractor  // L1 source for non-YouTube links
	Concurrency int

	jobs     chan NotesJob
	inflight map[string]struct{}
	mu       sync.Mutex
}

// NotesParams collects NotesService dependencies
type NotesParams struct {
	MDLocation  string
	Transcriber *TranscribeService
	Enricher    *EnrichService
	Notion      *NotionWriter
	Downloader  *ytfeed.Downloader
	SubtitleSvc *SubtitleService
	Extractor   *ArticleExtractor
	Concurrency int
}

// NewNotesService creates the notes pipeline service
func NewNotesService(params NotesParams) *NotesService {
	concurrency := params.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	return &NotesService{
		MDLocation:  params.MDLocation,
		Transcriber: params.Transcriber,
		Enricher:    params.Enricher,
		Notion:      params.Notion,
		Downloader:  params.Downloader,
		SubtitleSvc: params.SubtitleSvc,
		Extractor:   params.Extractor,
		Concurrency: concurrency,
		jobs:        make(chan NotesJob, notesQueueSize),
		inflight:    map[string]struct{}{},
	}
}

// NotionEnabled reports whether L2 (Notion) is configured
func (n *NotesService) NotionEnabled() bool { return n != nil && n.Notion != nil }

// Enqueue adds a job to the queue, rejecting duplicates and overflow
func (n *NotesService) Enqueue(job NotesJob) error {
	n.mu.Lock()
	if _, ok := n.inflight[job.SourceID]; ok {
		n.mu.Unlock()
		return errAlreadyQueued
	}
	n.inflight[job.SourceID] = struct{}{}
	n.mu.Unlock()

	select {
	case n.jobs <- job:
		return nil
	default:
		n.mu.Lock()
		delete(n.inflight, job.SourceID)
		n.mu.Unlock()
		return errQueueFull
	}
}

// Run starts the worker pool and blocks until ctx is done
func (n *NotesService) Run(ctx context.Context) {
	log.Printf("[INFO] starting notes service, workers: %d, md location: %s", n.Concurrency, n.MDLocation)
	var wg sync.WaitGroup
	for i := 0; i < n.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-n.jobs:
					n.handle(ctx, job)
				}
			}
		}()
	}
	wg.Wait()
}

// handle runs one job and reports the result
func (n *NotesService) handle(ctx context.Context, job NotesJob) {
	defer func() {
		n.mu.Lock()
		delete(n.inflight, job.SourceID)
		n.mu.Unlock()
	}()

	res, err := n.process(ctx, job)
	if err != nil {
		log.Printf("[ERROR] notes job failed for %s: %v", job.URL, err)
	} else {
		log.Printf("[INFO] notes job done for %s: %s", job.URL, res.MDPath)
	}
	if job.Done != nil {
		job.Done(res, err)
	}
}

// progress calls the job progress callback if set
func (job *NotesJob) progress(stage string) {
	if job.Progress != nil {
		job.Progress(stage)
	}
}

// process executes the pipeline for one job
func (n *NotesService) process(ctx context.Context, job NotesJob) (NotesResult, error) {
	mdPath := filepath.Join(n.MDLocation, job.SourceID+".md")

	meta, body, reused, err := n.ensureL1(ctx, &job, mdPath)
	if err != nil {
		return NotesResult{}, err
	}

	res := NotesResult{
		MDPath:    mdPath,
		Title:     meta.Title,
		Meta:      meta,
		WordCount: len(strings.Fields(body)),
		Reused:    reused,
	}
	if job.Level != "notes" {
		return res, nil
	}

	if n.Notion == nil {
		return res, fmt.Errorf("notion не настроен (NOTION_TOKEN + notion_parent_page), сохранён только транскрипт %s", filepath.Base(mdPath))
	}

	job.progress("📚 конспектирую")
	summary, err := n.Enricher.Summarize(ctx, body)
	if err != nil {
		return res, fmt.Errorf("failed to summarize: %w", err)
	}
	refs, err := n.Enricher.ExtractReferences(ctx, body)
	if err != nil {
		log.Printf("[WARN] references extraction failed for %s, continuing without: %v", job.URL, err)
		refs = nil
	}

	job.progress("📓 пишу в Notion")
	pageURL, _, err := n.Notion.WriteEpisode(ctx, job.SourceID, EpisodeInput{
		Title:       meta.Title,
		URL:         meta.URL,
		Channel:     meta.Channel,
		Date:        meta.Date,
		DurationMin: meta.DurationMin,
		Tags:        meta.Tags,
		Summary:     summary,
		Transcript:  body,
		Refs:        refs,
	})
	if err != nil {
		return res, fmt.Errorf("failed to write to notion: %w", err)
	}
	res.NotionPageURL = pageURL

	meta.addProcessed("notes")
	if err := writeNoteFile(mdPath, meta, body); err != nil {
		log.Printf("[WARN] failed to update processed marker in %s: %v", mdPath, err)
	}
	return res, nil
}

// ensureL1 returns the L1 transcript for the job, producing it if the file
// does not exist yet
func (n *NotesService) ensureL1(ctx context.Context, job *NotesJob, mdPath string) (meta NoteMeta, body string, reused bool, err error) {
	if _, statErr := os.Stat(mdPath); statErr == nil {
		meta, body, err = readNoteFile(mdPath)
		if err == nil {
			job.progress("♻️ найден готовый транскрипт")
			return meta, body, true, nil
		}
		log.Printf("[WARN] existing note file %s unreadable, redoing: %v", mdPath, err)
	}

	seed := seedMeta{Title: job.URL}
	if job.SeedInfo != nil {
		s, seedErr := job.SeedInfo(ctx)
		if seedErr != nil {
			log.Printf("[WARN] failed to get source info for %s: %v", job.URL, seedErr)
		} else {
			seed = s
		}
	}

	var fidelityNote string
	switch job.Source {
	case "youtube":
		body, fidelityNote, err = n.transcribeYouTube(ctx, job, seed)
	default:
		body, err = n.extractArticleText(ctx, job, &seed)
	}
	if err != nil {
		return meta, "", false, err
	}

	meta = NoteMeta{
		Title:       seed.Title,
		Source:      job.Source,
		URL:         job.URL,
		Channel:     seed.Channel,
		Date:        seed.Date,
		DurationMin: seed.DurationMin,
		Lang:        DetectLanguage(body),
		Tags:        []string{},
	}
	if meta.Date == "" {
		meta.Date = time.Now().Format("2006-01-02")
	}

	if tagsMeta, metaErr := n.Enricher.ExtractMeta(ctx, meta.Title, meta.Channel, body); metaErr != nil {
		log.Printf("[WARN] tags extraction failed for %s, continuing without: %v", job.URL, metaErr)
	} else {
		meta.Tags = tagsMeta.Tags
		if tagsMeta.Lang != "" {
			meta.Lang = tagsMeta.Lang
		}
	}

	if fidelityNote != "" {
		body = fidelityNote + "\n\n" + body
	}
	meta.addProcessed("md")
	if err := writeNoteFile(mdPath, meta, body); err != nil {
		return meta, "", false, err
	}
	return meta, body, false, nil
}

// transcribeYouTube produces the cleaned transcript body for a YouTube video,
// via Whisper with a subtitles fallback. Returns an optional fidelity note.
func (n *NotesService) transcribeYouTube(ctx context.Context, job *NotesJob, seed seedMeta) (body, fidelityNote string, err error) {
	audioPath, cleanup, err := n.audioFor(ctx, job)
	if err == nil {
		defer cleanup()
		tr, trErr := n.Transcriber.Transcribe(ctx, audioPath, func(done, total int) {
			job.progress(fmt.Sprintf("🎧 транскрибирую %d/%d", done, total))
		})
		if trErr == nil {
			job.progress("🧹 чищу текст")
			body, err = n.Enricher.CleanTranscript(ctx, tr, func(done, total int) {
				if total > 1 {
					job.progress(fmt.Sprintf("🧹 чищу текст %d/%d", done, total))
				}
			})
			if err != nil {
				return "", "", err
			}
			return body, "", nil
		}
		log.Printf("[WARN] whisper transcription failed for %s, falling back to subtitles: %v", job.URL, trErr)
	} else {
		log.Printf("[WARN] no audio for %s, falling back to subtitles: %v", job.URL, err)
	}

	if n.SubtitleSvc == nil {
		return "", "", fmt.Errorf("транскрипция не удалась, а fallback на субтитры не настроен")
	}
	job.progress("📝 качаю субтитры (fallback)")
	videoURL := "https://www.youtube.com/watch?v=" + job.SourceID
	subFile, _, err := n.SubtitleSvc.DownloadSubtitles(ctx, videoURL)
	if err != nil {
		return "", "", fmt.Errorf("failed to download subtitles: %w", err)
	}
	defer n.SubtitleSvc.Cleanup(subFile)

	subText, err := n.SubtitleSvc.ParseSubtitles(subFile)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse subtitles: %w", err)
	}

	job.progress("🧹 чищу текст")
	body, err = n.Enricher.CleanPlainText(ctx, subText, func(done, total int) {
		if total > 1 {
			job.progress(fmt.Sprintf("🧹 чищу текст %d/%d", done, total))
		}
	})
	if err != nil {
		return "", "", err
	}
	return body, "> ⚠️ источник: субтитры (без таймкодов, точность ниже Whisper)", nil
}

// audioFor returns a local mp3 for the job: the pre-downloaded feed file when
// available, otherwise a fresh temp download. cleanup removes only temp files.
func (n *NotesService) audioFor(ctx context.Context, job *NotesJob) (path string, cleanup func(), err error) {
	noop := func() {}
	if job.ReuseAudio != "" {
		if fi, statErr := os.Stat(job.ReuseAudio); statErr == nil && fi.Size() > 0 {
			return job.ReuseAudio, noop, nil
		}
	}
	if n.Downloader == nil {
		return "", noop, fmt.Errorf("downloader not configured")
	}
	job.progress("⬇️ скачиваю аудио")
	file, err := n.Downloader.Get(ctx, job.SourceID, "notes_"+job.SourceID)
	if err != nil {
		return "", noop, fmt.Errorf("failed to download audio: %w", err)
	}
	return file, func() {
		if rmErr := os.Remove(file); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Printf("[WARN] failed to remove temp audio %s: %v", file, rmErr)
		}
	}, nil
}

// extractArticleText produces the L1 body for a non-YouTube link from the
// article text (no audio, no timecodes)
func (n *NotesService) extractArticleText(ctx context.Context, job *NotesJob, seed *seedMeta) (string, error) {
	if n.Extractor == nil {
		return "", fmt.Errorf("извлечение статей не настроено")
	}
	job.progress("📰 извлекаю текст статьи")
	article, err := n.Extractor.Extract(ctx, job.URL)
	if err != nil {
		return "", fmt.Errorf("failed to extract article: %w", err)
	}
	if article.Title != "" {
		seed.Title = article.Title
	}
	if seed.Channel == "" {
		seed.Channel = article.SiteName
	}
	text := strings.TrimSpace(article.TextContent)
	if text == "" {
		return "", fmt.Errorf("empty article text")
	}
	return text, nil
}

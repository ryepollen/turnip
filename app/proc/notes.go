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
	ytstore "github.com/umputun/feed-master/app/youtube/store"
)

const (
	maxQueuedNotesJobs = 100                 // soft cap on the queued backlog
	notesPollInterval  = 3 * time.Second     // worker poll period (kick channel makes pickup instant)
	notesJobsKeep      = 14 * 24 * time.Hour // done/failed records pruned after this
)

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

// queue errors, user-visible via bot messages
var (
	errAlreadyQueued = fmt.Errorf("задача уже в очереди")
	errQueueFull     = fmt.Errorf("очередь конспектов переполнена")
)

// NotesNotifier receives job lifecycle events (implemented by the telegram bot).
// Persisted jobs cannot carry closures, so results are delivered through this
// interface using the chat/message ids stored in the job record.
type NotesNotifier interface {
	NotesJobProgress(job ytstore.NotesJobRecord, stage string)
	NotesJobDone(job ytstore.NotesJobRecord, res NotesResult)
	NotesJobFailed(job ytstore.NotesJobRecord, err error)
}

// NotesJobStore persists the job queue (implemented by ytstore.BoltDB)
type NotesJobStore interface {
	SaveNotesJob(job ytstore.NotesJobRecord) error
	ClaimNextNotesJob() (ytstore.NotesJobRecord, bool, error)
	LoadNotesJobs(status string, limit int) ([]ytstore.NotesJobRecord, error)
	CountNotesJobs(status string) (int, error)
	HasActiveNotesJob(sourceID string) (bool, error)
	ResetProcessingNotesJobs() (int, error)
	DeleteOldNotesJobs(cutoff time.Time) (int, error)
}

// NotesService runs the transcription pipeline: audio -> Groq Whisper -> LLM
// cleanup -> L1 markdown file, and optionally -> summary+references -> Notion (L2).
// The queue lives in bolt: jobs survive restarts (interrupted ones are requeued
// on startup), workers poll the store and claim jobs atomically.
type NotesService struct {
	MDLocation  string
	Transcriber *TranscribeService
	Enricher    *EnrichService
	Notion      *NotionWriter      // nil -> /notes degrades to /md
	Downloader  *ytfeed.Downloader // separate instance, destination is a tmp dir
	SubtitleSvc *SubtitleService   // fallback transcript source
	Extractor   *ArticleExtractor  // L1 source for non-YouTube links
	Concurrency int
	JobStore    NotesJobStore
	Notifier    NotesNotifier // set once before Run, nil-safe

	kick chan struct{} // wakes workers right after Enqueue, no poll latency
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
	JobStore    NotesJobStore
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
		JobStore:    params.JobStore,
		kick:        make(chan struct{}, 1),
	}
}

// NotionEnabled reports whether L2 (Notion) is configured
func (n *NotesService) NotionEnabled() bool { return n != nil && n.Notion != nil }

// Enqueue persists a new queued job, rejecting duplicates and overflow.
// The record's ID, status and timestamps are set here.
func (n *NotesService) Enqueue(rec ytstore.NotesJobRecord) error {
	if n.JobStore == nil {
		return fmt.Errorf("очередь конспектов не настроена")
	}
	active, err := n.JobStore.HasActiveNotesJob(rec.SourceID)
	if err != nil {
		return fmt.Errorf("failed to check queue: %w", err)
	}
	if active {
		return errAlreadyQueued
	}
	queued, err := n.JobStore.CountNotesJobs(ytstore.NotesJobQueued)
	if err != nil {
		return fmt.Errorf("failed to count queue: %w", err)
	}
	if queued >= maxQueuedNotesJobs {
		return errQueueFull
	}

	now := time.Now().UTC()
	rec.ID = fmt.Sprintf("%020d-%s", now.UnixNano(), rec.SourceID)
	rec.Status = ytstore.NotesJobQueued
	rec.CreatedAt, rec.UpdatedAt = now, now
	if err := n.JobStore.SaveNotesJob(rec); err != nil {
		return fmt.Errorf("failed to persist job: %w", err)
	}
	select {
	case n.kick <- struct{}{}:
	default:
	}
	return nil
}

// QueueStatus reports queue counters and the few most recent jobs (for /status)
func (n *NotesService) QueueStatus() (queued, processing int, recent []ytstore.NotesJobRecord, err error) {
	if n.JobStore == nil {
		return 0, 0, nil, fmt.Errorf("очередь конспектов не настроена")
	}
	if queued, err = n.JobStore.CountNotesJobs(ytstore.NotesJobQueued); err != nil {
		return 0, 0, nil, err
	}
	if processing, err = n.JobStore.CountNotesJobs(ytstore.NotesJobProcessing); err != nil {
		return 0, 0, nil, err
	}
	recent, err = n.JobStore.LoadNotesJobs("", 5)
	return queued, processing, recent, err
}

// Run requeues interrupted jobs, prunes old records and blocks running the
// worker pool until ctx is done
func (n *NotesService) Run(ctx context.Context) {
	if n.JobStore == nil {
		log.Printf("[ERROR] notes service has no job store, not starting")
		return
	}
	if cnt, err := n.JobStore.ResetProcessingNotesJobs(); err != nil {
		log.Printf("[WARN] failed to requeue interrupted notes jobs: %v", err)
	} else if cnt > 0 {
		log.Printf("[INFO] requeued %d interrupted notes jobs", cnt)
	}
	if cnt, err := n.JobStore.DeleteOldNotesJobs(time.Now().Add(-notesJobsKeep)); err != nil {
		log.Printf("[WARN] failed to prune old notes jobs: %v", err)
	} else if cnt > 0 {
		log.Printf("[INFO] pruned %d old notes jobs", cnt)
	}

	log.Printf("[INFO] starting notes service, workers: %d, md location: %s", n.Concurrency, n.MDLocation)
	var wg sync.WaitGroup
	for i := 0; i < n.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(notesPollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				case <-n.kick:
				}
				n.drainQueue(ctx)
			}
		}()
	}
	wg.Wait()
}

// drainQueue claims and runs jobs until the queue is empty
func (n *NotesService) drainQueue(ctx context.Context) {
	for ctx.Err() == nil {
		job, ok, err := n.JobStore.ClaimNextNotesJob()
		if err != nil {
			log.Printf("[WARN] failed to claim notes job: %v", err)
			return
		}
		if !ok {
			return
		}
		n.runJob(ctx, job)
	}
}

// runJob processes one claimed job and stores the final status
func (n *NotesService) runJob(ctx context.Context, job ytstore.NotesJobRecord) {
	res, err := n.process(ctx, job)
	job.UpdatedAt = time.Now().UTC()
	if err != nil {
		log.Printf("[ERROR] notes job failed for %s: %v", job.URL, err)
		job.Status, job.Error = ytstore.NotesJobFailed, err.Error()
		if n.Notifier != nil {
			n.Notifier.NotesJobFailed(job, err)
		}
	} else {
		log.Printf("[INFO] notes job done for %s: %s", job.URL, res.MDPath)
		job.Status, job.Error = ytstore.NotesJobDone, ""
		if n.Notifier != nil {
			n.Notifier.NotesJobDone(job, res)
		}
	}
	if serr := n.JobStore.SaveNotesJob(job); serr != nil {
		log.Printf("[WARN] failed to store notes job result %s: %v", job.ID, serr)
	}
}

// progress forwards a stage update to the notifier if set
func (n *NotesService) progress(job ytstore.NotesJobRecord, stage string) {
	if n.Notifier != nil {
		n.Notifier.NotesJobProgress(job, stage)
	}
}

// seedFor gathers source metadata before the pipeline runs
func (n *NotesService) seedFor(ctx context.Context, job ytstore.NotesJobRecord) seedMeta {
	seed := seedMeta{Title: job.URL}
	if job.Source != "youtube" || n.Downloader == nil {
		return seed
	}
	info, err := n.Downloader.GetInfo(ctx, "https://www.youtube.com/watch?v="+job.SourceID)
	if err != nil {
		log.Printf("[WARN] failed to get source info for %s: %v", job.URL, err)
		return seed
	}
	seed = seedMeta{Title: info.Title, Channel: info.Uploader, DurationMin: int(info.Duration) / 60}
	if parsed, perr := time.Parse("20060102", info.UploadDate); perr == nil {
		seed.Date = parsed.Format("2006-01-02")
	}
	return seed
}

// process executes the pipeline for one job
func (n *NotesService) process(ctx context.Context, job ytstore.NotesJobRecord) (NotesResult, error) {
	mdPath := filepath.Join(n.MDLocation, job.SourceID+".md")

	meta, body, reused, err := n.ensureL1(ctx, job, mdPath)
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

	n.progress(job, "📚 конспектирую")
	summary, err := n.Enricher.Summarize(ctx, body)
	if err != nil {
		return res, fmt.Errorf("failed to summarize: %w", err)
	}
	refs, err := n.Enricher.ExtractReferences(ctx, body)
	if err != nil {
		log.Printf("[WARN] references extraction failed for %s, continuing without: %v", job.URL, err)
		refs = nil
	}

	n.progress(job, "📓 пишу в Notion")
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
func (n *NotesService) ensureL1(ctx context.Context, job ytstore.NotesJobRecord, mdPath string) (meta NoteMeta, body string, reused bool, err error) {
	if _, statErr := os.Stat(mdPath); statErr == nil {
		meta, body, err = readNoteFile(mdPath)
		if err == nil {
			n.progress(job, "♻️ найден готовый транскрипт")
			return meta, body, true, nil
		}
		log.Printf("[WARN] existing note file %s unreadable, redoing: %v", mdPath, err)
	}

	seed := n.seedFor(ctx, job)

	var fidelityNote string
	switch job.Source {
	case "youtube":
		body, fidelityNote, err = n.transcribeYouTube(ctx, job)
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
func (n *NotesService) transcribeYouTube(ctx context.Context, job ytstore.NotesJobRecord) (body, fidelityNote string, err error) {
	audioPath, cleanup, err := n.audioFor(ctx, job)
	if err == nil {
		defer cleanup()
		tr, trErr := n.Transcriber.Transcribe(ctx, audioPath, func(done, total int) {
			n.progress(job, fmt.Sprintf("🎧 транскрибирую %d/%d", done, total))
		})
		if trErr == nil {
			n.progress(job, "🧹 чищу текст")
			body, err = n.Enricher.CleanTranscript(ctx, tr, func(done, total int) {
				if total > 1 {
					n.progress(job, fmt.Sprintf("🧹 чищу текст %d/%d", done, total))
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
	n.progress(job, "📝 качаю субтитры (fallback)")
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

	n.progress(job, "🧹 чищу текст")
	body, err = n.Enricher.CleanPlainText(ctx, subText, func(done, total int) {
		if total > 1 {
			n.progress(job, fmt.Sprintf("🧹 чищу текст %d/%d", done, total))
		}
	})
	if err != nil {
		return "", "", err
	}
	return body, "> ⚠️ источник: субтитры (без таймкодов, точность ниже Whisper)", nil
}

// audioFor returns a local mp3 for the job: the pre-downloaded feed file when
// available, otherwise a fresh temp download. cleanup removes only temp files.
func (n *NotesService) audioFor(ctx context.Context, job ytstore.NotesJobRecord) (path string, cleanup func(), err error) {
	noop := func() {}
	if job.ReuseAudio != "" {
		if fi, statErr := os.Stat(job.ReuseAudio); statErr == nil && fi.Size() > 0 {
			return job.ReuseAudio, noop, nil
		}
	}
	if n.Downloader == nil {
		return "", noop, fmt.Errorf("downloader not configured")
	}
	n.progress(job, "⬇️ скачиваю аудио")
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
func (n *NotesService) extractArticleText(ctx context.Context, job ytstore.NotesJobRecord, seed *seedMeta) (string, error) {
	if n.Extractor == nil {
		return "", fmt.Errorf("извлечение статей не настроено")
	}
	n.progress(job, "📰 извлекаю текст статьи")
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

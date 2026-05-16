package proc

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"html/template"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	log "github.com/go-pkgz/lgr"
	tb "gopkg.in/tucnak/telebot.v2"

	ytfeed "github.com/umputun/feed-master/app/youtube/feed"
	ytstore "github.com/umputun/feed-master/app/youtube/store"
)

// TelegramBot handles incoming messages for YouTube video additions and article TTS
type TelegramBot struct {
	Bot              *tb.Bot
	AllowedUserID    int64
	FeedName         string
	FeedTitle        string
	MaxItems         int
	Downloader       *ytfeed.Downloader
	Store            *ytstore.BoltDB
	DurationSvc      DurationService
	FilesLocation    string
	BaseURL          string
	TTSEnabled       bool
	TTS              TTSProvider
	ArticleExtractor *ArticleExtractor
	VoiceoverSvc     *VoiceoverService
	SubtitleSvc      *SubtitleService
	Translator       *Translator

	pendingMu      sync.Mutex
	pendingActions map[string]*pendingAction
}

// pendingAction stores the URL(s) extracted from a user message while the user
// picks an action from the inline menu. Callback data is limited to 64 bytes,
// so the menu carries only a short token referencing this entry.
type pendingAction struct {
	kind        string // "yt" or "article"
	videoIDs    []string
	url         string
	originalMsg *tb.Message
	created     time.Time
}

const (
	defaultListPageSize    = 10
	defaultHistoryPageSize = 10
	maxPageSize            = 50
	pendingActionTTL       = 10 * time.Minute
)

// TelegramBotParams contains all parameters for creating a new TelegramBot
type TelegramBotParams struct {
	Token         string
	APIURL        string
	AllowedUserID int64
	FeedName      string
	FeedTitle     string
	MaxItems      int
	Downloader    *ytfeed.Downloader
	Store         *ytstore.BoltDB
	DurationSvc   DurationService
	FilesLocation string
	BaseURL       string
	TTSEnabled    bool
	TTSVoice      string
	CookiesFile   string
}

// NewTelegramBot creates a new bot for receiving YouTube URLs
func NewTelegramBot(params TelegramBotParams) (*TelegramBot, error) {
	if params.Token == "" {
		return nil, fmt.Errorf("telegram token required")
	}

	apiURL := params.APIURL
	if apiURL == "" {
		apiURL = "https://api.telegram.org"
	}

	bot, err := tb.NewBot(tb.Settings{
		URL:    apiURL,
		Token:  params.Token,
		Poller: &tb.LongPoller{Timeout: 30 * time.Second},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create bot: %w", err)
	}

	tb := &TelegramBot{
		Bot:            bot,
		AllowedUserID:  params.AllowedUserID,
		FeedName:       params.FeedName,
		FeedTitle:      params.FeedTitle,
		MaxItems:       params.MaxItems,
		Downloader:     params.Downloader,
		Store:          params.Store,
		DurationSvc:    params.DurationSvc,
		FilesLocation:  params.FilesLocation,
		BaseURL:        params.BaseURL,
		TTSEnabled:     params.TTSEnabled,
		pendingActions: make(map[string]*pendingAction),
	}

	// Initialize TTS if enabled
	if params.TTSEnabled {
		tb.TTS = NewEdgeTTS(params.TTSVoice)
		tb.ArticleExtractor = NewArticleExtractor()
	}

	// Initialize voiceover service (for YouTube voice-over translation)
	tb.VoiceoverSvc = NewVoiceoverService(params.FilesLocation, "ru", params.CookiesFile)

	// Initialize subtitle service and translator (for long video fallback)
	tb.SubtitleSvc = NewSubtitleService(params.FilesLocation, params.CookiesFile)
	tb.Translator = NewTranslatorWithKey(os.Getenv("YANDEX_TRANSLATE_KEY"), os.Getenv("YANDEX_FOLDER_ID"), "ru")

	return tb, nil
}

// Run starts the bot and listens for messages
func (t *TelegramBot) Run(ctx context.Context) error {
	log.Printf("[INFO] starting telegram bot for user %d, feed: %s", t.AllowedUserID, t.FeedName)

	// Register handlers
	t.Bot.Handle(tb.OnText, t.handleText)
	t.Bot.Handle("/list", t.handleList)
	t.Bot.Handle("/history", t.handleHistory)
	t.Bot.Handle("/del", t.handleDelete)
	t.Bot.Handle("/vo", t.handleVoiceover)
	t.Bot.Handle("/help", t.handleHelp)
	t.Bot.Handle("/start", t.handleHelp)

	// Callback handler for pagination and delete actions
	t.Bot.Handle(tb.OnCallback, t.handleCallback)

	// Start polling in goroutine
	go t.Bot.Start()

	// Periodically drop stale pending menu entries
	go t.gcPendingActions(ctx)

	// Wait for context cancellation
	<-ctx.Done()
	t.Bot.Stop()
	log.Printf("[INFO] telegram bot stopped")
	return ctx.Err()
}

func (t *TelegramBot) gcPendingActions(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			t.pendingMu.Lock()
			for k, pa := range t.pendingActions {
				if now.Sub(pa.created) > pendingActionTTL {
					delete(t.pendingActions, k)
				}
			}
			t.pendingMu.Unlock()
		}
	}
}

func (t *TelegramBot) storePendingAction(pa *pendingAction) string {
	pa.created = time.Now()
	var buf [6]byte
	_, _ = rand.Read(buf[:])
	token := hex.EncodeToString(buf[:])
	t.pendingMu.Lock()
	t.pendingActions[token] = pa
	t.pendingMu.Unlock()
	return token
}

func (t *TelegramBot) takePendingAction(token string) *pendingAction {
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()
	pa, ok := t.pendingActions[token]
	if !ok {
		return nil
	}
	delete(t.pendingActions, token)
	return pa
}

// handleText processes text messages (YouTube URLs or article URLs).
// Instead of acting on the link directly, it offers an inline menu so the user
// picks what to do (download audio, voice-over, TTS for articles, cancel).
func (t *TelegramBot) handleText(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		log.Printf("[WARN] unauthorized user %d tried to send message", m.Sender.ID)
		_, _ = t.Bot.Send(m.Chat, "Unauthorized. This bot is private.")
		return
	}

	videoIDs := t.extractAllYouTubeVideoIDs(m.Text)
	if len(videoIDs) > 0 {
		token := t.storePendingAction(&pendingAction{kind: "yt", videoIDs: videoIDs, originalMsg: m})
		var prompt string
		if len(videoIDs) == 1 {
			prompt = "🤔 Что сделать со ссылкой?"
		} else {
			prompt = fmt.Sprintf("🤔 Что сделать с %d ссылками?", len(videoIDs))
		}
		_, _ = t.Bot.Send(m.Chat, prompt, t.buildActionMenu(token, "yt"))
		return
	}

	articleURL := t.extractURL(m.Text)
	if articleURL != "" && t.TTSEnabled && IsArticleURL(articleURL) {
		token := t.storePendingAction(&pendingAction{kind: "article", url: articleURL, originalMsg: m})
		_, _ = t.Bot.Send(m.Chat, "🤔 Что сделать со ссылкой?", t.buildActionMenu(token, "article"))
		return
	}

	helpMsg := "No valid URL found. Send a link:\n• YouTube: https://youtube.com/watch?v=VIDEO_ID"
	if t.TTSEnabled {
		helpMsg += "\n• Article: any web page URL"
	}
	_, _ = t.Bot.Send(m.Chat, helpMsg)
}

// buildActionMenu builds the inline keyboard shown for an incoming link.
func (t *TelegramBot) buildActionMenu(token, kind string) *tb.ReplyMarkup {
	markup := &tb.ReplyMarkup{}
	var rows [][]tb.InlineButton
	switch kind {
	case "yt":
		btnAudio := markup.Data("🎵 Аудио", "act", token+"|audio")
		btnVO := markup.Data("🎙 Озвучка (RU)", "act", token+"|vo")
		rows = append(rows, []tb.InlineButton{*btnAudio.Inline(), *btnVO.Inline()})
	case "article":
		btnTTS := markup.Data("📝 Озвучить", "act", token+"|tts")
		rows = append(rows, []tb.InlineButton{*btnTTS.Inline()})
	}
	btnCancel := markup.Data("🚫 Отмена", "act", token+"|cancel")
	rows = append(rows, []tb.InlineButton{*btnCancel.Inline()})
	markup.InlineKeyboard = rows
	return markup
}

// handleList shows recent entries
func (t *TelegramBot) handleList(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		return
	}

	pageSize := t.parsePageSize(m.Text, defaultListPageSize)
	entries, err := t.Store.Load(t.FeedName, t.MaxItems)
	if err != nil {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("Error loading entries: %v", err))
		return
	}

	if len(entries) == 0 {
		_, _ = t.Bot.Send(m.Chat, "No videos in feed yet.")
		return
	}

	msg, markup := t.buildListMessage("list", entries, 0, pageSize)
	_, _ = t.Bot.Send(m.Chat, msg, markup)
}

// handleHistory shows history of all processed videos
func (t *TelegramBot) handleHistory(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		return
	}

	pageSize := t.parsePageSize(m.Text, defaultHistoryPageSize)
	entries, err := t.Store.Load(t.FeedName, t.MaxItems)
	if err != nil {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("Error: %v", err))
		return
	}

	if len(entries) == 0 {
		_, _ = t.Bot.Send(m.Chat, "No videos added yet.")
		return
	}

	msg, markup := t.buildListMessage("history", entries, 0, pageSize)
	_, _ = t.Bot.Send(m.Chat, msg, markup, tb.NoPreview)
}

// handleDelete removes entry from feed and deletes file from disk
func (t *TelegramBot) handleDelete(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		return
	}

	// parse argument: /del 1 or /del (without arg = delete last)
	args := regexp.MustCompile(`\s+`).Split(m.Text, -1)
	idx := 1 // default: first (most recent)
	if len(args) > 1 && args[1] != "" {
		if _, err := fmt.Sscanf(args[1], "%d", &idx); err != nil || idx < 1 {
			_, _ = t.Bot.Send(m.Chat, "Usage: /del [number]\nExample: /del 1 (delete most recent)")
			return
		}
	}

	entries, err := t.Store.Load(t.FeedName, t.MaxItems)
	if err != nil {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("Error: %v", err))
		return
	}

	if len(entries) == 0 {
		_, _ = t.Bot.Send(m.Chat, "Feed is empty.")
		return
	}

	if idx > len(entries) {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("Only %d entries in feed.", len(entries)))
		return
	}

	entry := entries[idx-1]

	if err := t.deleteEntry(entry); err != nil {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("Error removing: %v", err))
		return
	}

	// Show updated list after deletion
	updatedEntries, err := t.Store.Load(t.FeedName, 10)
	if err != nil {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("🗑 Deleted: %s\n\n(Error loading updated list: %v)", entry.Title, err))
		return
	}

	msg := fmt.Sprintf("🗑 Deleted: %s\n\n", entry.Title)
	if len(updatedEntries) == 0 {
		msg += "Feed is now empty."
	} else {
		msg += fmt.Sprintf("Remaining (%d):\n", len(updatedEntries))
		for i, e := range updatedEntries {
			dur := time.Duration(e.Duration) * time.Second
			msg += fmt.Sprintf("%d. %s (%s)\n", i+1, e.Title, t.formatDuration(dur))
		}
	}
	_, _ = t.Bot.Send(m.Chat, msg)
}

// handleHelp sends help message
func (t *TelegramBot) handleHelp(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		_, _ = t.Bot.Send(m.Chat, "Unauthorized. This bot is private.")
		return
	}

	help := fmt.Sprintf(`🎧 Turnip Podcast Bot

Send a URL to add audio to your feed:
• YouTube video → downloads audio
• Article/webpage → TTS озвучка (if enabled)

Commands:
/vo <url> - озвучка YouTube на русском
/list - recent entries in feed
/history - all entries with links
/del - delete most recent
/del N - delete entry N
/help - this help

TTS: %v
RSS: %s/yt/rss/%s`, t.TTSEnabled, t.BaseURL, t.FeedName)

	_, _ = t.Bot.Send(m.Chat, help)
}

// isAuthorized checks if user is allowed
func (t *TelegramBot) isAuthorized(user *tb.User) bool {
	if user == nil {
		return false
	}
	return int64(user.ID) == t.AllowedUserID
}

// videoResult holds the outcome of processing a single video (without Telegram UI).
type videoResult struct {
	VideoID  string
	Title    string
	Duration time.Duration
	Skipped  bool // true when video was already in feed
}

// extractYouTubeVideoID extracts video ID from YouTube URL
func (t *TelegramBot) extractYouTubeVideoID(text string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`youtube\.com/watch\?v=([a-zA-Z0-9_-]{11})`),
		regexp.MustCompile(`youtu\.be/([a-zA-Z0-9_-]{11})`),
		regexp.MustCompile(`youtube\.com/embed/([a-zA-Z0-9_-]{11})`),
		regexp.MustCompile(`youtube\.com/v/([a-zA-Z0-9_-]{11})`),
	}

	for _, re := range patterns {
		if matches := re.FindStringSubmatch(text); len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

// extractAllYouTubeVideoIDs extracts all unique YouTube video IDs from text, preserving order.
func (t *TelegramBot) extractAllYouTubeVideoIDs(text string) []string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`youtube\.com/watch\?v=([a-zA-Z0-9_-]{11})`),
		regexp.MustCompile(`youtu\.be/([a-zA-Z0-9_-]{11})`),
		regexp.MustCompile(`youtube\.com/embed/([a-zA-Z0-9_-]{11})`),
		regexp.MustCompile(`youtube\.com/v/([a-zA-Z0-9_-]{11})`),
	}

	seen := map[string]bool{}
	var ids []string
	for _, re := range patterns {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			id := m[1]
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// processVideoItem contains the core video processing logic without any Telegram UI calls.
// It downloads the video, saves it to the store, and returns the result.
func (t *TelegramBot) processVideoItem(ctx context.Context, videoID string) (*videoResult, error) {
	videoURL := "https://www.youtube.com/watch?v=" + videoID

	// 1. Fetch metadata
	info, err := t.Downloader.GetInfo(ctx, videoURL)
	if err != nil {
		return nil, fmt.Errorf("failed to get video info: %w", err)
	}

	// 2. Check if already processed
	tempEntry := ytfeed.Entry{ChannelID: t.FeedName, VideoID: videoID}
	if found, _, _ := t.Store.CheckProcessed(tempEntry); found {
		return &videoResult{VideoID: videoID, Title: info.Title, Skipped: true}, nil
	}

	// 3. Download audio
	fname := t.makeFileName(videoID)
	file, err := t.Downloader.Get(ctx, videoID, fname)
	if err != nil {
		return nil, fmt.Errorf("failed to download: %w", err)
	}

	// 4. Get duration from file
	duration := int(info.Duration)
	if t.DurationSvc != nil {
		if fileDur := t.DurationSvc.File(file); fileDur > 0 {
			duration = fileDur
		}
	}

	// 5. Create Entry
	entry := t.createEntry(info, file, duration)

	// 6. Store in BoltDB
	created, err := t.Store.Save(entry)
	if err != nil {
		return nil, fmt.Errorf("failed to save: %w", err)
	}
	if !created {
		return &videoResult{VideoID: videoID, Title: info.Title, Skipped: true}, nil
	}

	// 7. Mark as processed
	if err := t.Store.SetProcessed(entry); err != nil {
		log.Printf("[WARN] failed to mark as processed: %v", err)
	}

	// 8. Remove old entries if exceeding MaxItems
	t.removeOldEntries()

	dur := time.Duration(duration) * time.Second
	log.Printf("[INFO] added video %s: %s (duration: %s)", videoID, info.Title, dur.String())

	return &videoResult{VideoID: videoID, Title: info.Title, Duration: dur}, nil
}

// processVideo downloads and stores a YouTube video (single-video path with Telegram status messages).
func (t *TelegramBot) processVideo(ctx context.Context, chat *tb.Chat, statusMsg, originalMsg *tb.Message, videoID string) error {
	res, err := t.processVideoItem(ctx, videoID)
	if err != nil {
		return err
	}

	if res.Skipped {
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⚠️ Already in feed: %s", res.Title))
	} else {
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("✅ %s (%s)", res.Title, t.formatDuration(res.Duration)))
	}

	t.deleteMessageAfterDelay(originalMsg, 5*time.Second)
	return nil
}

// processVideoBatch processes multiple YouTube videos sequentially, updating a single status message.
func (t *TelegramBot) processVideoBatch(ctx context.Context, chat *tb.Chat, statusMsg, originalMsg *tb.Message, videoIDs []string) {
	total := len(videoIDs)
	var added, skipped, failed int
	cookieErrShown := false

	for i, id := range videoIDs {
		pos := fmt.Sprintf("%d/%d", i+1, total)
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⬇️ %s: Processing...", pos))

		res, err := t.processVideoItem(ctx, id)
		if err != nil {
			failed++
			log.Printf("[ERROR] batch %s: failed to process video %s: %v", pos, id, err)
			if ytfeed.IsCookieError(err.Error()) && !cookieErrShown {
				cookieErrShown = true
				_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⚠️ %s: cookies expired, continuing...", pos))
			}
			continue
		}

		if res.Skipped {
			skipped++
			_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⚠️ %s: %s (already in feed)", pos, res.Title))
		} else {
			added++
			_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("✅ %s: %s (%s)", pos, res.Title, t.formatDuration(res.Duration)))
		}
	}

	// Build final summary
	summary := fmt.Sprintf("✅ Added %d/%d", added, total)
	var details []string
	if skipped > 0 {
		details = append(details, fmt.Sprintf("%d already in feed", skipped))
	}
	if failed > 0 {
		details = append(details, fmt.Sprintf("%d failed", failed))
	}
	if len(details) > 0 {
		summary += " (" + strings.Join(details, ", ") + ")"
	}
	if cookieErrShown {
		summary += "\n⚠️ YouTube cookies expired. Run update-cookies.sh to fix."
	}

	_, _ = t.Bot.Edit(statusMsg, summary)
	t.deleteMessageAfterDelay(originalMsg, 5*time.Second)
}

// deleteMessageAfterDelay deletes a message after specified delay
func (t *TelegramBot) deleteMessageAfterDelay(msg *tb.Message, delay time.Duration) {
	if msg == nil {
		return
	}
	go func() {
		time.Sleep(delay)
		_ = t.Bot.Delete(msg)
	}()
}

// removeOldEntries removes entries exceeding MaxItems and deletes their files
func (t *TelegramBot) removeOldEntries() {
	if t.MaxItems <= 0 {
		return
	}

	files, err := t.Store.RemoveOld(t.FeedName, t.MaxItems)
	if err != nil {
		log.Printf("[WARN] failed to remove old entries: %v", err)
	}

	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] failed to delete old file %s: %v", f, err)
		} else {
			log.Printf("[INFO] auto-removed old file %s", f)
		}
	}
}

// formatDuration formats duration as human-readable string
func (t *TelegramBot) formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// createEntry creates ytfeed.Entry from VideoInfo
func (t *TelegramBot) createEntry(info *ytfeed.VideoInfo, file string, duration int) ytfeed.Entry {
	published := time.Now()
	// Parse upload date if available: YYYYMMDD
	if info.UploadDate != "" {
		if parsed, err := time.Parse("20060102", info.UploadDate); err == nil {
			published = parsed
		}
	}

	return ytfeed.Entry{
		ChannelID: t.FeedName,
		VideoID:   info.ID,
		Title:     info.Title,
		Link: struct {
			Href string `xml:"href,attr"`
		}{Href: info.WebpageURL},
		Published: published,
		Updated:   time.Now(),
		Media: struct {
			Description template.HTML `xml:"description"`
			Thumbnail   struct {
				URL string `xml:"url,attr"`
			} `xml:"thumbnail"`
		}{
			Description: template.HTML(info.Description),
			Thumbnail:   struct{ URL string `xml:"url,attr"` }{URL: info.Thumbnail},
		},
		Author: struct {
			Name string `xml:"name"`
			URI  string `xml:"uri"`
		}{
			Name: info.Uploader,
			URI:  info.ChannelURL,
		},
		File:     file,
		Duration: duration,
	}
}

func (t *TelegramBot) makeFileName(videoID string) string {
	h := sha1.New()
	h.Write([]byte(t.FeedName + "::" + videoID))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// extractURL extracts any URL from text
func (t *TelegramBot) extractURL(text string) string {
	re := regexp.MustCompile(`https?://[^\s<>"{}|\\^` + "`" + `\[\]]+`)
	matches := re.FindString(text)
	return matches
}

// parsePageSize extracts optional page size from command text
func (t *TelegramBot) parsePageSize(text string, def int) int {
	parts := regexp.MustCompile(`\s+`).Split(text, -1)
	if len(parts) < 2 || parts[1] == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(parts[1], "%d", &n); err != nil || n < 1 {
		return def
	}
	if n > maxPageSize {
		return maxPageSize
	}
	return n
}

// buildListMessage builds paginated list/history output and inline keyboard
func (t *TelegramBot) buildListMessage(kind string, entries []ytfeed.Entry, page, pageSize int) (string, *tb.ReplyMarkup) {
	if pageSize <= 0 {
		pageSize = defaultListPageSize
	}
	if page < 0 {
		page = 0
	}
	total := len(entries)
	pages := (total + pageSize - 1) / pageSize
	if pages == 0 {
		pages = 1
	}
	if page >= pages {
		page = pages - 1
	}

	start := page * pageSize
	end := start + pageSize
	if end > total {
		end = total
	}

	var msg string
	if kind == "history" {
		msg = fmt.Sprintf("📜 History (%d) — page %d/%d:\n\n", total, page+1, pages)
	} else {
		msg = fmt.Sprintf("Recent videos (%d) — page %d/%d:\n\n", total, page+1, pages)
	}

	for i := start; i < end; i++ {
		num := i + 1
		e := entries[i]
		if kind == "history" {
			msg += fmt.Sprintf("%d. %s\n%s\n\n", num, e.Title, e.Link.Href)
		} else {
			dur := time.Duration(e.Duration) * time.Second
			msg += fmt.Sprintf("%d. %s (%s)\n", num, e.Title, t.formatDuration(dur))
		}
	}

	markup := &tb.ReplyMarkup{}

	// Pagination buttons
	if pages > 1 {
		prevPage := page - 1
		if prevPage < 0 {
			prevPage = pages - 1
		}
		nextPage := page + 1
		if nextPage >= pages {
			nextPage = 0
		}

		btnPrev := markup.Data("◀︎", "list_page", t.packCallbackData(kind, prevPage, pageSize, ""))
		btnNext := markup.Data("▶︎", "list_page", t.packCallbackData(kind, nextPage, pageSize, ""))
		markup.InlineKeyboard = append(markup.InlineKeyboard, []tb.InlineButton{*btnPrev.Inline(), *btnNext.Inline()})
	}

	// Quick delete buttons for list view only
	if kind == "list" {
		row := []tb.InlineButton{}
		for i := start; i < end; i++ {
			num := i + 1
			entry := entries[i]
			btn := markup.Data(fmt.Sprintf("🗑 %d", num), "list_del", t.packCallbackData(kind, page, pageSize, entry.VideoID))
			row = append(row, *btn.Inline())
			if len(row) == 5 {
				markup.InlineKeyboard = append(markup.InlineKeyboard, row)
				row = []tb.InlineButton{}
			}
		}
		if len(row) > 0 {
			markup.InlineKeyboard = append(markup.InlineKeyboard, row)
		}
	}

	return msg, markup
}

func (t *TelegramBot) packCallbackData(kind string, page, pageSize int, videoID string) string {
	if videoID == "" {
		return fmt.Sprintf("k=%s|p=%d|s=%d", kind, page, pageSize)
	}
	return fmt.Sprintf("k=%s|p=%d|s=%d|v=%s", kind, page, pageSize, videoID)
}

func (t *TelegramBot) unpackCallbackData(data string) (kind string, page int, pageSize int, videoID string) {
	page = 0
	pageSize = defaultListPageSize
	parts := strings.Split(data, "|")
	for _, p := range parts {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "k":
			kind = kv[1]
		case "p":
			_, _ = fmt.Sscanf(kv[1], "%d", &page)
		case "s":
			_, _ = fmt.Sscanf(kv[1], "%d", &pageSize)
		case "v":
			videoID = kv[1]
		}
	}
	if pageSize < 1 {
		pageSize = defaultListPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	return kind, page, pageSize, videoID
}

func (t *TelegramBot) handleCallback(c *tb.Callback) {
	if c == nil || c.Message == nil || !t.isAuthorized(c.Sender) {
		return
	}

	// Route by callback data prefix: "\flist_page|..." or "\flist_del|..."
	switch {
	case strings.HasPrefix(c.Data, "\flist_page|"):
		c.Data = strings.TrimPrefix(c.Data, "\flist_page|")
		t.handleListPageCallback(c)
	case strings.HasPrefix(c.Data, "\flist_del|"):
		c.Data = strings.TrimPrefix(c.Data, "\flist_del|")
		t.handleListDeleteCallback(c)
	case strings.HasPrefix(c.Data, "\fact|"):
		c.Data = strings.TrimPrefix(c.Data, "\fact|")
		t.handleActionCallback(c)
	default:
		log.Printf("[WARN] unknown callback: %q", c.Data)
		_ = t.Bot.Respond(c)
	}
}

// handleActionCallback handles inline-menu buttons shown for incoming links.
// Callback data format: "<token>|<action>" where action ∈ {audio, vo, tts, cancel}.
func (t *TelegramBot) handleActionCallback(c *tb.Callback) {
	if c == nil || c.Message == nil || !t.isAuthorized(c.Sender) {
		return
	}

	parts := strings.SplitN(c.Data, "|", 2)
	if len(parts) != 2 {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Bad data"})
		return
	}
	token, action := parts[0], parts[1]

	pa := t.takePendingAction(token)
	if pa == nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Просрочено"})
		_, _ = t.Bot.Edit(c.Message, "⏱ Меню просрочено или уже использовано")
		return
	}

	_ = t.Bot.Respond(c)

	if action == "cancel" {
		_, _ = t.Bot.Edit(c.Message, "🚫 Отменено")
		return
	}

	statusMsg := c.Message
	chat := c.Message.Chat

	switch pa.kind {
	case "yt":
		switch action {
		case "audio":
			if len(pa.videoIDs) == 1 {
				_, _ = t.Bot.Edit(statusMsg, "⏳ Processing...")
				go func() {
					if err := t.processVideo(context.Background(), chat, statusMsg, pa.originalMsg, pa.videoIDs[0]); err != nil {
						log.Printf("[ERROR] failed to process video %s: %v", pa.videoIDs[0], err)
						if ytfeed.IsCookieError(err.Error()) {
							_, _ = t.Bot.Edit(statusMsg,
								"❌ YouTube cookies expired. This video requires authentication.\nRun update-cookies.sh to fix.")
						} else {
							_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("❌ Error: %v", err))
						}
					}
				}()
			} else {
				_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⏳ Processing %d videos...", len(pa.videoIDs)))
				go t.processVideoBatch(context.Background(), chat, statusMsg, pa.originalMsg, pa.videoIDs)
			}
		case "vo":
			if !IsVotCliAvailable() {
				_, _ = t.Bot.Edit(statusMsg, "❌ vot-cli not installed")
				return
			}
			if len(pa.videoIDs) == 1 {
				_, _ = t.Bot.Edit(statusMsg, "⏳ Получаю озвучку...")
				videoID := pa.videoIDs[0]
				videoURL := "https://www.youtube.com/watch?v=" + videoID
				go func() {
					if err := t.processVoiceover(context.Background(), chat, statusMsg, pa.originalMsg, videoURL, videoID); err != nil {
						log.Printf("[ERROR] failed to process voiceover %s: %v", videoID, err)
						if ytfeed.IsCookieError(err.Error()) {
							_, _ = t.Bot.Edit(statusMsg,
								"❌ YouTube cookies expired. This video requires authentication.\nRun update-cookies.sh to fix.")
						} else {
							_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("❌ Error: %v", err))
						}
					}
				}()
			} else {
				_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⏳ Озвучиваю %d видео...", len(pa.videoIDs)))
				go t.processVoiceoverBatch(context.Background(), chat, statusMsg, pa.originalMsg, pa.videoIDs)
			}
		default:
			_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("❌ Unknown action: %s", action))
		}
	case "article":
		if action == "tts" {
			_, _ = t.Bot.Edit(statusMsg, "⏳ Озвучиваю статью...")
			go func() {
				if err := t.processArticle(context.Background(), chat, statusMsg, pa.originalMsg, pa.url); err != nil {
					log.Printf("[ERROR] failed to process article %s: %v", pa.url, err)
					_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("❌ Error: %v", err))
				}
			}()
		} else {
			_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("❌ Unknown action: %s", action))
		}
	}
}

// processVoiceoverBatch sequentially voices over multiple videos. Each call to
// processVoiceover rewrites statusMsg with its own progress; a final summary
// replaces it when the loop is done.
func (t *TelegramBot) processVoiceoverBatch(ctx context.Context, chat *tb.Chat, statusMsg, originalMsg *tb.Message, videoIDs []string) {
	total := len(videoIDs)
	var added, failed int
	cookieErrShown := false

	for i, id := range videoIDs {
		pos := fmt.Sprintf("%d/%d", i+1, total)
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("🎙 %s: запускаю озвучку...", pos))
		videoURL := "https://www.youtube.com/watch?v=" + id
		if err := t.processVoiceover(ctx, chat, statusMsg, originalMsg, videoURL, id); err != nil {
			failed++
			log.Printf("[ERROR] batch voiceover %s: %v", pos, err)
			if ytfeed.IsCookieError(err.Error()) && !cookieErrShown {
				cookieErrShown = true
				_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⚠️ %s: cookies expired, continuing...", pos))
			}
			continue
		}
		added++
	}

	summary := fmt.Sprintf("✅ Озвучено %d/%d", added, total)
	if failed > 0 {
		summary += fmt.Sprintf(" (%d failed)", failed)
	}
	_, _ = t.Bot.Edit(statusMsg, summary)
}

func (t *TelegramBot) handleListPageCallback(c *tb.Callback) {
	if c == nil || c.Message == nil || !t.isAuthorized(c.Sender) {
		return
	}

	kind, page, pageSize, _ := t.unpackCallbackData(c.Data)
	if kind != "list" && kind != "history" {
		kind = "list"
	}

	entries, err := t.Store.Load(t.FeedName, t.MaxItems)
	if err != nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Error loading entries"})
		return
	}

	msg, markup := t.buildListMessage(kind, entries, page, pageSize)
	_, _ = t.Bot.Edit(c.Message, msg, markup, tb.NoPreview)
	_ = t.Bot.Respond(c)
}

func (t *TelegramBot) handleListDeleteCallback(c *tb.Callback) {
	if c == nil || c.Message == nil || !t.isAuthorized(c.Sender) {
		return
	}

	kind, page, pageSize, videoID := t.unpackCallbackData(c.Data)
	if videoID == "" {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Missing video id"})
		return
	}

	entries, err := t.Store.Load(t.FeedName, t.MaxItems)
	if err != nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Error loading entries"})
		return
	}

	var entry *ytfeed.Entry
	for i := range entries {
		if entries[i].VideoID == videoID {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Not found"})
		return
	}

	if err := t.deleteEntry(*entry); err != nil {
		_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Delete failed"})
		return
	}

	entries, _ = t.Store.Load(t.FeedName, t.MaxItems)
	msg, markup := t.buildListMessage(kind, entries, page, pageSize)
	_, _ = t.Bot.Edit(c.Message, msg, markup, tb.NoPreview)
	_ = t.Bot.Respond(c, &tb.CallbackResponse{Text: "Deleted"})
}

func (t *TelegramBot) deleteEntry(entry ytfeed.Entry) error {
	// delete audio file from disk
	if entry.File != "" {
		if err := os.Remove(entry.File); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] failed to delete file %s: %v", entry.File, err)
		} else {
			log.Printf("[INFO] deleted file %s", entry.File)
		}
	}

	// remove from database
	if err := t.Store.Remove(entry); err != nil {
		return err
	}

	// reset processed status so it can be re-added if needed
	_ = t.Store.ResetProcessed(entry)

	log.Printf("[INFO] deleted entry %s: %s", entry.VideoID, entry.Title)
	return nil
}

// processArticle extracts article text, converts to speech, and adds to feed
func (t *TelegramBot) processArticle(ctx context.Context, chat *tb.Chat, statusMsg, originalMsg *tb.Message, articleURL string) error {
	// 1. Extract article content
	_, _ = t.Bot.Edit(statusMsg, "⏳ Извлекаю текст статьи...")
	article, err := t.ArticleExtractor.Extract(ctx, articleURL)
	if err != nil {
		return fmt.Errorf("failed to extract article: %w", err)
	}

	if article.TextContent == "" {
		return fmt.Errorf("no text content found in article")
	}

	// 1.5. Translate if needed (for non-Russian articles)
	translator := NewTranslatorWithKey(os.Getenv("YANDEX_TRANSLATE_KEY"), os.Getenv("YANDEX_FOLDER_ID"), "ru")
	if translator.NeedsTranslation(article.TextContent) {
		detectedLang := DetectLanguage(article.TextContent)
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("🌐 Перевожу с %s на русский...", detectedLang))

		translatedText, err := translator.Translate(ctx, article.TextContent)
		if err != nil {
			return fmt.Errorf("failed to translate article: %w", err)
		}
		article.TextContent = translatedText
	}

	// 2. Generate unique ID for this article
	articleID := t.makeArticleID(articleURL)

	// 3. Check if already processed
	tempEntry := ytfeed.Entry{ChannelID: t.FeedName, VideoID: articleID}
	if found, _, _ := t.Store.CheckProcessed(tempEntry); found {
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⚠️ Already in feed: %s", article.Title))
		t.deleteMessageAfterDelay(originalMsg, 5*time.Second)
		return nil
	}

	// 4. Convert to speech
	const maxTextLen = 150000 // ~2.5 hours of audio
	runes := []rune(article.TextContent)
	if len(runes) > maxTextLen {
		article.TextContent = string(runes[:maxTextLen])
		log.Printf("[WARN] article text truncated from %d to %d characters", len(runes), maxTextLen)
	}
	charCount := len([]rune(article.TextContent))
	_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("🔊 Озвучиваю: %s (%d символов)...", article.Title, charCount))

	edgeTTS, ok := t.TTS.(*EdgeTTS)
	if !ok {
		return fmt.Errorf("TTS provider is not EdgeTTS")
	}

	audioData, err := edgeTTS.SynthesizeLongText(ctx, article.TextContent, 3000)
	if err != nil {
		return fmt.Errorf("failed to synthesize speech: %w", err)
	}

	// 5. Save audio file
	fname := t.makeFileName(articleID)
	filePath := t.FilesLocation + "/" + fname + ".mp3"
	if err := os.WriteFile(filePath, audioData, 0644); err != nil {
		return fmt.Errorf("failed to save audio file: %w", err)
	}

	// 6. Estimate duration (Edge TTS ~150 words/min, ~6 chars/word = ~900 chars/min)
	duration := int(float64(charCount) / 900.0 * 60.0)
	if t.DurationSvc != nil {
		if fileDur := t.DurationSvc.File(filePath); fileDur > 0 {
			duration = fileDur
		}
	}

	// 7. Create entry
	entry := t.createArticleEntry(article, articleURL, filePath, duration)

	// 8. Store in BoltDB
	created, err := t.Store.Save(entry)
	if err != nil {
		return fmt.Errorf("failed to save: %w", err)
	}
	if !created {
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⚠️ Already exists: %s", article.Title))
		t.deleteMessageAfterDelay(originalMsg, 5*time.Second)
		return nil
	}

	// 9. Mark as processed
	if err := t.Store.SetProcessed(entry); err != nil {
		log.Printf("[WARN] failed to mark as processed: %v", err)
	}

	// 10. Remove old entries if exceeding MaxItems
	t.removeOldEntries()

	dur := time.Duration(duration) * time.Second
	_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("✅ 📖 %s (%s)", article.Title, t.formatDuration(dur)))

	log.Printf("[INFO] added article %s: %s (duration: %s, chars: %d)", articleID, article.Title, dur.String(), charCount)

	// Delete user's message after delay
	t.deleteMessageAfterDelay(originalMsg, 5*time.Second)
	return nil
}

// makeArticleID creates a unique ID for an article URL
func (t *TelegramBot) makeArticleID(url string) string {
	h := sha1.New()
	h.Write([]byte("article::" + url))
	return fmt.Sprintf("art_%x", h.Sum(nil))[:16]
}

// createArticleEntry creates ytfeed.Entry from Article
func (t *TelegramBot) createArticleEntry(article *Article, url, file string, duration int) ytfeed.Entry {
	title := article.Title
	if title == "" {
		title = "Article"
	}

	thumbnail := article.Image
	if thumbnail == "" {
		// Use a default article icon or leave empty
		thumbnail = ""
	}

	return ytfeed.Entry{
		ChannelID: t.FeedName,
		VideoID:   t.makeArticleID(url),
		Title:     "📖 " + title,
		Link: struct {
			Href string `xml:"href,attr"`
		}{Href: url},
		Published: time.Now(),
		Updated:   time.Now(),
		Media: struct {
			Description template.HTML `xml:"description"`
			Thumbnail   struct {
				URL string `xml:"url,attr"`
			} `xml:"thumbnail"`
		}{
			Description: template.HTML(fmt.Sprintf("TTS озвучка статьи: %s", url)),
			Thumbnail:   struct{ URL string `xml:"url,attr"` }{URL: thumbnail},
		},
		Author: struct {
			Name string `xml:"name"`
			URI  string `xml:"uri"`
		}{
			Name: article.SiteName,
			URI:  url,
		},
		File:     file,
		Duration: duration,
	}
}

// handleVoiceover handles /vo command for YouTube voice-over translation
func (t *TelegramBot) handleVoiceover(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		return
	}

	// Extract YouTube URL from command argument
	args := regexp.MustCompile(`\s+`).Split(m.Text, 2)
	if len(args) < 2 || args[1] == "" {
		_, _ = t.Bot.Send(m.Chat, "Usage: /vo <youtube_url>\nExample: /vo https://youtube.com/watch?v=xxx")
		return
	}

	videoURL := args[1]
	videoID := t.extractYouTubeVideoID(videoURL)
	if videoID == "" {
		_, _ = t.Bot.Send(m.Chat, "❌ Invalid YouTube URL")
		return
	}

	// Check if vot-cli is available
	if !IsVotCliAvailable() {
		_, _ = t.Bot.Send(m.Chat, "❌ vot-cli not installed")
		return
	}

	statusMsg, _ := t.Bot.Send(m.Chat, "⏳ Получаю озвучку...")
	go func() {
		if err := t.processVoiceover(context.Background(), m.Chat, statusMsg, m, videoURL, videoID); err != nil {
			log.Printf("[ERROR] failed to process voiceover %s: %v", videoID, err)
			if ytfeed.IsCookieError(err.Error()) {
				_, _ = t.Bot.Edit(statusMsg,
					"❌ YouTube cookies expired. This video requires authentication.\nRun update-cookies.sh to fix.")
			} else {
				_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("❌ Error: %v", err))
			}
		}
	}()
}

// processVoiceover downloads voice-over translated audio for a YouTube video
func (t *TelegramBot) processVoiceover(ctx context.Context, chat *tb.Chat, statusMsg, originalMsg *tb.Message, videoURL, videoID string) error {
	// 1. Generate unique ID for this voiceover
	voiceoverID := fmt.Sprintf("vo_%s", videoID)

	// 2. Check if already processed
	tempEntry := ytfeed.Entry{ChannelID: t.FeedName, VideoID: voiceoverID}
	if found, _, _ := t.Store.CheckProcessed(tempEntry); found {
		_, _ = t.Bot.Edit(statusMsg, "⚠️ Уже есть в ленте")
		t.deleteMessageAfterDelay(originalMsg, 5*time.Second)
		return nil
	}

	// 3. Fetch video info first (for title and thumbnail)
	_, _ = t.Bot.Edit(statusMsg, "⏳ Получаю информацию о видео...")
	info, err := t.Downloader.GetInfo(ctx, videoURL)
	if err != nil {
		return fmt.Errorf("failed to get video info: %w", err)
	}

	// 4. Choose voiceover method (priority: YouTube Dubbed → vot-cli → subtitles)
	var filePath string
	var duration int
	var method string

	// 4a. Try YouTube Dubbed track first
	_, _ = t.Bot.Edit(statusMsg, "🔍 Ищу русскую дорожку на YouTube...")
	tracks, trackErr := t.VoiceoverSvc.GetDubbedAudioTracks(ctx, videoURL)
	dubbedTrack := t.VoiceoverSvc.FindDubbedTrack(tracks)

	if trackErr == nil && dubbedTrack != nil {
		// Found dubbed track - download it
		log.Printf("[INFO] found YouTube dubbed track (lang=%s) for %s", dubbedTrack.Language, videoID)
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("🎬 Скачиваю дубляж YouTube: %s...", info.Title))

		result, err := t.VoiceoverSvc.DownloadDubbedTrack(ctx, videoURL, dubbedTrack)
		if err != nil {
			log.Printf("[WARN] failed to download dubbed track, falling back: %v", err)
		} else {
			filePath = result.FilePath
			method = "youtube-dubbed"
			log.Printf("[INFO] downloaded YouTube dubbed track: %s", filePath)
		}
	}

	// 4b. Fallback to vot-cli or subtitles if no dubbed track
	if filePath == "" {
		maxDuration := 4 * 60 * 60 // 4 hours in seconds

		if int(info.Duration) > maxDuration {
			// Subtitles fallback for long videos
			log.Printf("[INFO] video > 4 hours, using subtitle fallback for %s", videoID)
			_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("📝 Видео > 4ч, скачиваю субтитры: %s...", info.Title))

			fp, dur, err := t.processVoiceoverViaSubtitles(ctx, statusMsg, videoURL, videoID, info)
			if err != nil {
				return err
			}
			filePath = fp
			duration = dur
			method = "subtitles-tts"
		} else {
			// vot-cli for videos under 4 hours
			_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("🎙 Скачиваю озвучку (vot-cli): %s...", info.Title))
			result, err := t.VoiceoverSvc.TranslateVideo(ctx, videoURL)
			if err != nil {
				return fmt.Errorf("failed to get voiceover: %w", err)
			}

			log.Printf("[INFO] voiceover downloaded via vot-cli: %s (size: %d bytes)", result.FilePath, result.FileSize)
			filePath = result.FilePath
			method = "vot-cli"
		}
	}

	// Get duration from file if not already set
	if duration == 0 && t.DurationSvc != nil {
		if fileDur := t.DurationSvc.File(filePath); fileDur > 0 {
			duration = fileDur
		}
	}

	log.Printf("[INFO] voiceover complete via %s: %s", method, filePath)

	// 7. Create entry using video info
	thumbnail := info.Thumbnail
	if thumbnail == "" {
		thumbnail = fmt.Sprintf("https://i.ytimg.com/vi/%s/hqdefault.jpg", videoID)
	}

	// Choose emoji based on method
	titleEmoji := "🎙" // default for vot-cli
	switch method {
	case "youtube-dubbed":
		titleEmoji = "🎬" // official dub
	case "subtitles-tts":
		titleEmoji = "📝" // subtitles
	}

	entry := ytfeed.Entry{
		ChannelID: t.FeedName,
		VideoID:   voiceoverID,
		Title:     titleEmoji + " " + info.Title,
		Link: struct {
			Href string `xml:"href,attr"`
		}{Href: videoURL},
		Published: time.Now(),
		Updated:   time.Now(),
		Media: struct {
			Description template.HTML `xml:"description"`
			Thumbnail   struct {
				URL string `xml:"url,attr"`
			} `xml:"thumbnail"`
		}{
			Description: template.HTML(fmt.Sprintf("Озвучка YouTube видео (%s): %s\n%s", method, info.Title, info.Description)),
			Thumbnail:   struct{ URL string `xml:"url,attr"` }{URL: thumbnail},
		},
		Author: struct {
			Name string `xml:"name"`
			URI  string `xml:"uri"`
		}{
			Name: info.Uploader,
			URI:  info.ChannelURL,
		},
		File:     filePath,
		Duration: duration,
	}

	// 8. Store in BoltDB
	created, err := t.Store.Save(entry)
	if err != nil {
		return fmt.Errorf("failed to save: %w", err)
	}
	if !created {
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⚠️ Already exists: %s", info.Title))
		t.deleteMessageAfterDelay(originalMsg, 5*time.Second)
		return nil
	}

	// 9. Mark as processed
	if err := t.Store.SetProcessed(entry); err != nil {
		log.Printf("[WARN] failed to mark as processed: %v", err)
	}

	// 10. Remove old entries if exceeding MaxItems
	t.removeOldEntries()

	dur := time.Duration(duration) * time.Second
	_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("✅ %s %s (%s)", titleEmoji, info.Title, t.formatDuration(dur)))

	log.Printf("[INFO] added voiceover %s via %s: %s (duration: %s)", voiceoverID, method, info.Title, dur.String())

	// Delete user's message after delay
	t.deleteMessageAfterDelay(originalMsg, 5*time.Second)
	return nil
}

// processVoiceoverViaSubtitles handles long videos (>4h) by downloading subtitles,
// translating them, and converting to speech via Edge TTS
func (t *TelegramBot) processVoiceoverViaSubtitles(ctx context.Context, statusMsg *tb.Message, videoURL, videoID string, info *ytfeed.VideoInfo) (string, int, error) {
	// 1. Download subtitles
	_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("📝 Скачиваю субтитры: %s...", info.Title))
	subFile, lang, err := t.SubtitleSvc.DownloadSubtitles(ctx, videoURL)
	if err != nil {
		return "", 0, fmt.Errorf("не удалось скачать субтитры: %w", err)
	}
	defer t.SubtitleSvc.Cleanup(subFile)

	// 2. Parse subtitles to text
	_, _ = t.Bot.Edit(statusMsg, "📄 Извлекаю текст из субтитров...")
	text, err := t.SubtitleSvc.ParseSubtitles(subFile)
	if err != nil {
		return "", 0, fmt.Errorf("не удалось распарсить субтитры: %w", err)
	}

	if text == "" {
		return "", 0, fmt.Errorf("субтитры пустые")
	}

	const maxSubtitleLen = 150000 // ~2.5 hours of audio
	subRunes := []rune(text)
	if len(subRunes) > maxSubtitleLen {
		text = string(subRunes[:maxSubtitleLen])
		log.Printf("[WARN] subtitle text truncated from %d to %d characters", len(subRunes), maxSubtitleLen)
	}

	charCount := len([]rune(text))
	log.Printf("[INFO] extracted %d characters from subtitles (lang: %s)", charCount, lang)

	// 3. Translate if not Russian
	if lang != "ru" && t.Translator != nil && t.Translator.NeedsTranslation(text) {
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("🌐 Перевожу с %s на русский (%d символов)...", lang, charCount))
		translated, err := t.Translator.Translate(ctx, text)
		if err != nil {
			return "", 0, fmt.Errorf("не удалось перевести: %w", err)
		}
		text = translated
		charCount = len([]rune(text))
	}

	// 4. Convert to speech via Edge TTS
	_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("🔊 Озвучиваю (%d символов, это займёт время)...", charCount))

	// Need TTS provider
	if t.TTS == nil {
		// Initialize TTS if not available
		t.TTS = NewEdgeTTS("ru-RU-DmitryNeural")
	}

	edgeTTS, ok := t.TTS.(*EdgeTTS)
	if !ok {
		return "", 0, fmt.Errorf("TTS провайдер недоступен")
	}

	audioData, err := edgeTTS.SynthesizeLongText(ctx, text, 3000)
	if err != nil {
		return "", 0, fmt.Errorf("не удалось озвучить: %w", err)
	}

	// 5. Save audio file
	filePath := fmt.Sprintf("%s/vo_%s_%d.mp3", t.FilesLocation, videoID, time.Now().Unix())
	if err := os.WriteFile(filePath, audioData, 0644); err != nil {
		return "", 0, fmt.Errorf("не удалось сохранить файл: %w", err)
	}

	// 6. Get duration
	duration := 0
	if t.DurationSvc != nil {
		if fileDur := t.DurationSvc.File(filePath); fileDur > 0 {
			duration = fileDur
		}
	}
	if duration == 0 {
		// Estimate: ~900 chars/min for Russian TTS
		duration = int(float64(charCount) / 900.0 * 60.0)
	}

	log.Printf("[INFO] subtitle voiceover created: %s (chars: %d, duration: %ds)", filePath, charCount, duration)
	return filePath, duration, nil
}

// splitTelegramMessage splits a message into chunks that fit within Telegram's message size limit.
// Splits at line boundaries to avoid breaking entries mid-line.
func splitTelegramMessage(msg string, maxSize int) []string {
	if len(msg) <= maxSize {
		return []string{msg}
	}

	var chunks []string
	lines := strings.Split(msg, "\n")
	var current strings.Builder

	for _, line := range lines {
		if current.Len()+len(line)+1 > maxSize && current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(line)
	}

	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	return chunks
}

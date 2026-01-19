package proc

import (
	"context"
	"crypto/sha1"
	"fmt"
	"html/template"
	"os"
	"regexp"
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
}

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
		Bot:           bot,
		AllowedUserID: params.AllowedUserID,
		FeedName:      params.FeedName,
		FeedTitle:     params.FeedTitle,
		MaxItems:      params.MaxItems,
		Downloader:    params.Downloader,
		Store:         params.Store,
		DurationSvc:   params.DurationSvc,
		FilesLocation: params.FilesLocation,
		BaseURL:       params.BaseURL,
		TTSEnabled:    params.TTSEnabled,
	}

	// Initialize TTS if enabled
	if params.TTSEnabled {
		tb.TTS = NewEdgeTTS(params.TTSVoice)
		tb.ArticleExtractor = NewArticleExtractor()
	}

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
	t.Bot.Handle("/help", t.handleHelp)
	t.Bot.Handle("/start", t.handleHelp)

	// Start polling in goroutine
	go t.Bot.Start()

	// Wait for context cancellation
	<-ctx.Done()
	t.Bot.Stop()
	log.Printf("[INFO] telegram bot stopped")
	return ctx.Err()
}

// handleText processes text messages (YouTube URLs or article URLs)
func (t *TelegramBot) handleText(m *tb.Message) {
	// Authorization check
	if !t.isAuthorized(m.Sender) {
		log.Printf("[WARN] unauthorized user %d tried to send message", m.Sender.ID)
		_, _ = t.Bot.Send(m.Chat, "Unauthorized. This bot is private.")
		return
	}

	// Extract YouTube URL first
	videoID := t.extractYouTubeVideoID(m.Text)
	if videoID != "" {
		// Process as YouTube video
		statusMsg, _ := t.Bot.Send(m.Chat, "⏳ Processing...")
		go func() {
			if err := t.processVideo(context.Background(), m.Chat, statusMsg, m, videoID); err != nil {
				log.Printf("[ERROR] failed to process video %s: %v", videoID, err)
				_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("❌ Error: %v", err))
			}
		}()
		return
	}

	// Check if it's an article URL (and TTS is enabled)
	articleURL := t.extractURL(m.Text)
	if articleURL != "" && t.TTSEnabled && IsArticleURL(articleURL) {
		statusMsg, _ := t.Bot.Send(m.Chat, "⏳ Озвучиваю статью...")
		go func() {
			if err := t.processArticle(context.Background(), m.Chat, statusMsg, m, articleURL); err != nil {
				log.Printf("[ERROR] failed to process article %s: %v", articleURL, err)
				_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("❌ Error: %v", err))
			}
		}()
		return
	}

	// No valid URL found
	helpMsg := "No valid URL found. Send a link:\n• YouTube: https://youtube.com/watch?v=VIDEO_ID"
	if t.TTSEnabled {
		helpMsg += "\n• Article: any web page URL"
	}
	_, _ = t.Bot.Send(m.Chat, helpMsg)
}

// handleList shows recent entries
func (t *TelegramBot) handleList(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		return
	}

	entries, err := t.Store.Load(t.FeedName, 10)
	if err != nil {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("Error loading entries: %v", err))
		return
	}

	if len(entries) == 0 {
		_, _ = t.Bot.Send(m.Chat, "No videos in feed yet.")
		return
	}

	msg := fmt.Sprintf("Recent videos (%d):\n\n", len(entries))
	for i, e := range entries {
		dur := time.Duration(e.Duration) * time.Second
		msg += fmt.Sprintf("%d. %s (%s)\n", i+1, e.Title, dur.String())
	}

	_, _ = t.Bot.Send(m.Chat, msg)
}

// handleHistory shows history of all processed videos
func (t *TelegramBot) handleHistory(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		return
	}

	entries, err := t.Store.Load(t.FeedName, t.MaxItems)
	if err != nil {
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("Error: %v", err))
		return
	}

	if len(entries) == 0 {
		_, _ = t.Bot.Send(m.Chat, "No videos added yet.")
		return
	}

	msg := fmt.Sprintf("📜 History (%d):\n\n", len(entries))
	for i, e := range entries {
		msg += fmt.Sprintf("%d. %s\n%s\n\n", i+1, e.Title, e.Link.Href)
	}

	_, _ = t.Bot.Send(m.Chat, msg, tb.NoPreview)
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
		_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("Error removing: %v", err))
		return
	}

	// reset processed status so it can be re-added if needed
	_ = t.Store.ResetProcessed(entry)

	_, _ = t.Bot.Send(m.Chat, fmt.Sprintf("🗑 Deleted: %s", entry.Title))
	log.Printf("[INFO] deleted entry %s: %s", entry.VideoID, entry.Title)
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

// processVideo downloads and stores a YouTube video
func (t *TelegramBot) processVideo(ctx context.Context, chat *tb.Chat, statusMsg, originalMsg *tb.Message, videoID string) error {
	videoURL := "https://www.youtube.com/watch?v=" + videoID

	// 1. Fetch metadata
	_, _ = t.Bot.Edit(statusMsg, "⏳ Fetching video info...")
	info, err := t.Downloader.GetInfo(ctx, videoURL)
	if err != nil {
		return fmt.Errorf("failed to get video info: %w", err)
	}

	// 2. Check if already processed
	tempEntry := ytfeed.Entry{ChannelID: t.FeedName, VideoID: videoID}
	if found, _, _ := t.Store.CheckProcessed(tempEntry); found {
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⚠️ Already in feed: %s", info.Title))
		t.deleteMessageAfterDelay(originalMsg, 5*time.Second)
		return nil
	}

	// 3. Download audio
	_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⬇️ Downloading: %s...", info.Title))
	fname := t.makeFileName(videoID)
	file, err := t.Downloader.Get(ctx, videoID, fname)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
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
		return fmt.Errorf("failed to save: %w", err)
	}
	if !created {
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("⚠️ Already exists: %s", info.Title))
		t.deleteMessageAfterDelay(originalMsg, 5*time.Second)
		return nil
	}

	// 7. Mark as processed
	if err := t.Store.SetProcessed(entry); err != nil {
		log.Printf("[WARN] failed to mark as processed: %v", err)
	}

	// 8. Remove old entries if exceeding MaxItems
	t.removeOldEntries()

	dur := time.Duration(duration) * time.Second
	_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("✅ %s (%s)", info.Title, t.formatDuration(dur)))

	log.Printf("[INFO] added video %s: %s (duration: %s)", videoID, info.Title, dur.String())

	// delete user's message after delay, keep bot's status
	t.deleteMessageAfterDelay(originalMsg, 5*time.Second)
	return nil
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

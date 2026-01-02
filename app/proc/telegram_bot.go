package proc

import (
	"context"
	"crypto/sha1"
	"fmt"
	"html/template"
	"regexp"
	"time"

	log "github.com/go-pkgz/lgr"
	tb "gopkg.in/tucnak/telebot.v2"

	ytfeed "github.com/umputun/feed-master/app/youtube/feed"
	ytstore "github.com/umputun/feed-master/app/youtube/store"
)

// TelegramBot handles incoming messages for YouTube video additions
type TelegramBot struct {
	Bot           *tb.Bot
	AllowedUserID int64
	FeedName      string
	FeedTitle     string
	MaxItems      int
	Downloader    *ytfeed.Downloader
	Store         *ytstore.BoltDB
	DurationSvc   DurationService
	FilesLocation string
	BaseURL       string
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

	return &TelegramBot{
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
	}, nil
}

// Run starts the bot and listens for messages
func (t *TelegramBot) Run(ctx context.Context) error {
	log.Printf("[INFO] starting telegram bot for user %d, feed: %s", t.AllowedUserID, t.FeedName)

	// Register handlers
	t.Bot.Handle(tb.OnText, t.handleText)
	t.Bot.Handle("/list", t.handleList)
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

// handleText processes text messages (YouTube URLs)
func (t *TelegramBot) handleText(m *tb.Message) {
	// Authorization check
	if !t.isAuthorized(m.Sender) {
		log.Printf("[WARN] unauthorized user %d tried to send message", m.Sender.ID)
		_, _ = t.Bot.Send(m.Chat, "Unauthorized. This bot is private.")
		return
	}

	// Extract YouTube URL
	videoID := t.extractYouTubeVideoID(m.Text)
	if videoID == "" {
		_, _ = t.Bot.Send(m.Chat, "No YouTube video URL found. Send a link like:\nhttps://youtube.com/watch?v=VIDEO_ID\nhttps://youtu.be/VIDEO_ID")
		return
	}

	// Send processing message
	statusMsg, _ := t.Bot.Send(m.Chat, "Processing video...")

	// Process in background
	go func() {
		if err := t.processVideo(context.Background(), m.Chat, statusMsg, videoID); err != nil {
			log.Printf("[ERROR] failed to process video %s: %v", videoID, err)
			_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("Error: %v", err))
		}
	}()
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

// handleHelp sends help message
func (t *TelegramBot) handleHelp(m *tb.Message) {
	if !t.isAuthorized(m.Sender) {
		_, _ = t.Bot.Send(m.Chat, "Unauthorized. This bot is private.")
		return
	}

	help := fmt.Sprintf(`Turnip Podcast Bot

Send a YouTube URL to add video audio to your podcast feed.

Supported formats:
- https://youtube.com/watch?v=VIDEO_ID
- https://youtu.be/VIDEO_ID
- https://www.youtube.com/watch?v=VIDEO_ID

Commands:
/list - show recent additions
/help - show this help

RSS feed: %s/yt/rss/%s`, t.BaseURL, t.FeedName)

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
func (t *TelegramBot) processVideo(ctx context.Context, chat *tb.Chat, statusMsg *tb.Message, videoID string) error {
	videoURL := "https://www.youtube.com/watch?v=" + videoID

	// 1. Fetch metadata
	_, _ = t.Bot.Edit(statusMsg, "Fetching video info...")
	info, err := t.Downloader.GetInfo(ctx, videoURL)
	if err != nil {
		return fmt.Errorf("failed to get video info: %w", err)
	}

	// 2. Check if already processed
	tempEntry := ytfeed.Entry{ChannelID: t.FeedName, VideoID: videoID}
	if found, _, _ := t.Store.CheckProcessed(tempEntry); found {
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("Video already in feed: %s", info.Title))
		return nil
	}

	// 3. Download audio
	_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("Downloading: %s...", info.Title))
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
		_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("Video already exists: %s", info.Title))
		return nil
	}

	// 7. Mark as processed
	if err := t.Store.SetProcessed(entry); err != nil {
		log.Printf("[WARN] failed to mark as processed: %v", err)
	}

	dur := time.Duration(duration) * time.Second
	_, _ = t.Bot.Edit(statusMsg, fmt.Sprintf("Added: %s\nDuration: %s\n\nRSS: %s/yt/rss/%s",
		info.Title, dur.String(), t.BaseURL, t.FeedName))

	log.Printf("[INFO] added video %s: %s (duration: %s)", videoID, info.Title, dur.String())
	return nil
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

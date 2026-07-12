package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/ChimeraCoder/anaconda"

	log "github.com/go-pkgz/lgr"
	"github.com/google/uuid"
	"github.com/jessevdk/go-flags"
	bolt "go.etcd.io/bbolt"

	"github.com/umputun/feed-master/app/api"
	"github.com/umputun/feed-master/app/config"
	"github.com/umputun/feed-master/app/duration"
	rssfeed "github.com/umputun/feed-master/app/feed"
	"github.com/umputun/feed-master/app/proc"
	"github.com/umputun/feed-master/app/publisher"
	"github.com/umputun/feed-master/app/youtube"
	ytfeed "github.com/umputun/feed-master/app/youtube/feed"
	"github.com/umputun/feed-master/app/youtube/store"
)

type options struct {
	Port int    `short:"p" long:"port" description:"port to listen" default:"8080"`
	DB   string `short:"c" long:"db" env:"FM_DB" default:"var/feed-master.bdb" description:"bolt db file"`
	Conf string `short:"f" long:"conf" env:"FM_CONF" default:"feed-master.yml" description:"config file (yml)"`

	// single feed overrides
	Feed            string        `long:"feed" env:"FM_FEED" description:"single feed, overrides config"`
	TelegramChannel string        `long:"telegram_chan" env:"TELEGRAM_CHAN" description:"single telegram channel, overrides config"`
	UpdateInterval  time.Duration `long:"update-interval" env:"UPDATE_INTERVAL" default:"1m" description:"update interval, overrides config"`

	TelegramServer        string        `long:"telegram_server" env:"TELEGRAM_SERVER" default:"https://api.telegram.org" description:"telegram bot api server"`
	TelegramToken         string        `long:"telegram_token" env:"TELEGRAM_TOKEN" description:"telegram token"`
	TelegramTimeout       time.Duration `long:"telegram_timeout" env:"TELEGRAM_TIMEOUT" default:"1m" description:"telegram timeout"`
	TwitterConsumerKey    string        `long:"consumer-key" env:"TWI_CONSUMER_KEY" description:"twitter consumer key"`
	TwitterConsumerSecret string        `long:"consumer-secret" env:"TWI_CONSUMER_SECRET" description:"twitter consumer secret"`
	TwitterAccessToken    string        `long:"access-token" env:"TWI_ACCESS_TOKEN" description:"twitter access token"`
	TwitterAccessSecret   string        `long:"access-secret" env:"TWI_ACCESS_SECRET" description:"twitter access secret"`
	TwitterTemplate       string        `long:"template" env:"TEMPLATE" default:"{{.Title}} - {{.Link}}" description:"twitter message template"`

	AdminPasswd string `long:"admin-passwd" env:"ADMIN_PASSWD" description:"admin password for protected endpoints"`

	// one-shot publishing mode: upload a file into a category feed and exit
	// (does not open bolt, safe to run next to the live container)
	Publish         string `long:"publish" description:"publish an audio file to R2 and exit"`
	PublishCategory string `long:"publish-category" description:"category for --publish"`

	Dbg bool `long:"dbg" env:"DEBUG" description:"debug mode"`
}

var revision = "local"

func main() {
	fmt.Printf("feed-master %s\n", revision)
	var opts options
	if _, err := flags.Parse(&opts); err != nil {
		os.Exit(1)
	}
	setupLog(opts.Dbg)

	var conf = &config.Conf{}
	if opts.Feed != "" { // single feed (no config) mode
		conf = config.SingleFeed(opts.Feed, opts.TelegramChannel, opts.UpdateInterval)
	}

	var err error
	if opts.Feed == "" {
		conf, err = config.Load(opts.Conf)
		if err != nil {
			log.Fatalf("[ERROR] can't load config %s, %v", opts.Conf, err)
		}
	}

	pubSvc := makePublisher(conf)

	// one-shot publish: runs before bolt is opened, so it works next to the
	// live container (bolt holds an exclusive file lock)
	if opts.Publish != "" {
		if pubSvc == nil {
			log.Fatalf("[ERROR] publishing not configured: need R2_* env and FEED_SECRET")
		}
		if opts.PublishCategory == "" {
			log.Fatalf("[ERROR] --publish requires --publish-category")
		}
		ep, pubErr := pubSvc.PublishFile(context.Background(), opts.Publish, opts.PublishCategory)
		if pubErr != nil {
			log.Fatalf("[ERROR] publish failed: %v", pubErr)
		}
		fmt.Printf("published: %s\naudio:     %s\nfeed:      %s\n", ep.Title, ep.PublicURL, pubSvc.FeedURL(opts.PublishCategory))
		return
	}

	db, err := makeBoltDB(opts.DB)
	if err != nil {
		log.Fatalf("[ERROR] can't open db %s, %v", opts.DB, err)
	}
	procStore := &proc.BoltDB{DB: db}

	telegramNotif, err := proc.NewTelegramClient(opts.TelegramToken, opts.TelegramServer, opts.TelegramTimeout,
		&duration.Service{}, &proc.TelegramSenderImpl{})
	if err != nil {
		log.Fatalf("[ERROR] failed to initialize telegram client %s, %v", opts.TelegramToken, err)
	}

	p := &proc.Processor{Conf: conf, Store: procStore, TelegramNotif: telegramNotif, TwitterNotif: makeTwitter(opts)}
	go func() {
		if err := p.Do(context.Background()); err != nil {
			log.Printf("[ERROR] processor failed: %v", err)
		}
	}()

	var ytSvc youtube.Service
	var ytStore *store.BoltDB

	// Initialize YouTube service if we have channels OR telegram_bot is enabled
	needYouTube := len(conf.YouTube.Channels) > 0 || conf.TelegramBot.Enabled
	if needYouTube {
		log.Printf("[INFO] initializing youtube service")
		outWr := log.ToWriter(log.Default(), "DEBUG")
		errWr := log.ToWriter(log.Default(), "INFO")
		dwnl := ytfeed.NewDownloader(conf.YouTube.DlTemplate, outWr, errWr, conf.YouTube.FilesLocation, conf.YouTube.CookiesFile)
		fd := ytfeed.Feed{Client: &http.Client{Timeout: 10 * time.Second},
			ChannelBaseURL: conf.YouTube.BaseChanURL, PlaylistBaseURL: conf.YouTube.BasePlaylistURL}

		channels := []string{}
		for _, c := range conf.YouTube.Channels {
			channels = append(channels, c.ID)
		}
		// Add telegram_bot feed to channels
		if conf.TelegramBot.Enabled {
			channels = append(channels, conf.TelegramBot.FeedName)
		}
		log.Printf("[DEBUG] buckets for youtube store: %s", strings.Join(channels, ", "))

		ytStore = &store.BoltDB{DB: db, Channels: channels}
		ytSvc = youtube.Service{
			Feeds:          conf.YouTube.Channels,
			Downloader:     dwnl,
			ChannelService: &fd,
			Store:          ytStore,
			CheckDuration:  conf.YouTube.UpdateInterval,
			KeepPerChannel: conf.YouTube.MaxItems,
			RootURL:        conf.YouTube.BaseURL,
			RSSFileStore: youtube.RSSFileStore{
				Location: conf.YouTube.RSSLocation,
				Enabled:  conf.YouTube.RSSLocation != "",
			},
			DurationService: &duration.Service{},
			SkipShorts:      conf.YouTube.SkipShorts,
		}
		if conf.YouTube.YtDlpUpdate.Interval > 0 {
			log.Printf("[INFO] yt-dlp updater enabled, interval %s", conf.YouTube.YtDlpUpdate.Interval)
			ytSvc.YtDlpUpdCommand = conf.YouTube.YtDlpUpdate.Command
			ytSvc.YtDlpUpdDuration = conf.YouTube.YtDlpUpdate.Interval
		} else {
			log.Printf("[INFO] yt-dlp updater is disabled")
		}

		// Only run youtube processor if we have channels configured
		if len(conf.YouTube.Channels) > 0 {
			go func() {
				if conf.YouTube.DisableUpdates {
					log.Printf("[INFO] youtube updates are disabled")
					return
				}
				if err := ytSvc.Do(context.TODO()); err != nil {
					log.Printf("[ERROR] youtube processor failed: %v", err)
				}
			}()
		}
	}

	// owner notifications for the audio watcher (set when the bot comes up)
	var ownerNotify func(string)

	// Initialize Telegram Bot for manual video additions
	if conf.TelegramBot.Enabled && opts.TelegramToken != "" && conf.TelegramBot.AllowedUserID != 0 {
		log.Printf("[INFO] starting telegram bot for user %d, feed: %s", conf.TelegramBot.AllowedUserID, conf.TelegramBot.FeedName)

		outWr := log.ToWriter(log.Default(), "DEBUG")
		errWr := log.ToWriter(log.Default(), "INFO")
		botDownloader := ytfeed.NewDownloader(conf.YouTube.DlTemplate, outWr, errWr, conf.YouTube.FilesLocation, conf.YouTube.CookiesFile)

		notesSvc := makeNotesService(conf, ytStore, outWr, errWr)

		// feed media offload: new episodes go to R2, /yt/media redirects there
		var feedMedia *publisher.FeedMedia
		if pubSvc != nil {
			feedMedia = &publisher.FeedMedia{Store: pubSvc.R2, Secret: pubSvc.Secret}
		}
		var mediaOffloader proc.MediaOffloader
		if feedMedia != nil {
			mediaOffloader = feedMedia
		}

		tgBot, err := proc.NewTelegramBot(proc.TelegramBotParams{
			Token:         opts.TelegramToken,
			APIURL:        opts.TelegramServer,
			AllowedUserID: conf.TelegramBot.AllowedUserID,
			FeedName:      conf.TelegramBot.FeedName,
			FeedTitle:     conf.TelegramBot.FeedTitle,
			MaxItems:      conf.TelegramBot.MaxItems,
			Downloader:    botDownloader,
			Store:         ytStore,
			DurationSvc:   &duration.Service{},
			FilesLocation: conf.YouTube.FilesLocation,
			BaseURL:       conf.System.BaseURL,
			TTSEnabled:    conf.TelegramBot.TTSEnabled,
			TTSVoice:      conf.TelegramBot.TTSVoice,
			CookiesFile:   conf.YouTube.CookiesFile,
			NotesSvc:      notesSvc,
			Media:         mediaOffloader,
			Pub:           pubSvc,
		})
		if err != nil {
			log.Printf("[ERROR] failed to create telegram bot: %v", err)
		} else {
			if notesSvc != nil {
				notesSvc.Notifier = tgBot                    // set before Run starts the workers
				notesSvc.External = tgBot.RunQueuedVoiceover // podcast translations ride the same queue
			}
			ownerNotify = tgBot.NotifyOwner
			go func() {
				if err := tgBot.Run(context.Background()); err != nil {
					log.Printf("[ERROR] telegram bot failed: %v", err)
				}
			}()
		}
		if notesSvc != nil {
			go notesSvc.Run(context.Background())
		}
	}

	// audio watcher: new files in originals/{category}/ get normalized,
	// uploaded to R2 and added to the category feed automatically
	if pubSvc != nil {
		go pubSvc.Watch(context.Background(), time.Minute, ownerNotify)
	}

	if opts.AdminPasswd == "" {
		log.Printf("[WARN] admin password is not set, protected endpoints are disabled")
		opts.AdminPasswd = uuid.New().String() // generate random (uuid) password
	}

	server := api.Server{
		Version:      revision,
		Conf:         *conf,
		Store:        procStore,
		YoutubeStore: ytStore,
		YoutubeSvc:   &ytSvc,
		AdminPasswd:  opts.AdminPasswd,
	}
	if pubSvc != nil {
		server.PodSecret = pubSvc.Secret
		server.PodFeedsDir = filepath.Join(conf.Audio.Location, "feeds")
		fm := publisher.FeedMedia{Store: pubSvc.R2, Secret: pubSvc.Secret}
		server.MediaRedirectBase = fm.PublicBase()
	}
	server.Run(context.Background(), opts.Port)
}

// makePublisher builds the R2-backed publishing service when R2_* env and
// FEED_SECRET are present; nil otherwise (feature off)
func makePublisher(conf *config.Conf) *publisher.Service {
	r2cfg := publisher.R2Config{
		AccountID:     os.Getenv("R2_ACCOUNT_ID"),
		AccessKeyID:   os.Getenv("R2_ACCESS_KEY_ID"),
		SecretKey:     os.Getenv("R2_SECRET_ACCESS_KEY"),
		Bucket:        os.Getenv("R2_BUCKET"),
		PublicBaseURL: os.Getenv("R2_PUBLIC_BASE_URL"),
	}
	if !r2cfg.Enabled() {
		return nil
	}
	secret := os.Getenv("FEED_SECRET")
	if secret == "" {
		log.Printf("[WARN] R2 configured but FEED_SECRET not set, publishing disabled")
		return nil
	}
	r2, err := publisher.NewR2Store(r2cfg)
	if err != nil {
		log.Printf("[ERROR] failed to init R2: %v", err)
		return nil
	}
	log.Printf("[INFO] publisher enabled: bucket %s, audio dir %s", r2cfg.Bucket, conf.Audio.Location)
	return &publisher.Service{
		R2:       r2,
		AudioDir: conf.Audio.Location,
		Secret:   secret,
		Duration: &duration.Service{},
		BaseURL:  conf.System.BaseURL,
	}
}

// makeNotesService builds the transcription/notes pipeline when enabled and
// GROQ_API_KEY is set. Notion publishing additionally needs NOTION_TOKEN and
// notion_parent_page; without them /notes degrades to /md.
func makeNotesService(conf *config.Conf, ytStore *store.BoltDB, outWr, errWr io.Writer) *proc.NotesService {
	if !conf.Notes.Enabled {
		return nil
	}
	groqKey := os.Getenv("GROQ_API_KEY")
	if groqKey == "" {
		log.Printf("[WARN] notes enabled but GROQ_API_KEY is not set, disabling notes")
		return nil
	}

	// both providers are swappable via OpenAI-compatible endpoints:
	// LLM_API_KEY + notes.llm_base_url for the text LLM,
	// WHISPER_API_KEY + notes.whisper_base_url for transcription
	// (defaults: the same Groq key and Groq endpoints)
	llmKey := os.Getenv("LLM_API_KEY")
	if llmKey == "" {
		llmKey = groqKey
	}
	whisperKey := os.Getenv("WHISPER_API_KEY")
	if whisperKey == "" {
		whisperKey = groqKey
	}

	var notion *proc.NotionWriter
	notionToken := os.Getenv("NOTION_TOKEN")
	switch {
	case notionToken != "" && conf.Notes.NotionParentPage != "":
		notion = proc.NewNotionWriter(notionToken, conf.Notes.NotionParentPage, ytStore)
	case notionToken == "":
		log.Printf("[INFO] NOTION_TOKEN not set, /notes will be transcript-only")
	default:
		log.Printf("[INFO] notes.notion_parent_page not set, /notes will be transcript-only")
	}

	notesDownloader := ytfeed.NewDownloader(conf.YouTube.DlTemplate, outWr, errWr,
		filepath.Join(conf.Notes.MDLocation, "tmp"), conf.YouTube.CookiesFile)

	log.Printf("[INFO] notes enabled: md location %s, whisper %s, llm %s, notion: %v",
		conf.Notes.MDLocation, conf.Notes.WhisperModel, conf.Notes.LLMModel, notion != nil)

	enricher := proc.NewEnrichService(llmKey, conf.Notes.LLMModel)
	if conf.Notes.LLMBaseURL != "" {
		enricher.BaseURL = conf.Notes.LLMBaseURL
	}
	transcriber := proc.NewTranscribeService(whisperKey, conf.Notes.WhisperModel, conf.Notes.ChunkSeconds, &duration.Service{})
	if conf.Notes.WhisperBaseURL != "" {
		transcriber.BaseURL = conf.Notes.WhisperBaseURL
	}

	return proc.NewNotesService(proc.NotesParams{
		MDLocation:  conf.Notes.MDLocation,
		Transcriber: transcriber,
		Enricher:    enricher,
		Notion:      notion,
		Downloader:  notesDownloader,
		SubtitleSvc: proc.NewSubtitleService(filepath.Join(conf.Notes.MDLocation, "tmp"), conf.YouTube.CookiesFile),
		Extractor:   proc.NewArticleExtractor(),
		Apple:       proc.NewAppleResolver(),
		Concurrency: conf.Notes.Concurrency,
		JobStore:    ytStore,
	})
}

func makeBoltDB(dbFile string) (*bolt.DB, error) {
	log.Printf("[INFO] bolt (persistent) store, %s", dbFile)
	if dbFile == "" {
		return nil, fmt.Errorf("empty db")
	}
	if err := os.MkdirAll(path.Dir(dbFile), 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(dbFile, 0o600, &bolt.Options{Timeout: 1 * time.Second}) // nolint
	if err != nil {
		return nil, err
	}

	return db, err
}

func makeTwitter(opts options) *proc.TwitterClient {
	twitterFmtFn := func(item rssfeed.Item) string {
		b1 := bytes.Buffer{}
		if err := template.Must(template.New("twi").Parse(opts.TwitterTemplate)).Execute(&b1, item); err != nil { // nolint
			// template failed to parse record, backup predefined format
			return fmt.Sprintf("%s - %s", item.Title, item.Link)
		}
		return strings.ReplaceAll(proc.CleanText(b1.String(), 280), `\n`, "\n") // \n in template
	}

	twiAuth := proc.TwitterAuth{
		ConsumerKey:    opts.TwitterConsumerKey,
		ConsumerSecret: opts.TwitterConsumerSecret,
		AccessToken:    opts.TwitterAccessToken,
		AccessSecret:   opts.TwitterAccessSecret,
	}

	twitPoster := anaconda.NewTwitterApiWithCredentials(twiAuth.AccessToken, twiAuth.AccessSecret, twiAuth.ConsumerKey, twiAuth.ConsumerSecret)

	return proc.NewTwitterClient(twiAuth, twitterFmtFn, twitPoster)
}

func setupLog(dbg bool) {
	if dbg {
		log.Setup(log.Debug, log.CallerFile, log.Msec, log.LevelBraces)
		return
	}
	log.Setup(log.Msec, log.LevelBraces)
}

package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	log "github.com/go-pkgz/lgr"
)

// IsCookieError checks if yt-dlp error is related to expired/invalid cookies
func IsCookieError(errStr string) bool {
	markers := []string{
		"cookies are no longer valid",
		"cookies have expired",
		"Please sign in",
		"Sign in to confirm your age",
		"Sign in to confirm you",
	}
	for _, m := range markers {
		if strings.Contains(errStr, m) {
			return true
		}
	}
	return false
}

// VideoInfo contains metadata fetched via yt-dlp --dump-json
type VideoInfo struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Uploader    string  `json:"uploader"`
	ChannelID   string  `json:"channel_id"`
	ChannelURL  string  `json:"channel_url"`
	Duration    float64 `json:"duration"`
	Thumbnail   string  `json:"thumbnail"`
	UploadDate  string  `json:"upload_date"` // YYYYMMDD format
	WebpageURL  string  `json:"webpage_url"`
}

// ErrSkip is returned when the file is not downloaded
var ErrSkip = errors.New("skip")

// Downloader executes an external command to download a video and extract its audio.
type Downloader struct {
	ytTemplate   string
	logOutWriter io.Writer
	logErrWriter io.Writer
	destination  string
	cookiesFile  string
}

// NewDownloader creates a new Downloader with the given template (full command with placeholders for {{.ID}} and {{.Filename}}.
// Destination is the directory where the audio files will be stored.
func NewDownloader(tmpl string, logOutWriter, logErrWriter io.Writer, destination, cookiesFile string) *Downloader {
	return &Downloader{
		ytTemplate:   tmpl,
		logOutWriter: logOutWriter,
		logErrWriter: logErrWriter,
		destination:  destination,
		cookiesFile:  cookiesFile,
	}
}

// ytdlpArgs returns common yt-dlp arguments including cookies if configured
func (d *Downloader) ytdlpArgs(args ...string) []string {
	var result []string
	if d.cookiesFile != "" {
		result = append(result, "--cookies", d.cookiesFile)
	}
	result = append(result, "--no-playlist")
	result = append(result, args...)
	return result
}

// Get downloads a video from youtube and extracts audio.
// yt-dlp --extract-audio --audio-format=mp3 --audio-quality=0 -f m4a/bestaudio "https://www.youtube.com/watch?v={{.ID}}" --no-progress -o {{.Filename}}
// On cookie errors, retries without cookies as a fallback.
func (d *Downloader) Get(ctx context.Context, id, fname string) (file string, err error) {
	file, err = d.get(ctx, id, fname, true)
	if err != nil && d.cookiesFile != "" && IsCookieError(err.Error()) {
		log.Printf("[WARN] cookies expired, retrying Get without cookies")
		return d.get(ctx, id, fname, false)
	}
	return file, err
}

func (d *Downloader) get(ctx context.Context, id, fname string, useCookies bool) (file string, err error) {
	if err := os.MkdirAll(d.destination, 0o750); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %w", d.destination, err)
	}

	tmplParams := struct {
		ID       string
		FileName string
	}{
		ID:       id,
		FileName: fname,
	}
	b1 := bytes.Buffer{}
	if err := template.Must(template.New("youtube-dl").Parse(d.ytTemplate)).Execute(&b1, tmplParams); err != nil { // nolint
		return "", fmt.Errorf("failed to parse template: %v", err)
	}

	cmdStr := b1.String()
	if useCookies && d.cookiesFile != "" {
		cmdStr = strings.Replace(cmdStr, "yt-dlp ", "yt-dlp --cookies "+d.cookiesFile+" ", 1)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr) // nolint
	cmd.Stdin = os.Stdin
	cmd.Stdout = d.logOutWriter
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(d.logErrWriter, &stderrBuf)
	cmd.Dir = d.destination
	log.Printf("[DEBUG] executing command: %s", cmdStr)
	if err := cmd.Run(); err != nil {
		stderrStr := stderrBuf.String()
		if stderrStr != "" {
			return "", fmt.Errorf("failed to execute command: %v\n%s", err, stderrStr)
		}
		return "", fmt.Errorf("failed to execute command: %v", err)
	}

	file = filepath.Join(d.destination, fname+".mp3")
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return file, ErrSkip
	}
	return file, nil
}

// GetInfo fetches video metadata without downloading using yt-dlp --dump-json.
// On cookie errors, retries without cookies as a fallback.
func (d *Downloader) GetInfo(ctx context.Context, videoURL string) (*VideoInfo, error) {
	info, err := d.getInfo(ctx, videoURL, true)
	if err != nil && d.cookiesFile != "" && IsCookieError(err.Error()) {
		log.Printf("[WARN] cookies expired, retrying GetInfo without cookies")
		return d.getInfo(ctx, videoURL, false)
	}
	return info, err
}

func (d *Downloader) getInfo(ctx context.Context, videoURL string, useCookies bool) (*VideoInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	args := d.ytdlpArgs("--dump-json", "--no-download", videoURL)
	if !useCookies {
		args = ytdlpArgsWithoutCookies(args)
	}

	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(d.logErrWriter, &stderrBuf)

	output, err := cmd.Output()
	if err != nil {
		stderrStr := stderrBuf.String()
		if stderrStr != "" {
			return nil, fmt.Errorf("failed to get video info: %w\n%s", err, stderrStr)
		}
		return nil, fmt.Errorf("failed to get video info: %w", err)
	}

	var info VideoInfo
	if err := json.Unmarshal(output, &info); err != nil {
		return nil, fmt.Errorf("failed to parse video info: %w", err)
	}

	log.Printf("[DEBUG] got video info: id=%s, title=%s, duration=%.0fs", info.ID, info.Title, info.Duration)
	return &info, nil
}

// ytdlpArgsWithoutCookies removes --cookies and its value from args slice
func ytdlpArgsWithoutCookies(args []string) []string {
	var result []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--cookies" && i+1 < len(args) {
			i++ // skip the cookies file path too
			continue
		}
		result = append(result, args[i])
	}
	return result
}

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
	"text/template"
	"time"

	log "github.com/go-pkgz/lgr"
)

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
}

// NewDownloader creates a new Downloader with the given template (full command with placeholders for {{.ID}} and {{.Filename}}.
// Destination is the directory where the audio files will be stored.
func NewDownloader(tmpl string, logOutWriter, logErrWriter io.Writer, destination string) *Downloader {
	return &Downloader{
		ytTemplate:   tmpl,
		logOutWriter: logOutWriter,
		logErrWriter: logErrWriter,
		destination:  destination,
	}
}

// Get downloads a video from youtube and extracts audio.
// yt-dlp --extract-audio --audio-format=mp3 --audio-quality=0 -f m4a/bestaudio "https://www.youtube.com/watch?v={{.ID}}" --no-progress -o {{.Filename}}
func (d *Downloader) Get(ctx context.Context, id, fname string) (file string, err error) {

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

	cmd := exec.CommandContext(ctx, "sh", "-c", b1.String()) // nolint
	cmd.Stdin = os.Stdin
	cmd.Stdout = d.logOutWriter
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(d.logErrWriter, &stderrBuf)
	cmd.Dir = d.destination
	log.Printf("[DEBUG] executing command: %s", b1.String())
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

// GetInfo fetches video metadata without downloading using yt-dlp --dump-json
func (d *Downloader) GetInfo(ctx context.Context, videoURL string) (*VideoInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "yt-dlp", "--dump-json", "--no-download",
		"--no-playlist",
		"--extractor-args", "youtube:player_client=web_creator",
		videoURL)
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

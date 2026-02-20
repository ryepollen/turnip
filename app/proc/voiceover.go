package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	log "github.com/go-pkgz/lgr"

	ytfeed "github.com/umputun/feed-master/app/youtube/feed"
)

// AudioTrack represents a YouTube audio track (original or dubbed)
type AudioTrack struct {
	FormatID string
	Language string
	Quality  string
	Bitrate  int
}

// VoiceoverService handles YouTube video voice-over translation using vot-cli
type VoiceoverService struct {
	OutputDir   string
	TargetLang  string
	CookiesFile string
}

// NewVoiceoverService creates a new voiceover service
func NewVoiceoverService(outputDir, targetLang, cookiesFile string) *VoiceoverService {
	if targetLang == "" {
		targetLang = "ru"
	}
	return &VoiceoverService{
		OutputDir:   outputDir,
		TargetLang:  targetLang,
		CookiesFile: cookiesFile,
	}
}

// ytdlpArgs returns common yt-dlp arguments including cookies if configured.
// If useCookies is false, cookies are omitted even if CookiesFile is set.
func (v *VoiceoverService) ytdlpArgs(useCookies bool, args ...string) []string {
	var result []string
	if useCookies && v.CookiesFile != "" {
		result = append(result, "--cookies", v.CookiesFile)
	}
	result = append(result, "--no-playlist")
	result = append(result, args...)
	return result
}

// VoiceoverResult contains the result of voice-over translation
type VoiceoverResult struct {
	FilePath string
	Title    string
	Duration int
	FileSize int64
}

// TranslateVideo downloads voice-over translated audio for a YouTube video
func (v *VoiceoverService) TranslateVideo(ctx context.Context, videoURL string) (*VoiceoverResult, error) {
	// Normalize URL: replace m.youtube.com with www.youtube.com
	videoURL = normalizeYouTubeURL(videoURL)

	// Create unique filename based on video ID and timestamp
	videoID := extractVideoID(videoURL)
	if videoID == "" {
		return nil, fmt.Errorf("could not extract video ID from URL")
	}

	outputFile := filepath.Join(v.OutputDir, fmt.Sprintf("vo_%s_%d.mp3", videoID, time.Now().Unix()))

	// Build vot-cli command
	// vot-cli --output /path/to --output-file name.mp3 --reslang ru "URL"
	args := []string{
		"--output", v.OutputDir,
		"--output-file", filepath.Base(outputFile),
		"--reslang", v.TargetLang,
		videoURL,
	}

	log.Printf("[INFO] running vot-cli with args: %v", args)

	// Use timeout context for vot-cli (30 minutes max for long videos)
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "vot-cli", args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	log.Printf("[DEBUG] vot-cli stdout: %s", stdout.String())
	log.Printf("[DEBUG] vot-cli stderr: %s", stderr.String())

	if err != nil {
		return nil, fmt.Errorf("vot-cli failed: %w\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	// Check if file was created and has content
	fileInfo, statErr := os.Stat(outputFile)
	if os.IsNotExist(statErr) {
		return nil, fmt.Errorf("vot-cli did not create output file at %s", outputFile)
	}
	if statErr != nil {
		return nil, fmt.Errorf("failed to stat output file: %w", statErr)
	}
	if fileInfo.Size() == 0 {
		return nil, fmt.Errorf("vot-cli created empty file")
	}

	log.Printf("[INFO] vot-cli created file %s (size: %d bytes)", outputFile, fileInfo.Size())

	return &VoiceoverResult{
		FilePath: outputFile,
		Title:    "", // Title will be set by caller using video info
		Duration: 0,  // Will be determined from file later
		FileSize: fileInfo.Size(),
	}, nil
}

// normalizeYouTubeURL converts mobile and other YouTube URL variants to standard format
func normalizeYouTubeURL(url string) string {
	// Replace mobile URL with standard
	url = strings.Replace(url, "m.youtube.com", "www.youtube.com", 1)
	url = strings.Replace(url, "music.youtube.com", "www.youtube.com", 1)
	return url
}

// extractVideoID extracts YouTube video ID from URL
func extractVideoID(url string) string {
	// Handle various YouTube URL formats
	// https://www.youtube.com/watch?v=VIDEO_ID
	// https://youtu.be/VIDEO_ID
	// https://youtube.com/watch?v=VIDEO_ID

	if strings.Contains(url, "youtube.com/watch") {
		parts := strings.Split(url, "v=")
		if len(parts) > 1 {
			id := parts[1]
			if idx := strings.Index(id, "&"); idx != -1 {
				id = id[:idx]
			}
			return id
		}
	}

	if strings.Contains(url, "youtu.be/") {
		parts := strings.Split(url, "youtu.be/")
		if len(parts) > 1 {
			id := parts[1]
			if idx := strings.Index(id, "?"); idx != -1 {
				id = id[:idx]
			}
			return id
		}
	}

	return ""
}

// extractTitleFromOutput tries to extract video title from vot-cli output
func extractTitleFromOutput(output string) string {
	// vot-cli may output the title, try to parse it
	// This is a simple implementation, may need adjustment based on actual output format
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Title:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Title:"))
		}
	}
	return ""
}

// IsVotCliAvailable checks if vot-cli is installed and accessible
func IsVotCliAvailable() bool {
	_, err := exec.LookPath("vot-cli")
	return err == nil
}

// ytdlpFormat represents a format from yt-dlp --dump-json output
type ytdlpFormat struct {
	FormatID   string  `json:"format_id"`
	Language   string  `json:"language"`
	Resolution string  `json:"resolution"`
	Ext        string  `json:"ext"`
	Abr        float64 `json:"abr"`
	Vcodec     string  `json:"vcodec"`
	Acodec     string  `json:"acodec"`
}

// ytdlpInfo represents video info from yt-dlp --dump-json
type ytdlpInfo struct {
	Formats []ytdlpFormat `json:"formats"`
}

// GetDubbedAudioTracks returns available dubbed audio tracks for a YouTube video.
// On cookie errors, retries without cookies as a fallback.
func (v *VoiceoverService) GetDubbedAudioTracks(ctx context.Context, videoURL string) ([]AudioTrack, error) {
	tracks, err := v.getDubbedAudioTracks(ctx, videoURL, true)
	if err != nil && v.CookiesFile != "" && ytfeed.IsCookieError(err.Error()) {
		log.Printf("[WARN] cookies expired, retrying GetDubbedAudioTracks without cookies")
		return v.getDubbedAudioTracks(ctx, videoURL, false)
	}
	return tracks, err
}

func (v *VoiceoverService) getDubbedAudioTracks(ctx context.Context, videoURL string, useCookies bool) ([]AudioTrack, error) {
	videoURL = normalizeYouTubeURL(videoURL)

	cmdCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "yt-dlp", v.ytdlpArgs(useCookies, "--dump-json", "--no-download", videoURL)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp dump-json failed: %w\nstderr: %s", err, stderr.String())
	}

	var info ytdlpInfo
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		return nil, fmt.Errorf("failed to parse yt-dlp output: %w", err)
	}

	var tracks []AudioTrack
	seen := make(map[string]bool)

	for _, f := range info.Formats {
		// Audio-only formats have vcodec=none and acodec != none
		if f.Vcodec != "none" || f.Acodec == "none" || f.Acodec == "" {
			continue
		}

		// Skip if no language info
		if f.Language == "" {
			continue
		}

		// Avoid duplicates (same language, keep best quality)
		key := f.Language
		if seen[key] {
			continue
		}
		seen[key] = true

		tracks = append(tracks, AudioTrack{
			FormatID: f.FormatID,
			Language: f.Language,
			Quality:  f.Ext,
			Bitrate:  int(f.Abr),
		})
	}

	log.Printf("[INFO] found %d audio tracks for %s", len(tracks), videoURL)
	for _, t := range tracks {
		log.Printf("[DEBUG] audio track: lang=%s format=%s bitrate=%d", t.Language, t.FormatID, t.Bitrate)
	}

	return tracks, nil
}

// FindDubbedTrack finds a dubbed track for the target language
func (v *VoiceoverService) FindDubbedTrack(tracks []AudioTrack) *AudioTrack {
	for _, t := range tracks {
		// Check for exact match or prefix match (e.g., "ru" matches "ru-RU")
		if t.Language == v.TargetLang || strings.HasPrefix(t.Language, v.TargetLang+"-") ||
			strings.HasPrefix(v.TargetLang, t.Language+"-") {
			return &t
		}
	}
	return nil
}

// DownloadDubbedTrack downloads a specific audio track using yt-dlp.
// On cookie errors, retries without cookies as a fallback.
func (v *VoiceoverService) DownloadDubbedTrack(ctx context.Context, videoURL string, track *AudioTrack) (*VoiceoverResult, error) {
	result, err := v.downloadDubbedTrack(ctx, videoURL, track, true)
	if err != nil && v.CookiesFile != "" && ytfeed.IsCookieError(err.Error()) {
		log.Printf("[WARN] cookies expired, retrying DownloadDubbedTrack without cookies")
		return v.downloadDubbedTrack(ctx, videoURL, track, false)
	}
	return result, err
}

func (v *VoiceoverService) downloadDubbedTrack(ctx context.Context, videoURL string, track *AudioTrack, useCookies bool) (*VoiceoverResult, error) {
	videoURL = normalizeYouTubeURL(videoURL)

	videoID := extractVideoID(videoURL)
	if videoID == "" {
		return nil, fmt.Errorf("could not extract video ID")
	}

	outputFile := filepath.Join(v.OutputDir, fmt.Sprintf("vo_%s_%d.mp3", videoID, time.Now().Unix()))

	// Download specific audio track and convert to mp3
	args := v.ytdlpArgs(useCookies,
		"-f", track.FormatID,
		"--extract-audio",
		"--audio-format", "mp3",
		"--audio-quality", "128K",
		"-o", outputFile,
		videoURL,
	)

	log.Printf("[INFO] downloading dubbed track (lang=%s, format=%s) for %s", track.Language, track.FormatID, videoURL)

	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "yt-dlp", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp download failed: %w\nstderr: %s", err, stderr.String())
	}

	// yt-dlp might add extension, find the actual file
	actualFile := outputFile
	if _, err := os.Stat(outputFile); os.IsNotExist(err) {
		// Try with .mp3 extension if not already
		if !strings.HasSuffix(outputFile, ".mp3") {
			actualFile = outputFile + ".mp3"
		}
	}

	fileInfo, err := os.Stat(actualFile)
	if err != nil {
		return nil, fmt.Errorf("downloaded file not found: %w", err)
	}

	if fileInfo.Size() == 0 {
		return nil, fmt.Errorf("downloaded file is empty")
	}

	log.Printf("[INFO] downloaded dubbed track: %s (size: %d bytes)", actualFile, fileInfo.Size())

	return &VoiceoverResult{
		FilePath: actualFile,
		FileSize: fileInfo.Size(),
	}, nil
}

package proc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	log "github.com/go-pkgz/lgr"
)

// SubtitleService handles downloading and parsing YouTube subtitles
type SubtitleService struct {
	OutputDir string
}

// NewSubtitleService creates a new subtitle service
func NewSubtitleService(outputDir string) *SubtitleService {
	return &SubtitleService{OutputDir: outputDir}
}

// DownloadSubtitles downloads subtitles for a YouTube video using yt-dlp
// Returns path to the subtitle file and detected language
func (s *SubtitleService) DownloadSubtitles(ctx context.Context, videoURL string) (string, string, error) {
	// Create temp filename based on video URL hash
	videoID := extractVideoID(normalizeYouTubeURL(videoURL))
	if videoID == "" {
		return "", "", fmt.Errorf("could not extract video ID")
	}

	outputTemplate := filepath.Join(s.OutputDir, fmt.Sprintf("sub_%s_%d", videoID, time.Now().Unix()))

	// Try to download subtitles with yt-dlp
	// Priority: manual English subs > auto-generated English > manual Russian > auto Russian
	args := []string{
		"--write-sub",
		"--write-auto-sub",
		"--sub-lang", "en,ru",
		"--sub-format", "vtt/srt/best",
		"--skip-download",
		"--no-playlist",
		"--extractor-args", "youtube:player_client=web_creator",
		"--output", outputTemplate,
		videoURL,
	}

	log.Printf("[INFO] downloading subtitles with args: %v", args)

	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "yt-dlp", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("yt-dlp subtitles failed: %w\nstderr: %s", err, stderr.String())
	}

	log.Printf("[DEBUG] yt-dlp subtitle stdout: %s", stdout.String())

	// Find the downloaded subtitle file
	pattern := outputTemplate + "*.vtt"
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		// Try SRT
		pattern = outputTemplate + "*.srt"
		matches, err = filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			return "", "", fmt.Errorf("no subtitle file found")
		}
	}

	subFile := matches[0]

	// Detect language from filename (e.g., sub_xxx.en.vtt or sub_xxx.ru.vtt)
	lang := "en"
	if strings.Contains(subFile, ".ru.") {
		lang = "ru"
	}

	log.Printf("[INFO] downloaded subtitles: %s (lang: %s)", subFile, lang)
	return subFile, lang, nil
}

// ParseSubtitles extracts plain text from VTT or SRT subtitle file
func (s *SubtitleService) ParseSubtitles(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read subtitle file: %w", err)
	}

	content := string(data)

	if strings.HasSuffix(strings.ToLower(filePath), ".vtt") {
		return parseVTT(content), nil
	}
	return parseSRT(content), nil
}

// parseVTT extracts text from WebVTT format
func parseVTT(content string) string {
	var lines []string
	var lastLine string

	scanner := bufio.NewScanner(strings.NewReader(content))

	// Skip header
	inCue := false

	// Regex to match timestamps like "00:00:00.000 --> 00:00:05.000"
	timestampRegex := regexp.MustCompile(`^\d{2}:\d{2}:\d{2}[.,]\d{3}\s*-->`)
	// Regex to match VTT tags like <c>, </c>, <00:00:00.000>
	tagRegex := regexp.MustCompile(`<[^>]+>`)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines, WEBVTT header, NOTE lines, and style blocks
		if line == "" || line == "WEBVTT" || strings.HasPrefix(line, "NOTE") ||
		   strings.HasPrefix(line, "STYLE") || strings.HasPrefix(line, "Kind:") ||
		   strings.HasPrefix(line, "Language:") {
			inCue = false
			continue
		}

		// Skip timestamp lines
		if timestampRegex.MatchString(line) {
			inCue = true
			continue
		}

		// Skip cue identifiers (lines that are just numbers or identifiers before timestamps)
		if !inCue {
			continue
		}

		// Remove VTT tags
		line = tagRegex.ReplaceAllString(line, "")
		line = strings.TrimSpace(line)

		if line == "" {
			continue
		}

		// Avoid duplicate consecutive lines (common in auto-generated subs)
		if line != lastLine {
			lines = append(lines, line)
			lastLine = line
		}
	}

	return strings.Join(lines, " ")
}

// parseSRT extracts text from SRT format
func parseSRT(content string) string {
	var lines []string
	var lastLine string

	scanner := bufio.NewScanner(strings.NewReader(content))

	// Regex to match SRT timestamps like "00:00:00,000 --> 00:00:05,000"
	timestampRegex := regexp.MustCompile(`^\d{2}:\d{2}:\d{2},\d{3}\s*-->`)
	// Regex to match sequence numbers
	seqRegex := regexp.MustCompile(`^\d+$`)
	// Regex to match HTML-like tags
	tagRegex := regexp.MustCompile(`<[^>]+>`)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines, sequence numbers, timestamps
		if line == "" || seqRegex.MatchString(line) || timestampRegex.MatchString(line) {
			continue
		}

		// Remove tags
		line = tagRegex.ReplaceAllString(line, "")
		line = strings.TrimSpace(line)

		if line == "" {
			continue
		}

		// Avoid duplicates
		if line != lastLine {
			lines = append(lines, line)
			lastLine = line
		}
	}

	return strings.Join(lines, " ")
}

// Cleanup removes the subtitle file
func (s *SubtitleService) Cleanup(filePath string) {
	if filePath != "" {
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] failed to cleanup subtitle file %s: %v", filePath, err)
		}
	}
}

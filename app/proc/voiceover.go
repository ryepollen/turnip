package proc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	log "github.com/go-pkgz/lgr"
)

// VoiceoverService handles YouTube video voice-over translation using vot-cli
type VoiceoverService struct {
	OutputDir  string
	TargetLang string
}

// NewVoiceoverService creates a new voiceover service
func NewVoiceoverService(outputDir, targetLang string) *VoiceoverService {
	if targetLang == "" {
		targetLang = "ru"
	}
	return &VoiceoverService{
		OutputDir:  outputDir,
		TargetLang: targetLang,
	}
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

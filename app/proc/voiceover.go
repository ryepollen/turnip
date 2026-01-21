package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
}

// TranslateVideo downloads voice-over translated audio for a YouTube video
func (v *VoiceoverService) TranslateVideo(ctx context.Context, videoURL string) (*VoiceoverResult, error) {
	// Create unique filename based on video ID and timestamp
	videoID := extractVideoID(videoURL)
	if videoID == "" {
		return nil, fmt.Errorf("could not extract video ID from URL")
	}

	outputFile := filepath.Join(v.OutputDir, fmt.Sprintf("vo_%s_%d.mp3", videoID, time.Now().Unix()))

	// Build vot-cli command
	// vot-cli --reslang ru --output /path/to/file.mp3 "https://youtube.com/watch?v=xxx"
	args := []string{
		"--reslang", v.TargetLang,
		"--output", outputFile,
		videoURL,
	}

	cmd := exec.CommandContext(ctx, "vot-cli", args...)
	cmd.Stderr = os.Stderr // Log errors

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("vot-cli failed: %w (output: %s)", err, string(output))
	}

	// Check if file was created
	if _, err := os.Stat(outputFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("vot-cli did not create output file")
	}

	// Try to extract title from output or use video ID
	title := extractTitleFromOutput(string(output))
	if title == "" {
		title = "Voice-over: " + videoID
	}

	return &VoiceoverResult{
		FilePath: outputFile,
		Title:    title,
		Duration: 0, // Will be determined from file later
	}, nil
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

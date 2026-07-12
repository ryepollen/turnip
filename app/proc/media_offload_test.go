package proc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockOffloader records calls; failUpload switches the failure path
type mockOffloader struct {
	failUpload bool
	uploaded   []string
	deleted    []string
	total      int64
}

func (m *mockOffloader) UploadMedia(_ context.Context, _, basename string) (string, error) {
	if m.failUpload {
		return "", fmt.Errorf("r2 down")
	}
	m.uploaded = append(m.uploaded, basename)
	return "https://pub/m/s/" + basename, nil
}
func (m *mockOffloader) DeleteMedia(_ context.Context, basename string) error {
	m.deleted = append(m.deleted, basename)
	return nil
}
func (m *mockOffloader) TotalSize(context.Context) (int64, error) { return m.total, nil }

func TestOffloadMediaSyncRemovesLocalOnSuccess(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "episode.mp3")
	require.NoError(t, os.WriteFile(file, []byte("audio"), 0o600))

	mo := &mockOffloader{}
	bot := &TelegramBot{Media: mo}

	require.True(t, bot.offloadMediaSync(context.Background(), file))
	assert.Equal(t, []string{"episode.mp3"}, mo.uploaded)
	_, err := os.Stat(file)
	assert.True(t, os.IsNotExist(err), "local copy removed after offload")
}

func TestOffloadMediaSyncKeepsLocalOnFailure(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "episode.mp3")
	require.NoError(t, os.WriteFile(file, []byte("audio"), 0o600))

	mo := &mockOffloader{failUpload: true}
	bot := &TelegramBot{Media: mo}

	require.False(t, bot.offloadMediaSync(context.Background(), file))
	_, err := os.Stat(file)
	assert.NoError(t, err, "local copy must survive a failed upload")
}

func TestR2UsageLine(t *testing.T) {
	bot := &TelegramBot{}
	assert.Equal(t, "", bot.r2UsageLine(), "disabled without offloader")

	bot.Media = &mockOffloader{total: 3 << 30}
	assert.Equal(t, "☁️ R2: 3.0 GB / 10 GB", bot.r2UsageLine())
}

func TestLLMRateLine(t *testing.T) {
	// reset shared state
	llmRate.mu.Lock()
	llmRate.remaining, llmRate.limit = "", ""
	llmRate.mu.Unlock()

	assert.Equal(t, "", llmRateLine(), "empty before the first LLM call")

	h := make(map[string][]string)
	h["X-Ratelimit-Remaining-Tokens"] = []string{"34000"}
	h["X-Ratelimit-Limit-Tokens"] = []string{"100000"}
	captureRateHeaders(h)
	line := llmRateLine()
	assert.Contains(t, line, "34000")
	assert.Contains(t, line, "100000")
	assert.Contains(t, line, "только что")

	captureRateHeaders(map[string][]string{"Content-Type": {"application/json"}})
	assert.Contains(t, llmRateLine(), "34000", "headers without rate info don't wipe the snapshot")
}

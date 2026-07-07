//go:build integration

// Integration test against the real R2 bucket. Run with:
//
//	R2_ACCOUNT_ID=... R2_ACCESS_KEY_ID=... R2_SECRET_ACCESS_KEY=... \
//	R2_BUCKET=... R2_PUBLIC_BASE_URL=... \
//	go test -tags integration ./app/publisher -run TestIntegrationR2 -v
package publisher

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationR2UploadDownloadDelete(t *testing.T) {
	cfg := R2Config{
		AccountID:     os.Getenv("R2_ACCOUNT_ID"),
		AccessKeyID:   os.Getenv("R2_ACCESS_KEY_ID"),
		SecretKey:     os.Getenv("R2_SECRET_ACCESS_KEY"),
		Bucket:        os.Getenv("R2_BUCKET"),
		PublicBaseURL: os.Getenv("R2_PUBLIC_BASE_URL"),
	}
	if !cfg.Enabled() {
		t.Skip("R2_* env not set")
	}

	store, err := NewR2Store(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	f := filepath.Join(t.TempDir(), "hello.mp3")
	require.NoError(t, os.WriteFile(f, []byte("turnip r2 integration test payload"), 0o600))

	key := "a/" + RandomSecret() + "/hello.mp3"
	publicURL, err := store.Upload(ctx, f, key, "")
	require.NoError(t, err)
	t.Logf("uploaded: %s", publicURL)

	exists, err := store.Exists(ctx, key)
	require.NoError(t, err)
	assert.True(t, exists)

	// the public dev URL must serve the exact bytes
	resp, err := http.Get(publicURL) //nolint:gosec,noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "turnip r2 integration test payload", string(body))
	assert.Equal(t, "audio/mpeg", resp.Header.Get("Content-Type"))

	require.NoError(t, store.Delete(ctx, key))
	exists, err = store.Exists(ctx, key)
	require.NoError(t, err)
	assert.False(t, exists, "deleted object must be gone")
}

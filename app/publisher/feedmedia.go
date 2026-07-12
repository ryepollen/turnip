package publisher

import (
	"context"
	"path/filepath"
	"strings"
)

// FeedMedia offloads the main podcast feed's episodes (YouTube, translations,
// articles) to R2 under m/{secret}/{basename}. The VM then serves /yt/media
// as a redirect instead of streaming gigabytes through GCP egress. R2 here is
// a transit buffer, not storage: /del and auto-cleanup remove objects, and
// everything is re-fetchable (links live in the history log).
type FeedMedia struct {
	Store  *R2Store
	Secret string
}

// key builds the R2 object key for a media file
func (m *FeedMedia) key(basename string) string {
	return "m/" + m.Secret + "/" + filepath.Base(basename)
}

// UploadMedia uploads one episode file, returns its public URL
func (m *FeedMedia) UploadMedia(ctx context.Context, localPath, basename string) (string, error) {
	return m.Store.Upload(ctx, localPath, m.key(basename), "audio/mpeg")
}

// DeleteMedia removes an episode object (best-effort companion to /del)
func (m *FeedMedia) DeleteMedia(ctx context.Context, basename string) error {
	return m.Store.Delete(ctx, m.key(basename))
}

// TotalSize reports the whole bucket usage (books + media share the 10GB tier)
func (m *FeedMedia) TotalSize(ctx context.Context) (int64, error) {
	return m.Store.TotalSize(ctx)
}

// PublicBase is the redirect prefix for the API server: /yt/media/{file} ->
// {PublicBase}/{file}
func (m *FeedMedia) PublicBase() string {
	return strings.TrimRight(m.Store.publicBase, "/") + "/m/" + m.Secret
}

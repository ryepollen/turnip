// Package publisher implements the personal audio publishing platform:
// R2 object storage for audio files, per-category RSS feeds, and the
// originals-processing pipeline. The VM serves only kilobyte-sized feed XML;
// players download audio straight from R2 (zero-egress).
package publisher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// R2Store uploads audio files to a Cloudflare R2 bucket via the S3 API.
// Podcast players cannot authenticate to enclosures, so privacy relies on
// unguessable paths: every upload goes under a random secret segment.
type R2Store struct {
	client     *minio.Client
	bucket     string
	publicBase string // https://pub-....r2.dev or a custom domain
}

// R2Config collects the credentials (from env, see secrets.env)
type R2Config struct {
	AccountID     string
	AccessKeyID   string
	SecretKey     string
	Bucket        string
	PublicBaseURL string
}

// Enabled reports whether the config is complete
func (c R2Config) Enabled() bool {
	return c.AccountID != "" && c.AccessKeyID != "" && c.SecretKey != "" && c.Bucket != "" && c.PublicBaseURL != ""
}

// NewR2Store creates the R2 client
func NewR2Store(cfg R2Config) (*R2Store, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("incomplete R2 config")
	}
	endpoint := fmt.Sprintf("%s.r2.cloudflarestorage.com", cfg.AccountID)
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretKey, ""),
		Secure: true,
		Region: "auto",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create R2 client: %w", err)
	}
	return &R2Store{
		client:     client,
		bucket:     cfg.Bucket,
		publicBase: strings.TrimRight(cfg.PublicBaseURL, "/"),
	}, nil
}

// newR2StoreForEndpoint is the test hook: plain-HTTP custom endpoint
func newR2StoreForEndpoint(endpoint string, cfg R2Config) (*R2Store, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretKey, ""),
		Secure: false,
		Region: "auto",
	})
	if err != nil {
		return nil, err
	}
	return &R2Store{client: client, bucket: cfg.Bucket, publicBase: strings.TrimRight(cfg.PublicBaseURL, "/")}, nil
}

// RandomSecret returns an unguessable path segment (16 hex chars)
func RandomSecret() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// Upload puts a local file under the given key and returns its public URL.
// contentType defaults to audio/mpeg when empty.
func (s *R2Store) Upload(ctx context.Context, localPath, key, contentType string) (publicURL string, err error) {
	if contentType == "" {
		contentType = "audio/mpeg"
	}
	upCtx, cancel := context.WithTimeout(ctx, 60*time.Minute) // audiobooks are gigabytes
	defer cancel()

	if _, err := s.client.FPutObject(upCtx, s.bucket, key, localPath, minio.PutObjectOptions{
		ContentType: contentType,
	}); err != nil {
		return "", fmt.Errorf("failed to upload %s: %w", key, err)
	}
	return s.PublicURL(key), nil
}

// PublicURL builds the public link for a stored key
func (s *R2Store) PublicURL(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return s.publicBase + "/" + strings.Join(parts, "/")
}

// Delete removes an object
func (s *R2Store) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("failed to delete %s: %w", key, err)
	}
	return nil
}

// TotalSize sums the size of every object in the bucket (the R2 free tier is
// 10GB for the whole bucket, so books and feed media count together)
func (s *R2Store) TotalSize(ctx context.Context) (int64, error) {
	var total int64
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return 0, fmt.Errorf("failed to list bucket: %w", obj.Err)
		}
		total += obj.Size
	}
	return total, nil
}

// Exists checks whether a key is present (size > 0)
func (s *R2Store) Exists(ctx context.Context, key string) (bool, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" || errResp.StatusCode == 404 {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat %s: %w", key, err)
	}
	return info.Size > 0, nil
}

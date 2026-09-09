// Package duration provides a duration of audio from file or reader
package duration

import (
	"io"
	"net/http"
	"os"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/tcolgate/mp3"
)

// Service provides duration of audio from file or reader
type Service struct{}

// File scans MP3 file from provided file and returns its duration in seconds, ignoring possible errors
func (s *Service) File(fname string) int {
	fh, err := os.Open(fname) //nolint:gosec // this is not an inclusion as file was created by us
	if err != nil {
		log.Printf("[WARN] can't get duration, failed to open file %s: %v", fname, err)
		return 0
	}
	defer fh.Close() // nolint
	return s.reader(fh)
}

// URL fetches an audio file over HTTP and scans it for duration in seconds,
// streaming the body straight into the mp3 frame scanner (no temp file). Used by
// the variant-A Finalize path, where the audio lives in R2, not on local disk —
// the caller pays (free R2) egress but keeps zero bytes on the VM. Errors are
// logged and yield 0, matching File's best-effort contract.
func (s *Service) URL(url string) int {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url) //nolint:noctx // best-effort duration probe, bounded by the client timeout
	if err != nil {
		log.Printf("[WARN] can't get duration, failed to fetch %s: %v", url, err)
		return 0
	}
	defer resp.Body.Close() // nolint
	if resp.StatusCode != http.StatusOK {
		log.Printf("[WARN] can't get duration, status %d for %s", resp.StatusCode, url)
		return 0
	}
	return s.reader(resp.Body)
}

// reader scans MP3 from provided file and returns its duration in seconds, ignoring possible errors
func (s *Service) reader(r io.Reader) int {
	d := mp3.NewDecoder(r)

	var f mp3.Frame
	var skipped int
	var duration float64
	var err error

	for err == nil {
		if err = d.Decode(&f, &skipped); err != nil && err != io.EOF {
			log.Printf("[WARN] can't get duration for provided stream: %v", err)
			return 0
		}
		duration += f.Duration().Seconds()
	}
	return int(duration)
}

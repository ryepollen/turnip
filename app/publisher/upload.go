package publisher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Web ingest (variant B): the Mini App uploads a work's parts straight to R2
// with presigned PUTs, then commits — the server verifies the objects landed
// and writes the 0-byte stub skeleton under originals/{category}/{work}/, so
// the catalog, Finalize and the feeds see the work exactly as if its files
// were local. iCloud stays the master archive; no machine is ever on duty.

// presignExpiry is generous on purpose: one batch's URLs are signed up front,
// and a big book on a slow uplink may still be uploading hours later.
const presignExpiry = 12 * time.Hour

// uploadKey is the deterministic R2 key for a flat part name — the same layout
// partKey derives for catalog parts, so Finalize finds what the browser put.
func (p *Service) uploadKey(category, work, name string) string {
	return fmt.Sprintf("a/%s/%s/%s/%s", p.Secret, category, work, name)
}

// checkUploadName screens one part filename for the stub skeleton and the R2
// key: a plain basename, no path tricks, a known audio extension. Season
// subfolders are not a web-ingest feature — upload seasons as separate works
// or lay them out in iCloud and ingest flat.
func checkUploadName(name string) error {
	if name == "" || len(name) > 200 {
		return fmt.Errorf("bad file name length")
	}
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("bad file name %q", name)
	}
	if mimeForFile(name) == "" {
		return fmt.Errorf("not a supported audio file: %q", name)
	}
	return nil
}

// checkUploadTarget screens the category (must be an existing library dir —
// the web must not mint new public feed slugs) and the work folder name.
func (p *Service) checkUploadTarget(category, work string) error {
	if err := validCategory(category); err != nil {
		return err
	}
	if fi, err := os.Stat(p.categoryDir(category)); err != nil || !fi.IsDir() {
		return fmt.Errorf("unknown category %q", category)
	}
	if work == "" || len(work) > 150 || strings.ContainsAny(work, `/\`) ||
		strings.HasPrefix(work, ".") || work != strings.TrimSpace(work) {
		return fmt.Errorf("bad work name %q", work)
	}
	return nil
}

// UploadMime is the enclosure MIME type the browser should send with its PUT
// ("" = not audio); it mirrors what the generated feed will claim later.
func UploadMime(name string) string { return mimeForFile(name) }

// CategoryDirs lists the existing category directories under originals/ — the
// only valid ingest targets (a feed slug is a public URL, so the set is fixed
// on disk, never minted from the web).
func (p *Service) CategoryDirs() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(p.AudioDir, "originals"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cats []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			cats = append(cats, e.Name())
		}
	}
	return cats, nil
}

// PresignUploadPart signs a direct browser→R2 PUT for one part of a work.
func (p *Service) PresignUploadPart(ctx context.Context, category, work, name string) (string, error) {
	if err := p.checkUploadTarget(category, work); err != nil {
		return "", err
	}
	if err := checkUploadName(name); err != nil {
		return "", err
	}
	return p.R2.PresignPut(ctx, p.uploadKey(category, work, name), presignExpiry)
}

// VerifyUploaded HEADs every named part in R2 and returns the names still
// missing (absent or zero bytes). An empty result authorizes RegisterWork.
func (p *Service) VerifyUploaded(ctx context.Context, category, work string, names []string) ([]string, error) {
	if err := p.checkUploadTarget(category, work); err != nil {
		return nil, err
	}
	var missing []string
	for _, n := range names {
		if err := checkUploadName(n); err != nil {
			return nil, err
		}
		size, err := p.R2.Size(ctx, p.uploadKey(category, work, n))
		if err != nil {
			return nil, err
		}
		if size == 0 {
			missing = append(missing, n)
		}
	}
	return missing, nil
}

// RegisterWork writes the 0-byte stub skeleton for an uploaded work, turning it
// into a dormant library entry the next catalog scan picks up. Existing files
// are left untouched — re-committing only adds new names, and a real original
// can never be truncated from here.
func (p *Service) RegisterWork(category, work string, names []string) error {
	if err := p.checkUploadTarget(category, work); err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("no files to register")
	}
	for _, n := range names {
		if err := checkUploadName(n); err != nil {
			return err
		}
	}
	dir := filepath.Join(p.categoryDir(category), work)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, n := range names {
		path := filepath.Join(dir, n)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			return err
		}
	}
	return nil
}

package api

import (
	"context"
	"time"

	"golang.org/x/sys/unix"
)

// storage levels for the pulse line: the VM volume holding /srv/var and the
// R2 bucket against its free-tier cap — so «когда пора удалять» is visible
// before anything runs out, not after.

// r2FreeTierBytes is Cloudflare's free allowance; the pulse shows usage
// against it (paid growth just makes the meter read past the cap).
const r2FreeTierBytes = int64(10) << 30

// diskUsage reports used/total bytes of the filesystem holding path (0,0 on
// any error — the pulse simply omits the meter).
func diskUsage(path string) (used, total int64) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := int64(st.Bsize) //nolint:unconvert // Bsize is int64 on darwin, uint32 on linux
	total = int64(st.Blocks) * bs
	free := int64(st.Bavail) * bs
	return total - free, total
}

// r2Usage sums the bucket's object sizes (one LIST per call; the pulse loads
// once per app open, and ops are far inside the free tier).
func (s *Server) r2Usage() int64 {
	if s.Pub == nil || s.Pub.R2 == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	size, err := s.Pub.R2.TotalSize(ctx)
	if err != nil {
		return 0
	}
	return size
}

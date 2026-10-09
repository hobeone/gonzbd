package fsutil

import (
	"os"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// ShrinkAndSync makes f's bytes durable and trims it to end: fsync, truncate
// only if end > 0 and the file is longer than end, fsync again. Every error is
// a *storagefault.Fault. The first fsync lands the written bytes before the
// size changes; the second lands the new size. A file is never grown, and
// end <= 0 leaves its size alone.
//
// sync is the fsync to use, so a caller can route it through its own seam.
func ShrinkAndSync(f *os.File, end int64, sync func(*os.File) error) error {
	path := f.Name()
	if err := sync(f); err != nil {
		return storagefault.Classify("sync", path, err)
	}
	if end > 0 {
		fi, err := f.Stat()
		if err != nil {
			return storagefault.Classify("stat", path, err)
		}
		if fi.Size() > end {
			if err := f.Truncate(end); err != nil {
				return storagefault.Classify("truncate", path, err)
			}
		}
	}
	if err := sync(f); err != nil {
		return storagefault.Classify("sync", path, err)
	}
	return nil
}

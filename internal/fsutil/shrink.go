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
	if _, err := shrinkTo(f, end); err != nil {
		return err
	}
	if err := sync(f); err != nil {
		return storagefault.Classify("sync", path, err)
	}
	return nil
}

// ShrinkAfterSync is ShrinkAndSync for a file whose bytes an earlier
// successful fsync already landed, with nothing written to it since: it
// truncates as ShrinkAndSync does, and fsyncs only when it truncated, since
// otherwise nothing has changed for an fsync to land. Every error is a
// *storagefault.Fault.
func ShrinkAfterSync(f *os.File, end int64, sync func(*os.File) error) error {
	shrunk, err := shrinkTo(f, end)
	if err != nil || !shrunk {
		return err
	}
	if err := sync(f); err != nil {
		return storagefault.Classify("sync", f.Name(), err)
	}
	return nil
}

// shrinkTo truncates f to end when end > 0 and f is longer, and reports
// whether it did. It never grows a file. Every error is a *storagefault.Fault.
func shrinkTo(f *os.File, end int64) (bool, error) {
	if end > 0 {
		fi, err := f.Stat()
		if err != nil {
			return false, storagefault.Classify("stat", f.Name(), err)
		}
		if fi.Size() > end {
			if err := f.Truncate(end); err != nil {
				return false, storagefault.Classify("truncate", f.Name(), err)
			}
			return true, nil
		}
	}
	return false, nil
}

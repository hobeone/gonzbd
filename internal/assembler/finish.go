package assembler

import (
	"fmt"
	"os"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// finish makes a file's bytes durable and trims its preallocated tail: fsync,
// truncate to the end of the last owned byte, fsync again. A caller reports the
// file complete only after it returns nil. Nothing calls it yet.
//
// The sequence is fsutil.ShrinkAndSync, shared with the restart verifier, run
// through w.syncFile so a test can inject a device error. A file with no owned
// range is left at its preallocated size rather than truncated to zero, and a
// file already no longer than its last owned end is not grown. A returned error
// is a *storagefault.Fault, classified as on every other writer path.
//
// It takes no lock: it reads w.owned and w.handle, which the worker goroutine
// owns, and does its I/O without holding anything. The caller must be that
// goroutine.
func (w *FileWriter) finish() error {
	err := fsutil.ShrinkAndSync(w.handle, w.owned.maxEnd(), func(*os.File) error { return w.syncFile() })
	if err != nil {
		return fmt.Errorf("finish %s: %w", w.path, err)
	}
	return nil
}

package assembler

import "fmt"

// finish makes a file's bytes durable and trims its preallocated tail: fsync,
// truncate to the end of the last owned byte, fsync again. A caller reports the
// file complete only after it returns nil. Nothing calls it yet.
//
// The first fsync lands the written bytes before the size changes; the second
// lands the new size. A file with no owned range is left at its preallocated
// size rather than truncated to zero, and a file already no longer than its
// last owned end is not grown (Truncate refuses to grow).
//
// Errors are classified by Sync and Truncate like any other writer path.
//
// It takes no lock: it reads w.owned and w.handle, which the worker goroutine
// owns, and does its I/O without holding anything. The caller must be that
// goroutine.
func (w *FileWriter) finish() error {
	if err := w.Sync(); err != nil {
		return fmt.Errorf("finish %s: first fsync: %w", w.path, err)
	}
	if end := w.owned.maxEnd(); end > 0 {
		if err := w.Truncate(end); err != nil {
			return fmt.Errorf("finish %s: truncate to %d: %w", w.path, end, err)
		}
	}
	if err := w.Sync(); err != nil {
		return fmt.Errorf("finish %s: second fsync: %w", w.path, err)
	}
	return nil
}

// Package storagefault classifies errors returned by the storage layer.
//
// It deliberately has no concept of an article, job, or file index, and
// imports no gonzbd package. That is not an accident of layering: invariant
// A1 of the durability design forbids recording a storage fault as an article
// fault, and this package cannot violate it because it has no vocabulary in
// which to blame an article.
package storagefault

import "fmt"

// Fault is a classified failure of the storage layer.
type Fault struct {
	// Op is the syscall-level operation: "write", "sync", "open",
	// "truncate", "mkdir", "stat".
	Op string
	// Path is the filesystem path the operation targeted.
	Path string
	// Err is the underlying error, retained for errors.Is/As.
	Err error
	// Permanent reports whether retrying could ever succeed without
	// operator intervention.
	Permanent bool
}

func (f *Fault) Error() string {
	kind := "retryable"
	if f.Permanent {
		kind = "permanent"
	}
	return fmt.Sprintf("storage %s fault on %s %q: %v", kind, f.Op, f.Path, f.Err)
}

// Retryable reports whether the condition may clear on its own or after
// operator action, in which case the job stalls rather than fails.
func (f *Fault) Retryable() bool { return !f.Permanent }

func (f *Fault) Unwrap() error { return f.Err }

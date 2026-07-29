//go:build unix

package testutil

import "syscall"

// withoutConcurrentFork runs fn while holding syscall.ForkLock for
// reading, which prevents any fork+exec in this process from starting
// until fn returns. Callers use it to keep a write descriptor on a file
// they are about to exec from leaking into an unrelated child process,
// which would make that exec fail with ETXTBSY. See WriteExecutable for
// the full mechanism.
//
// fn must not itself fork or exec: ForkLock is a sync.RWMutex and the
// fork path takes it for writing, so that would deadlock.
func withoutConcurrentFork(fn func()) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fn()
}

//go:build !unix

package testutil

// withoutConcurrentFork runs fn directly. ETXTBSY is a Unix errno and
// syscall.ForkLock is defined only on Unix; Windows CreateProcess does
// not duplicate the parent's descriptor table the way fork does, so
// there is no window to close. See WriteExecutable.
func withoutConcurrentFork(fn func()) { fn() } //nocover: non-Unix stub, not compiled on the platforms CI measures

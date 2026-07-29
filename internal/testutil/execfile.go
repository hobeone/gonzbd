package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteExecutable writes content to path with mode 0o755, creating any
// missing parent directories, and returns path. The mode is applied
// independently of the process umask, so the file is always executable.
// It is safe to call from tests running under t.Parallel() that
// subsequently exec the written file.
//
// The naive form of this helper — open, write, close, then exec — fails
// the subsequent exec intermittently with ETXTBSY ("text file busy"),
// even though the writing descriptor is definitively closed first. The
// kernel returns ETXTBSY from execve whenever *any* process holds a
// descriptor open for writing on that inode, and a concurrent fork+exec
// anywhere else in the test binary duplicates our still-open write
// descriptor into its child, where it survives until that child reaches
// execve: O_CLOEXEC takes effect at execve, not at fork. Closing our own
// descriptor first is therefore not sufficient, because the offending
// descriptor belongs to someone else's child. See golang/go#22315.
//
// withoutConcurrentFork holds syscall.ForkLock for reading across the
// open→close window, which blocks any fork+exec in this process for its
// duration. That is exactly the protocol documented at
// syscall/exec_unix.go:32 — "the create new fd/mark close-on-exec
// operation is done holding ForkLock for reading, and the fork itself is
// done holding ForkLock for writing."
//
// The file is deliberately not fsync'd. fsync does nothing for ETXTBSY,
// since execve reads through the same page cache the write landed in,
// and an fsync inside the ForkLock window would stall every fork in the
// test binary for the length of a disk flush.
func WriteExecutable(t testing.TB, path, content string) string {
	t.Helper()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("WriteExecutable: mkdir %q: %v", dir, err)
	}

	var err error
	withoutConcurrentFork(func() {
		err = writeAndClose(path, content, os.OpenFile)
	})
	if err != nil {
		t.Fatalf("WriteExecutable %q: %v", path, err)
	}
	return path
}

// openFunc matches the signature of os.OpenFile. writeAndClose takes one
// as a parameter rather than reading a package-level variable, so that
// tests can drive its failure branches without mutating shared state that
// a parallel test could observe mid-flight.
type openFunc func(name string, flag int, perm os.FileMode) (*os.File, error)

// writeAndClose creates path with mode 0o755 via open, writes content, and
// closes it. It performs no fork or exec of its own: it runs while ForkLock
// is held for reading, and sync.RWMutex is not reentrant, so forking here
// would deadlock against the fork's own write-lock acquisition.
func writeAndClose(path, content string, open openFunc) error {
	f, err := open(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	// open's perm argument is masked by the process umask, so it does not
	// actually guarantee 0o755: under umask 0o027 the file lands at 0o750,
	// and under a umask carrying an execute bit it lands non-executable --
	// which would defeat the whole purpose, since every caller execs this
	// file. Chmod the descriptor to make the mode umask-independent. Using
	// the descriptor rather than the path avoids re-resolving it.
	if err := f.Chmod(0o755); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

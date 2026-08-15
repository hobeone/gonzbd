package storagefault

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestClassify_NilErrorYieldsNoFault(t *testing.T) {
	if got := Classify("write", "/x", nil); got != nil {
		t.Fatalf("Classify(nil) = %v, want nil", got)
	}
}

func TestClassify_Retryability(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"ENOSPC is retryable", syscall.ENOSPC, false},
		{"EDQUOT is retryable", syscall.EDQUOT, false},
		{"ETIMEDOUT is retryable", syscall.ETIMEDOUT, false},
		{"ESTALE is retryable", syscall.ESTALE, false},
		{"EIO is retryable — transient on network volumes", syscall.EIO, false},
		{"ENOENT is retryable — a missing dir is recoverable", syscall.ENOENT, false},
		{"EBUSY is retryable by omission", syscall.EBUSY, false},
		{"EAGAIN is retryable by omission", syscall.EAGAIN, false},
		{"EROFS is permanent", syscall.EROFS, true},
		{"ENOTDIR is permanent", syscall.ENOTDIR, true},
		{"EACCES is permanent", syscall.EACCES, true},
		{"EPERM is permanent", syscall.EPERM, true},
		{"EBADF is permanent", syscall.EBADF, true},
		{"EFBIG is permanent", syscall.EFBIG, true},
		{"EINVAL is permanent", syscall.EINVAL, true},
		{"unknown defaults to retryable", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := Classify("write", "/data/x.bin", tt.err)
			if f == nil {
				t.Fatal("Classify returned nil for a non-nil error")
			}
			if f.Permanent != tt.permanent {
				t.Errorf("Permanent = %v, want %v", f.Permanent, tt.permanent)
			}
			if !errors.Is(f, tt.err) {
				t.Errorf("errors.Is(fault, %v) = false, want true", tt.err)
			}
		})
	}
}

// TestClassify_WrappedErrnoIsUnwrapped guards the real call shape: the
// assembler passes an *os.PathError from WriteAt, never a bare errno. It
// wraps a permanent errno (EROFS) rather than a retryable one, so that a
// Classify implementation which compares the wrapper directly instead of
// unwrapping it (e.g. `err == error(e)`) fails this test instead of passing
// it vacuously — retryable is the default outcome for an error Classify
// doesn't recognize, so a wrapped retryable errno would "pass" even if
// unwrapping were deleted entirely.
func TestClassify_WrappedErrnoIsUnwrapped(t *testing.T) {
	wrapped := &os_PathErrorStub{Err: syscall.EROFS}
	f := Classify("write", "/data/x.bin", wrapped)
	if !f.Permanent {
		t.Error("wrapped EROFS classified retryable, want permanent")
	}
}

//nolint:revive // deliberately unidiomatic: the name signals a stand-in for *os.PathError
type os_PathErrorStub struct{ Err error }

func (e *os_PathErrorStub) Error() string { return "stub: " + e.Err.Error() }
func (e *os_PathErrorStub) Unwrap() error { return e.Err }

func TestFault_Error(t *testing.T) {
	tests := []struct {
		name      string
		permanent bool
		wantKind  string
	}{
		{"retryable fault", false, "retryable"},
		{"permanent fault", true, "permanent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			underlying := errors.New("disk exploded")
			f := &Fault{Op: "write", Path: "/data/x.bin", Err: underlying, Permanent: tt.permanent}
			got := f.Error()
			if !strings.Contains(got, tt.wantKind) {
				t.Errorf("Error() = %q, want it to contain kind %q", got, tt.wantKind)
			}
			if !strings.Contains(got, "write") {
				t.Errorf("Error() = %q, want it to contain op %q", got, "write")
			}
			if !strings.Contains(got, "/data/x.bin") {
				t.Errorf("Error() = %q, want it to contain path %q", got, "/data/x.bin")
			}
			if !strings.Contains(got, underlying.Error()) {
				t.Errorf("Error() = %q, want it to contain underlying error %q", got, underlying.Error())
			}
		})
	}
}

func TestPackageImportsNoGonzbdPackage(t *testing.T) {
	const self = "github.com/hobeone/gonzbd/internal/storagefault"
	out, err := exec.Command("go", "list", "-deps", self).Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	// go list -deps always includes the target package itself in its
	// output, so self is excluded: the invariant under test is that
	// storagefault imports no *other* gonzbd package, not that it is
	// absent from its own dependency closure.
	for line := range strings.SplitSeq(string(out), "\n") {
		if line != self && strings.HasPrefix(line, "github.com/hobeone/gonzbd/") {
			t.Errorf("storagefault must not depend on %s — invariant A1 relies on it having no article vocabulary", line)
		}
	}
}

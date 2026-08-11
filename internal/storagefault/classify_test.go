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
		{"EROFS is permanent", syscall.EROFS, true},
		{"EIO is permanent", syscall.EIO, true},
		{"ENOENT is permanent", syscall.ENOENT, true},
		{"EACCES is permanent", syscall.EACCES, true},
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
			if f.Retryable() == tt.permanent {
				t.Errorf("Retryable() = %v, contradicts Permanent = %v", f.Retryable(), f.Permanent)
			}
			if !errors.Is(f, tt.err) {
				t.Errorf("errors.Is(fault, %v) = false, want true", tt.err)
			}
		})
	}
}

// TestClassify_WrappedErrnoIsUnwrapped guards the real call shape: the
// assembler passes an *os.PathError from WriteAt, never a bare errno.
func TestClassify_WrappedErrnoIsUnwrapped(t *testing.T) {
	wrapped := &os_PathErrorStub{Err: syscall.ENOSPC}
	f := Classify("write", "/data/x.bin", wrapped)
	if f.Permanent {
		t.Error("wrapped ENOSPC classified permanent, want retryable")
	}
}

//nolint:revive // deliberately unidiomatic: the name signals a stand-in for *os.PathError
type os_PathErrorStub struct{ Err error }

func (e *os_PathErrorStub) Error() string { return "stub: " + e.Err.Error() }
func (e *os_PathErrorStub) Unwrap() error { return e.Err }

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

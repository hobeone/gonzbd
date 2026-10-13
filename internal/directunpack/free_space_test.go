package directunpack

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// A set whose declared size does not fit above MinFreeSpace is refused before
// anything is extracted, and its failure is classified as disk full. A
// MinFreeSpace no filesystem has free makes every set not fit.
func TestDirectUnpack_RefusesASetThatDoesNotFit(t *testing.T) {
	srcDir := testdataDir(t)
	workDir := t.TempDir()
	extractDir := t.TempDir()
	vol := copyRAR(t, srcDir, workDir, "single_rar5.rar")

	du := New(testLogger(t), "test-job", workDir, extractDir, Options{MinFreeSpace: int64(1) << 62})
	du.SetAllFilenames([]string{"single_rar5.rar"})
	du.Add(t.Context(), "single_rar5.rar", vol)
	waitReturns(t, du, 10*time.Second)

	f, ok := du.Failures()["single_rar5"]
	if !ok {
		t.Fatalf("single_rar5 did not fail: results %v", du.Results())
	}
	if f.Kind != unpack.FailDiskFull || !strings.Contains(f.Reason, unpack.ErrInsufficientSpace.Error()) {
		t.Errorf("failure = %+v, want Kind %v and a reason naming %q", f, unpack.FailDiskFull, unpack.ErrInsufficientSpace)
	}
	if got := dirNames(t, extractDir); len(got) != 0 {
		t.Errorf("a refused set left %q in the extract dir", got)
	}
}

// The same set extracts when it fits, so the refusal above is the bound and
// not some other failure.
func TestDirectUnpack_ExtractsASetThatFits(t *testing.T) {
	srcDir := testdataDir(t)
	workDir := t.TempDir()
	vol := copyRAR(t, srcDir, workDir, "single_rar5.rar")

	du := New(testLogger(t), "test-job", workDir, t.TempDir(), Options{MinFreeSpace: 1})
	du.SetAllFilenames([]string{"single_rar5.rar"})
	du.Add(t.Context(), "single_rar5.rar", vol)
	waitReturns(t, du, 10*time.Second)

	if _, ok := du.Results()["single_rar5"]; !ok {
		t.Errorf("single_rar5 was not extracted: failures %v", du.Failures())
	}
}

// A failure that filled the disk is classified as disk full whatever wraps
// it; any other failure is not.
func TestSettleLocked_ClassifiesDiskFull(t *testing.T) {
	cases := []struct {
		err  error
		want unpack.FailReason
	}{
		{fmt.Errorf("directunpack: extract f: %w", &os.PathError{Op: "write", Path: "f", Err: syscall.ENOSPC}), unpack.FailDiskFull},
		{fmt.Errorf("refused: %w", unpack.ErrInsufficientSpace), unpack.FailDiskFull},
		{errors.New("bad CRC"), unpack.FailUnknown},
	}
	for _, c := range cases {
		d := New(slog.New(slog.DiscardHandler), "job", "", "", Options{})
		d.settleLocked("s", SuccessSet{}, c.err)
		if got := d.failedSets["s"].Kind; got != c.want {
			t.Errorf("Kind for %v = %v, want %v", c.err, got, c.want)
		}
	}
}

// leadingVolumes stops at the first volume not yet added, so the free-space
// bound reads only volumes that are complete on disk.
func TestLeadingVolumes(t *testing.T) {
	d := New(slog.New(slog.DiscardHandler), "job", "", "", Options{})
	d.completedVols["s"] = map[int]string{1: "v1", 2: "v2", 4: "v4"}
	if got := d.leadingVolumes("s"); strings.Join(got, ",") != "v1,v2" {
		t.Errorf("leadingVolumes = %q, want [v1 v2]", got)
	}
	if got := d.leadingVolumes("none"); len(got) != 0 {
		t.Errorf("leadingVolumes of an unknown set = %q, want none", got)
	}
}

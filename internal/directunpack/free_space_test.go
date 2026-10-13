package directunpack

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
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

// The bound counts every volume on disk from volume 1, not volume 1 alone.
// late_member's second member starts in volume 3 and declares 256 MiB, so
// with every volume added first it is counted; volume 1 alone declares only
// the first member's 10000 bytes, which fit.
func TestDirectUnpack_BoundCountsEveryLeadingVolume(t *testing.T) {
	srcDir := testdataDir(t)
	workDir := t.TempDir()
	extractDir := t.TempDir()
	var names []string
	for i := 1; i <= 6; i++ {
		name := fmt.Sprintf("late_member.part%d.rar", i)
		copyRAR(t, srcDir, workDir, name)
		names = append(names, name)
	}
	free, err := assembler.FreeBytes(t.Context(), extractDir)
	if err != nil {
		t.Fatalf("FreeBytes: %v", err)
	}
	const margin = 128 << 20 // between the first member's size and the set's
	if free < 2*margin {
		t.Skipf("%d bytes free; the test needs %d", free, 2*margin)
	}

	du := New(testLogger(t), "test-job", workDir, extractDir, Options{MinFreeSpace: free - 10000 - margin})
	du.SetAllFilenames(names)
	for _, name := range append(names[1:], names[0]) { // volume 1 last: every volume is on disk when it starts
		du.Add(t.Context(), name, filepath.Join(workDir, name))
	}
	waitReturns(t, du, 10*time.Second)

	f, ok := du.Failures()["late_member"]
	if !ok || f.Kind != unpack.FailDiskFull {
		t.Errorf("Failures()[late_member] = %+v, %v; want a disk-full refusal counting the member that starts in volume 3", f, ok)
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

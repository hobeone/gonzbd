package durability

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// writeTrimFixture puts a file of n bytes on disk and returns its path.
func writeTrimFixture(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "A.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, n), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func sizeOf(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return fi.Size()
}

// TestTrimToRuns_CutsPreallocationsTail pins the ordinary case: a file longer
// than its runs account for is cut back to the last durable byte.
//
// The two runs are deliberately NOT merged into one. boundOver takes the
// maximum end offset rather than a sum, which is what lets the bound span a
// permanently failed article's hole instead of stopping at it — a sum over
// these two rows would answer 200 and cut away 100 bytes that are on disk.
func TestTrimToRuns_CutsPreallocationsTail(t *testing.T) {
	path := writeTrimFixture(t, 500)

	bound, err := TrimToRuns(path, []Run{
		{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0, Offset: 0, Length: 100},
		{FileIdx: 0, FirstArtIdx: 2, LastArtIdx: 2, Offset: 200, Length: 100},
	})
	if err != nil {
		t.Fatalf("TrimToRuns: %v", err)
	}
	if bound != 300 {
		t.Errorf("bound = %d, want 300 — the highest end offset over the runs, not their sum", bound)
	}
	if got := sizeOf(t, path); got != 300 {
		t.Errorf("file is %d bytes, want 300", got)
	}
}

// TestTrimToRuns_NeverGrowsAFile is the guard that matters most, because the
// failure it prevents is silent and the record then vouches for it.
//
// os.File.Truncate EXTENDS with zeros when the size exceeds the file's, so a
// bound above the file's length would manufacture content and the caller would
// go on to mark the file complete over it. §3.4's resume gate normally makes
// this unreachable — it establishes size >= max(Offset+Length) before adopting
// a file — but this function does not get to assume its caller ran that gate.
func TestTrimToRuns_NeverGrowsAFile(t *testing.T) {
	path := writeTrimFixture(t, 100)

	bound, err := TrimToRuns(path, []Run{
		{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 1, Offset: 0, Length: 400},
	})
	if err != nil {
		t.Fatalf("TrimToRuns: %v", err)
	}
	if bound != 0 {
		t.Errorf("bound = %d, want 0 — nothing was applied, and a non-zero return would "+
			"tell the caller a trim happened", bound)
	}
	if got := sizeOf(t, path); got != 100 {
		t.Errorf("file is %d bytes, want 100 — the runs claim more than is on disk, and "+
			"padding it with zeros would hand par2 content no article wrote while the "+
			"record says the file is accounted for", got)
	}
}

// TestTrimToRuns_NoRunsTrimsNothing pins the bound == 0 arm.
//
// A file whose every article failed permanently has no run at all. Truncating
// to 0 would cut away a file the record cannot account for, which is the
// opposite of conservative — the same reason FinalizeFile guards its own
// truncate on bound > 0.
func TestTrimToRuns_NoRunsTrimsNothing(t *testing.T) {
	path := writeTrimFixture(t, 250)

	bound, err := TrimToRuns(path, nil)
	if err != nil {
		t.Fatalf("TrimToRuns: %v", err)
	}
	if bound != 0 {
		t.Errorf("bound = %d, want 0", bound)
	}
	if got := sizeOf(t, path); got != 250 {
		t.Errorf("file is %d bytes, want 250 untouched", got)
	}
}

// TestTrimToRuns_ExactSizeIsANoOp pins that a file already at its bound is
// neither an error nor reported as trimmed. This is the common case on a
// second start after the repair has already run once, so it must be cheap and
// silent rather than logged as a repair every time.
func TestTrimToRuns_ExactSizeIsANoOp(t *testing.T) {
	path := writeTrimFixture(t, 200)

	bound, err := TrimToRuns(path, []Run{
		{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 1, Offset: 0, Length: 200},
	})
	if err != nil {
		t.Fatalf("TrimToRuns: %v", err)
	}
	if bound != 0 {
		t.Errorf("bound = %d, want 0 — nothing needed doing", bound)
	}
	if got := sizeOf(t, path); got != 200 {
		t.Errorf("file is %d bytes, want 200", got)
	}
}

// TestTrimToRuns_MissingFileIsAnError pins that a path that is not there is
// reported rather than swallowed. The caller marks the file complete on a nil
// error, so a silent success here would claim a file that does not exist.
func TestTrimToRuns_MissingFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.bin")

	if _, err := TrimToRuns(path, []Run{
		{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0, Offset: 0, Length: 100},
	}); err == nil {
		t.Error("TrimToRuns returned nil for a file that does not exist; its caller reads " +
			"nil as permission to record the file complete")
	}
}

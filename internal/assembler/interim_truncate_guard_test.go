package assembler

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResumeWithoutExtentSkipsTruncate pins the interim guard that stands in
// for the persisted high-water mark between its removal from job_files and the
// arrival of a verified extent.
//
// The fixture is #342/#350 exactly, minus the seed that used to defuse it: an
// earlier run wrote 20 bytes, this run is handed the one still-outstanding
// article at offset 0, and neither seed carries the earlier extent because
// nothing persists it any more. finalizeFile's high-water mark is therefore 4,
// the extent of what this run received, and truncating to it destroys sixteen
// bytes no later stage can recover when the job has no par2.
//
// The assertion is deliberately "did not shrink" rather than an exact size.
// The guard's contract is that it never trims on this path, not that it leaves
// any particular number of trailing zeros — pre-allocation decides that, and
// pinning the exact figure would make this a change-detector for the allocator.
func TestResumeWithoutExtentSkipsTruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job1_0.dat")

	prior := []byte("0123456789ABCDEFGHIJ")
	if err := os.WriteFile(path, prior, 0o644); err != nil {
		t.Fatalf("seed prior run's file: %v", err)
	}

	files := map[string]FileInfo{
		"job1:0": {
			Path:       path,
			TotalParts: 1, // only the still-unfinished article
			// Both seeds zero: job_files no longer stores either figure.
			InitialMaxWritten:  0,
			InitialWriteCursor: 0,
			// The pipeline withheld the articles an earlier run completed.
			ResumedFile: true,
		},
	}
	opts := makeOpts(dir, files)
	done := make(chan struct{}, 1)
	opts.OnFileComplete = func(string, int, uint32) { done <- struct{}{} }

	a := startAssembler(t, opts)
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, Offset: 0, Data: []byte("ZZZZ"),
	}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	<-done
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	got := readFile(t, path)
	if len(got) < len(prior) {
		t.Fatalf("file shrank to %d bytes, want at least %d — a resumed file with "+
			"no trustworthy extent was truncated to this run's high-water mark, "+
			"destroying what earlier runs wrote (#342/#350)", len(got), len(prior))
	}
	if want := "ZZZZ456789ABCDEFGHIJ"; string(got[:len(prior)]) != want {
		t.Errorf("first %d bytes = %q, want %q", len(prior), got[:len(prior)], want)
	}
}

// TestFreshFileStillTruncates is the other half of the guard: it must not fire
// for a file that was never resumed, or every download would keep
// pre-allocation's trailing zeros and par2 would report damage on all of them.
// ResumedFile is false here and everything else matches the test above.
func TestFreshFileStillTruncates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job2_0.dat")

	files := map[string]FileInfo{
		"job2:0": {
			Path:         path,
			TotalParts:   1,
			ExpectedSize: 64, // pre-allocate well above the decoded extent
			ResumedFile:  false,
		},
	}
	opts := makeOpts(dir, files)
	done := make(chan struct{}, 1)
	opts.OnFileComplete = func(string, int, uint32) { done <- struct{}{} }

	a := startAssembler(t, opts)
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job2", FileIdx: 0, Offset: 0, Data: []byte("ZZZZ"),
	}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	<-done
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := readFile(t, path); len(got) != 4 {
		t.Errorf("fresh file is %d bytes, want 4 — the interim resume guard must "+
			"not suppress the truncate that removes pre-allocation's zeros", len(got))
	}
}

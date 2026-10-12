package app

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestCompleteFinalizedFile_EvictedAfterTheResidencyCheckIsMarked pins #868:
// an eviction that lands after the completion's settle, peek and feed ran on a
// resident job, and before the mark, does not lose the completion. The mark
// lives on the progress record, which eviction keeps, so the file is marked,
// its complete=1 reaches the record, and the job's download is reported
// complete.
func TestCompleteFinalizedFile_EvictedAfterTheResidencyCheckIsMarked(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionOff, nil, false,
		[]peekFile{{"payload.bin", []byte("payload")}})
	if err := seedJobFiles(t.Context(), a.durable, a.j.ID(), a.j.NumFiles(), a.j.FileFetchPolicy); err != nil {
		t.Fatalf("seedJobFiles: %v", err)
	}
	fired := false
	a.residentCheckedHook = func(j *job.Job) {
		fired = true
		j.Evict()
	}
	var reported []string
	a.downloadReportedHook = func(id string) { reported = append(reported, id) }

	if err := a.completeFinalizedFile(FileComplete{JobID: a.j.ID(), FileIdx: 0}); err != nil {
		t.Fatalf("completeFinalizedFile: %v", err)
	}
	if !fired {
		t.Fatal("the hook never ran; the test exercises nothing")
	}
	if a.j.Resident() {
		t.Fatal("the job is still resident; the test exercises nothing")
	}
	if !a.j.Progress().FileComplete(0) {
		t.Error("file 0 is not complete: the completion was lost to an eviction after every step that needs the manifest had run")
	}
	if err := a.recorder.flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	files, err := a.durable.FileRows(t.Context(), a.j.ID())
	if err != nil {
		t.Fatalf("FileRows: %v", err)
	}
	if len(files) != 1 || !files[0].Complete {
		t.Errorf("job_files = %+v, want file 0 complete", files)
	}
	if len(reported) != 1 || reported[0] != a.j.ID() {
		t.Errorf("download-complete reports = %v, want one for %s", reported, a.j.ID())
	}
}

// TestCompleteFinalizedFile_EvictedBeforeTheResidencyCheckIsNotMarked pins
// the other half of #868: the settle, the peek and the DirectUnpack feed read
// the manifest and do nothing for an evicted job, so a completion that finds
// the job evicted before the mark leaves the file unmarked and reports
// nothing. The next start's verification finishes the file by path.
func TestCompleteFinalizedFile_EvictedBeforeTheResidencyCheckIsNotMarked(t *testing.T) {
	t.Parallel()
	a := newPeekApp(t, unwanted.ActionOff, nil, false,
		[]peekFile{{"payload.bin", []byte("payload")}})
	a.peekedHook = func() { a.j.Evict() }
	reported := false
	a.downloadReportedHook = func(string) { reported = true }

	err := a.completeFinalizedFile(FileComplete{JobID: a.j.ID(), FileIdx: 0})
	if !errors.Is(err, job.ErrNotResident) {
		t.Fatalf("completeFinalizedFile = %v, want job.ErrNotResident", err)
	}
	if a.j.Progress().FileComplete(0) {
		t.Error("file 0 is complete although the completion's settle, peek and feed found the job evicted")
	}
	if reported {
		t.Error("the download was reported complete for a completion the job did not take")
	}
}

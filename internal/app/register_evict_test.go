package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestRegisterFile_EvictedAfterCountRecordsFilename pins #854: an eviction
// that lands after registerFile counted the file's unfinished articles and
// before it recorded the file's filename must not leave the file registered
// with no filename recorded. Eviction drops only the manifest, and the
// filename lives on the always-resident progress record, so the filename is
// recorded and the dirty mark that persists it reads it.
func TestRegisterFile_EvictedAfterCountRecordsFilename(t *testing.T) {
	t.Parallel()
	a := newTestApplication(t)
	disp, j := helperJob(t, a, "evictwindow", 1, 2)
	p := helperPipeline(t, disp)

	fired := false
	p.countedHook = func(j *job.Job) {
		fired = true
		j.Evict()
	}
	dirty := "unmarked"
	p.markDirty = func(j *job.Job, fileIdx int) {
		dirty = j.Progress().FileFilename(fileIdx)
	}

	if err := p.registerFile(j, 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}
	if !fired {
		t.Fatal("the hook never ran; the test exercises nothing")
	}
	if j.Resident() {
		t.Fatal("the job is still resident; the test exercises nothing")
	}
	info, err := p.resolveFileInfo(j.ID(), 0)
	if err != nil {
		t.Fatalf("resolveFileInfo: %v", err)
	}
	if got := j.Progress().FileFilename(0); got != info.Name {
		t.Errorf("recorded filename %q, want the registered %q: a restart finds no filename and deletes the file's progress", got, info.Name)
	}
	if dirty != info.Name {
		t.Errorf("the dirty mark read filename %q, want the registered %q", dirty, info.Name)
	}
	if !j.DownloadBegun() {
		t.Error("DownloadBegun is false with a file registered, so a rename would move the job away from its files")
	}
}

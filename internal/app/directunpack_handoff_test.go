package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestFail_FromFetching_AbortsTheDirectUnpack: a job handed to
// post-processing before its download finished gets no more files, so its
// DirectUnpacker, part-way through a multi-volume set, would wait for a volume
// that never arrives. The hand-over aborts it instead, and the job is
// post-processed and its admission ends.
func TestFail_FromFetching_AbortsTheDirectUnpack(t *testing.T) {
	t.Parallel()
	stage := finishedStage()
	application, j := launchedAppAt(t, stage, job.Fetching)
	id := j.ID()
	if j.IsComplete() {
		t.Fatal("precondition: the Fetching job's download is already complete")
	}
	du := waitingDirectUnpack(t, application, id)

	fault := storagefault.Classify("write", "/mnt/ro/held.bin", syscall.EROFS)
	application.Fail(id, fault)
	awaitAdmissionsEnded(t, application)

	select {
	case <-du.Done():
	default:
		t.Error("the DirectUnpacker is still running after the job left post-processing")
	}
	if n := len(application.postProcessor.History()); n != 1 {
		t.Errorf("the post-processor finished %d copies of the job, want 1", n)
	}
	entry := historyEntry(t, application, id)
	if want := "Failed: " + fault.Error(); entry.Status != "Failed" || entry.FailMessage != want {
		t.Errorf("history Status, FailMessage = %q, %q, want Failed, %q", entry.Status, entry.FailMessage, want)
	}
}

// TestHandOver_AfterTheDownloadFinished_KeepsTheDirectUnpackResults: a job
// whose download finished has every volume its DirectUnpacker will get, so
// the hand-over waits for it rather than aborting it, and the post-processor
// is given what it extracted. An abort clears the unpacker's results even after
// it has finished, so an aborting hand-over loses them.
func TestHandOver_AfterTheDownloadFinished_KeepsTheDirectUnpackResults(t *testing.T) {
	t.Parallel()
	stage := finishedStage()
	application, j := launchedAppAt(t, stage, job.Extracting)
	id := j.ID()
	if !j.IsComplete() {
		t.Fatal("precondition: the Extracting job's download is not complete")
	}

	const volumes = 10
	names := make([]string, volumes)
	for i := range names {
		names[i] = fmt.Sprintf("multi_new.part%02d.rar", i+1)
	}
	duDir := t.TempDir()
	du := directunpack.New(slog.New(slog.DiscardHandler), id, duDir, t.TempDir(), directunpack.Options{})
	du.SetAllFilenames(names)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join("../unpack/testdata", name))
		if err != nil {
			t.Fatalf("read test archive: %v", err)
		}
		path := filepath.Join(duDir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		du.Add(t.Context(), name, path)
	}
	t.Cleanup(du.Abort)
	select {
	case <-du.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the DirectUnpacker never finished extracting the set")
	}
	if len(du.Results()) == 0 {
		t.Fatalf("precondition: the DirectUnpacker extracted nothing; failures %v", du.Failures())
	}
	application.duOrch.inject(id, du)

	application.maybeFinalize(id, "")
	awaitAdmissionsEnded(t, application)

	hist := application.postProcessor.History()
	if len(hist) != 1 {
		t.Fatalf("the post-processor finished %d copies of the job, want 1", len(hist))
	}
	if len(hist[0].DirectUnpackSets) == 0 {
		t.Errorf("the post-processor was given no DirectUnpack results (failures %v): the "+
			"hand-over aborted the unpacker of a job whose download had finished",
			hist[0].DirectUnpackFailures)
	}
}

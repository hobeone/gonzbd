package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestFail_FromFetching_AbortsTheDirectUnpack: a job handed to
// post-processing before its download finished gets no more files, and the
// hand-over aborts its DirectUnpacker rather than awaiting it. The unpacker is
// held inside a set's extraction (waitingDirectUnpack), so only the abort ends
// it: the job is post-processed and its admission ends.
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
	application.Fail(j, fault)
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
// is given what it extracted. The unpacker is still extracting the set at the
// hand-over, because an abort keeps a set already published: only the set an
// abort interrupts is lost. It has every volume, as a finished download's
// unpacker has, and is held inside the set's extraction by its OnLine
// callback, which runs after each member is written and before the set is
// published.
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
	extracting, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	du := directunpack.New(slog.New(slog.DiscardHandler), id, duDir, t.TempDir(), directunpack.Options{
		OnLine: func(string) {
			once.Do(func() {
				close(extracting)
				<-release
			})
		},
	})
	du.SetAllFilenames(names)
	paths := make([]string, volumes)
	for i, name := range names {
		data, err := os.ReadFile(filepath.Join("../unpack/testdata", name))
		if err != nil {
			t.Fatalf("read test archive: %v", err)
		}
		paths[i] = filepath.Join(duDir, name)
		if err := os.WriteFile(paths[i], data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(du.Abort)
	var releaseOnce sync.Once
	releaseExtraction := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseExtraction) // runs before du.Abort, which waits for run
	for i := range volumes {
		du.Add(t.Context(), names[i], paths[i])
	}
	application.duOrch.inject(id, du)
	select {
	case <-extracting:
	case <-time.After(10 * time.Second):
		t.Fatal("the DirectUnpacker never reached the set's first member")
	}

	application.maybeFinalizeJob(j, "")
	// An abort sets killed, which Status reports as inactive at once, while
	// run is still held in OnLine.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !du.Status().Active {
			t.Fatalf("the hand-over stopped the unpacker of a job whose download had finished (status %+v)",
				du.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	releaseExtraction()
	select {
	case <-du.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the DirectUnpacker never finished extracting the set")
	}
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

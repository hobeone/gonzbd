package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// rarVolumes is the multi-volume RAR5 set under internal/unpack/testdata.
const rarVolumes = 10

// unpackerForTest returns the DirectUnpacker the orchestrator holds for id, or
// nil.
func (o *directUnpackOrchestrator) unpackerForTest(id string) *directunpack.DirectUnpacker {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.unpackers[id]
}

// fetchingRarApp builds an Application with DirectUnpack enabled and one job
// of rarVolumes RAR volumes, launched at Fetching under a holdingRunner so
// nothing reports it, with every volume written to the download directory and
// none complete. Its post-processing stage holds a job until the test ends, so
// the job stays registered after a hand-off.
func fetchingRarApp(t *testing.T) (*Application, *job.Job) {
	t.Helper()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	application, repo, _ := newLifecycleTestApp(t, WithPostProcStages([]postproc.Stage{stage}))
	application.config.With(func(c *config.Config) {
		c.PostProc.DirectUnpack = true
		c.PostProc.EnableUnrar = true
	})
	application.ctx = t.Context()
	runner := holdingRunner{launched: make(chan string, 4)}
	d := dispatch.New(
		1, 1, 10*time.Millisecond, time.Now,
		&appWorkers{app: application},
		application.residency,
		dispatchstore.New(repo.DB(), nil),
		runner,
	)
	application.dispatcher = d
	application.pipeline.dispatcher = d
	t.Cleanup(application.duOrch.abortAll)

	parsed := &nzb.NZB{}
	data := make([][]byte, rarVolumes)
	for i := range rarVolumes {
		name := fmt.Sprintf("multi_new.part%02d.rar", i+1)
		b, err := os.ReadFile(filepath.Join("../unpack/testdata", name))
		if err != nil {
			t.Fatalf("read test archive: %v", err)
		}
		data[i] = b
		parsed.Files = append(parsed.Files, nzb.File{
			Subject:  name,
			Bytes:    int64(len(b)),
			Articles: []nzb.Article{{ID: fmt.Sprintf("v%d@t", i), Bytes: len(b), Number: 1}},
		})
	}
	j, hdr, err := BuildIngestJob(application.config, parsed, "race.nzb", types.FetchOptions{NzbName: "race"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	hdr.PP = 3
	if err := j.BeginAttempt(time.Now()); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	id := j.ID()
	if err := d.Add(t.Context(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { stopHeldDispatcher(d) })
	if err := application.postProcessor.Start(t.Context()); err != nil {
		t.Fatalf("postProcessor.Start: %v", err)
	}
	t.Cleanup(func() { _ = application.postProcessor.Stop() })
	// Registered last, so it runs first: the held stage lets go before the
	// post-processor and the dispatcher stop.
	t.Cleanup(func() { close(stage.finish) })
	select {
	case <-runner.launched:
	case <-time.After(10 * time.Second):
		t.Fatal("the job was never launched at Fetching")
	}

	dir := filepath.Join(application.config.GetGeneral().DownloadDir, j.Name())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for i, f := range parsed.Files {
		path := filepath.Join(dir, f.Subject)
		if err := os.WriteFile(path, data[i], 0o600); err != nil {
			t.Fatal(err)
		}
		application.pipeline.mu.Lock()
		application.pipeline.fileInfo[fileKey{jobID: id, fileIdx: i}] = assembler.FileInfo{Path: path}
		application.pipeline.mu.Unlock()
	}
	return application, j
}

// completeFile runs the completion of file i of id that follows its finalize.
func completeFile(t *testing.T, application *Application, id string, i int) {
	t.Helper()
	if err := application.completeFinalizedFile(FileComplete{JobID: id, FileIdx: i}); err != nil {
		t.Errorf("completeFinalizedFile(%d): %v", i, err)
	}
}

// TestHandOff_LastFileCompletingAtTheCollect_LeavesNoUnpackerAwaitedWithoutAVolume:
// a hand-off that does not wait for the download-finished report (Fail, a
// hopeless callback) reads whether the download finished and collects the
// job's DirectUnpacker, and the completion of the job's last file can land
// between the two. The unpacker the hand-off collected must then either have
// every volume or be aborted: one awaited with a volume missing never
// finishes, and holds the job's admission until removal or shutdown.
func TestHandOff_LastFileCompletingAtTheCollect_LeavesNoUnpackerAwaitedWithoutAVolume(t *testing.T) {
	t.Parallel()
	application, j := fetchingRarApp(t)
	id := j.ID()
	for i := range rarVolumes - 1 {
		completeFile(t, application, id, i)
	}
	du := application.duOrch.unpackerForTest(id)
	if du == nil {
		t.Fatal("precondition: the completed volumes started no DirectUnpacker")
	}
	application.directUnpackCollectHook = func(string) {
		completeFile(t, application, id, rarVolumes-1)
	}

	application.maybeFinalize(id, "")

	select {
	case <-du.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("the collected DirectUnpacker never finished: it was awaited without "+
			"the last volume (status %+v)", du.Status())
	}
	aborted := false
	for _, f := range du.Failures() {
		if strings.HasPrefix(f.Reason, "aborted") {
			aborted = true
		}
	}
	if !aborted && len(du.Results()) == 0 {
		t.Errorf("the collected DirectUnpacker was neither aborted nor extracted the set; failures %v",
			du.Failures())
	}
}

// TestHandOff_CompletionAfterTheCollect_StartsNoUnpacker: a completion already
// in flight when a hand-off collects the job's DirectUnpacker reaches the
// orchestrator after the collect. It must not start a fresh unpacker: nothing
// collects one started after the hand-off's collect, and only RemoveJob and
// Shutdown would abort it.
func TestHandOff_CompletionAfterTheCollect_StartsNoUnpacker(t *testing.T) {
	t.Parallel()
	application, j := fetchingRarApp(t)
	id := j.ID()
	for i := range 3 {
		completeFile(t, application, id, i)
	}
	if application.duOrch.unpackerForTest(id) == nil {
		t.Fatal("precondition: the completed volumes started no DirectUnpacker")
	}
	info, err := application.pipeline.resolveFileInfo(id, 3)
	if err != nil {
		t.Fatalf("precondition: %v", err)
	}

	// No failure reason, so the stages run and the held stage keeps the job
	// registered: a reason skips them, and the finalizer deregisters it.
	application.maybeFinalize(id, "")
	if !application.postProcAdmissions.has(j) {
		t.Fatal("precondition: the hand-off did not admit the job")
	}
	// The hand-off has dropped the job's file paths (pipeline.forgetJob). A
	// completion in flight across the hand-off resolved its path before
	// that, so it reaches the orchestrator holding one: put it back.
	application.pipeline.mu.Lock()
	application.pipeline.fileInfo[fileKey{jobID: id, fileIdx: 3}] = info
	application.pipeline.mu.Unlock()
	completeFile(t, application, id, 3)

	if du := application.duOrch.unpackerForTest(id); du != nil {
		t.Errorf("a completion after the hand-off's collect started a DirectUnpacker "+
			"nothing collects (status %+v)", du.Status())
	}
}

// TestRemoveJob_CompletionAfterTheAbort_StartsNoUnpacker: RemoveJob aborts the
// job's DirectUnpacker before its dispatcher.Remove, and the job is still
// registered in between, so a completion can reach the orchestrator there. It
// must not start a fresh unpacker: RemoveJob aborts the job's unpacker only
// once, so one started after that abort runs until Shutdown.
//
// The completion lands before RemoveJob's dispatcher cancel, while the job
// still holds its lease and so its manifest: past the cancel, a tick can
// evict the manifest, and maybeStart then returns before it decides anything.
func TestRemoveJob_CompletionAfterTheAbort_StartsNoUnpacker(t *testing.T) {
	t.Parallel()
	application, j := fetchingRarApp(t)
	id := j.ID()
	for i := range 3 {
		completeFile(t, application, id, i)
	}
	if application.duOrch.unpackerForTest(id) == nil {
		t.Fatal("precondition: the completed volumes started no DirectUnpacker")
	}

	hookRan := false
	application.removeAbortGapHook = func(string) {
		hookRan = true
		if application.duOrch.unpackerForTest(id) != nil {
			t.Error("precondition: the job's DirectUnpacker survived RemoveJob's abort")
		}
		completeFile(t, application, id, 3)
	}

	if err := application.RemoveJob(t.Context(), id, false); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	if !hookRan {
		t.Fatal("precondition: RemoveJob never reached its DirectUnpack abort")
	}

	if du := application.duOrch.unpackerForTest(id); du != nil {
		t.Errorf("a completion after RemoveJob's abort started a DirectUnpacker "+
			"nothing aborts (status %+v)", du.Status())
	}
	application.duOrch.mu.Lock()
	active := application.duOrch.active
	application.duOrch.mu.Unlock()
	if active != 0 {
		t.Errorf("DirectUnpack active count after RemoveJob = %d, want 0", active)
	}
}

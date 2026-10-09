package assembler

import (
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestCloseJobHandles_TombstonesEvenWhenTheDrainFailed pins the close arm's
// tombstones in the FAILING direction, which is the direction the per-file one
// was briefly broken in.
//
// The arm deletes the file from open unconditionally. Were the file then in
// neither open nor completed, and the job not tombstoned either, an article
// already in flight would fall through to openTargetFile — MkdirAll,
// OpenFile(O_CREATE), preallocate — re-creating a file the job has handed to
// post-processing, with an fd nothing closes: the control message is sent once
// per admission. The job would reappear in OpenJobIDs, the checkpoint loop
// would barrier it, and on NFS the handle held across post-processing's unlink
// is the .nfsXXXX silly-rename the arm exists to prevent.
//
// Gating either tombstone on success protects no re-dispatch: the caller,
// enqueuePostProc, admits the job first, and the downloader does not dispatch
// an admitted job.
func TestCloseJobHandles_TombstonesEvenWhenTheDrainFailed(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	a.opts.OnArticlesUnwritten = func(string, int, []int32) {}

	f := newHelperFile(t, dir, "job_0.dat", 0)
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	f.w.syncFile = func() error { return syscall.EIO }

	ack := make(chan error, 1)
	cancelledJobs := map[string]struct{}{}
	a.dispatchRequest(
		WriteRequest{JobID: "", FileIdx: fileIdxCloseHandles, MessageID: "job", ackCh: ack},
		open, completed, cancelledJobs)

	if _, tombstoned := completed[key]; !tombstoned {
		t.Error("a file whose close-time sync failed was not tombstoned, so it sits " +
			"in neither open nor completed. An article still in flight now reaches " +
			"openTargetFile and re-creates a file the job has handed to " +
			"post-processing, leaking the fd that CloseJobHandles exists to release")
	}
	if _, tombstoned := cancelledJobs["job"]; !tombstoned {
		t.Error("a job whose close-time sync failed was not tombstoned, so a late " +
			"article for a file it never opened creates one under the post-processor")
	}
	if _, still := open[key]; still {
		t.Error("the file was left in the open map after its handles were closed")
	}
}

// TestCloseJobHandles_ArmSendsTheCloseTimeFaultOnTheAck is the other half.
// The arm once acked with a bare close, so the fault it had just computed was
// never sent and enqueuePostProc handed the job to par2, unrar and cleanup over
// a file whose unsynced bytes never reached the platter. The only trace was a
// Warn inside drainAndClose.
//
// It pins the SEND, and only the send: it drives dispatchRequest directly and
// reads the ack itself, so it never calls CloseJobHandles. Under its previous
// name — ReportsACloseTimeFailureToItsCaller — that read as end-to-end
// coverage, and it was not: the receiver discarded the value with
// `case <-ack: return nil` for as long as this test was green. The receive
// half is one line in CloseJobHandles, and the reason it has no test of its
// own is recorded there.
func TestCloseJobHandles_ArmSendsTheCloseTimeFaultOnTheAck(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	a.opts.OnArticlesUnwritten = func(string, int, []int32) {}

	f := newHelperFile(t, dir, "job_0.dat", 0)
	open := map[fileKey]*openFile{{jobID: "job", fileIdx: 0}: f}

	f.w.syncFile = func() error { return syscall.EIO }

	ack := make(chan error, 1)
	a.dispatchRequest(
		WriteRequest{JobID: "", FileIdx: fileIdxCloseHandles, MessageID: "job", ackCh: ack},
		open, map[fileKey]struct{}{}, map[string]struct{}{})

	err := <-ack
	if _, ok := errors.AsType[*storagefault.Fault](err); !ok {
		t.Fatalf("the ack carried %v, want the close-time fault — enqueuePostProc is "+
			"about to hand this job to par2, unrar and cleanup over bytes that never "+
			"reached the platter", err)
	}
}

// TestCloseJobHandles_TombstonesTheWholeJob: an article that arrives after the
// close, for a file the job never opened, must not create that file. Only the
// job-level tombstone covers it — the per-file one is keyed on files the arm
// closed — and openTargetFile creates and preallocates with no queue check.
// The article was dispatched before the hand-off, so nothing on the downloader
// side can stop it.
func TestCloseJobHandles_TombstonesTheWholeJob(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 2)
	neverOpened := registerFile(t, dir, files, "job1", 1, 1)

	a := startAssembler(t, makeOpts(dir, files))
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("data"),
	})
	if err := a.CloseJobHandles(t.Context(), "job1"); err != nil {
		t.Fatalf("CloseJobHandles: %v", err)
	}
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 1, ArtIdx: 2, Offset: 0, Data: []byte("late"),
	})
	_ = a.Stop()

	if _, err := os.Stat(neverOpened); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) = %v, want not-exist — an article arriving after the "+
			"job's handles were closed created a file under the post-processor, and "+
			"nothing will close the handle it opened", neverOpened, err)
	}
}

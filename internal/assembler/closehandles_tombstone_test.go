package assembler

import (
	"errors"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestCloseJobHandles_TombstonesEvenWhenTheDrainFailed pins a guard that has
// to hold in the FAILING direction, which is the direction it was briefly
// broken in.
//
// The -2 arm deletes the file from open unconditionally. If the tombstone were
// gated on the close succeeding, the key would then be in NEITHER map, and an
// article already in flight when the fault hit would miss both of
// processRequest's guards and fall through to openTargetFile — which does
// MkdirAll, OpenFile(O_CREATE) and preallocate, re-creating a file the job has
// already handed to post-processing. Nothing closes the handle it opens: this
// control message is one-shot behind SetPostProcStarted's PostProc guard. So
// the fd leaks, the job reappears in OpenJobIDs and the checkpoint loop
// barriers a job in post-processing, and on NFS the handle held across
// post-processing's unlink is the .nfsXXXX silly-rename this whole function
// exists to prevent. The reopened writer's partsWritten restarts at zero, so
// that file can never complete again.
//
// The re-dispatch such a gate would protect cannot happen: this arm's only
// production caller is maybeFinalize, which calls SetPostProcStarted first,
// and ForEachUnfinishedArticle skips a job with PostProc set.
func TestCloseJobHandles_TombstonesEvenWhenTheDrainFailed(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	a.opts.OnArticlesUnwritten = func(string, int, []int32) {}

	wc := newWriteCache(1 << 20)
	f := newHelperFile(t, dir, "job_0.dat", 0)
	f.w.wc = wc
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	if !a.handleSuccessArticle(f, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 0, MessageID: "a", Offset: 0, Data: []byte("AAAA"),
	}) {
		t.Fatal("the article was not accepted, so the fixture never buffered it")
	}
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.ENOSPC }

	ack := make(chan error, 1)
	a.dispatchRequest(
		WriteRequest{JobID: "", FileIdx: -2, MessageID: "job", ackCh: ack},
		open, completed, map[string]struct{}{}, wc)

	if _, tombstoned := completed[key]; !tombstoned {
		t.Error("a file whose close-time drain failed was not tombstoned, so it sits " +
			"in neither open nor completed. An article still in flight now reaches " +
			"openTargetFile and re-creates a file the job has handed to " +
			"post-processing, leaking the fd that CloseJobHandles exists to release")
	}
	if _, still := open[key]; still {
		t.Error("the file was left in the open map after its handles were closed")
	}
}

// TestCloseJobHandles_ReportsACloseTimeFailureToItsCaller is the other half.
// The arm computed the error and acked with a bare close, so CloseJobHandles
// returned nil regardless and maybeFinalize handed the job to par2, unrar and
// cleanup over a file whose buffered bytes never reached the platter. The only
// trace was a Warn inside drainAndClose.
func TestCloseJobHandles_ReportsACloseTimeFailureToItsCaller(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	a.opts.OnArticlesUnwritten = func(string, int, []int32) {}

	wc := newWriteCache(1 << 20)
	f := newHelperFile(t, dir, "job_0.dat", 0)
	f.w.wc = wc
	open := map[fileKey]*openFile{{jobID: "job", fileIdx: 0}: f}

	if !a.handleSuccessArticle(f, WriteRequest{
		JobID: "job", FileIdx: 0, ArtIdx: 0, MessageID: "a", Offset: 0, Data: []byte("AAAA"),
	}) {
		t.Fatal("the article was not accepted, so the fixture never buffered it")
	}
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.ENOSPC }

	ack := make(chan error, 1)
	a.dispatchRequest(
		WriteRequest{JobID: "", FileIdx: -2, MessageID: "job", ackCh: ack},
		open, map[fileKey]struct{}{}, map[string]struct{}{}, wc)

	err := <-ack
	var fault *storagefault.Fault
	if !errors.As(err, &fault) {
		t.Fatalf("the ack carried %v, want the close-time fault — maybeFinalize is "+
			"about to hand this job to par2, unrar and cleanup over bytes that never "+
			"reached the platter", err)
	}
}

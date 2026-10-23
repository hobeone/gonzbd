package assembler

import (
	"errors"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestCloseJobHandles_DropsTheHandleEvenWhenTheSyncFailed pins the close arm
// in the FAILING direction: the file leaves open whether or not its
// close-time sync succeeded. A handle left in open after the arm would be
// written to again by a later article without passing the resolver, which is
// the one gate on a job that has handed its files to post-processing.
func TestCloseJobHandles_DropsTheHandleEvenWhenTheSyncFailed(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	a.opts.OnArticlesUnwritten = func(string, int, []int32) {}

	f := newHelperFile(t, dir, "job_0.dat", 0)
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	f.w.syncFile = func() error { return syscall.EIO }

	ack := make(chan error, 1)
	a.dispatchRequest(
		WriteRequest{JobID: "", FileIdx: fileIdxCloseHandles, MessageID: "job", ackCh: ack},
		open, completed)

	if _, still := open[key]; still {
		t.Error("the file was left in the open map after its handles were closed")
	}
}

// TestCloseJobHandles_ArmSendsTheCloseTimeFaultOnTheAck is the other half.
// The arm once acked with a bare close, so the fault it had just computed was
// never sent and enqueuePostProc handed the job to par2, unrar and cleanup over
// a file whose unsynced bytes never reached the platter. The only trace was a
// Warn inside syncAndClose.
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
		open, map[fileKey]struct{}{})

	err := <-ack
	if _, ok := errors.AsType[*storagefault.Fault](err); !ok {
		t.Fatalf("the ack carried %v, want the close-time fault — enqueuePostProc is "+
			"about to hand this job to par2, unrar and cleanup over bytes that never "+
			"reached the platter", err)
	}
}

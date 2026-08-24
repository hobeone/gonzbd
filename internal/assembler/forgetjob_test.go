package assembler

import "testing"

// TestForgetJob_DropsOnlyTheNamedJobsTombstones pins the control arm.
//
// The tombstone set is keyed on (jobID, fileIdx) and nothing else removes an
// entry for the life of the worker goroutine. A retry returns under the SAME
// job ID, so without this arm every file the assembler already finished for
// that job refuses the retry's writes as late duplicates — pooled, never
// written, failed again — and only a restart clears it.
//
// The other job's entry surviving is half the contract: this is scoped to one
// retry, and dropping a bystander's tombstone would re-open the finalize race
// the set exists to close.
func TestForgetJob_DropsOnlyTheNamedJobsTombstones(t *testing.T) {
	a := newHelperAssembler()
	wc := newWriteCache(0)

	retried0 := fileKey{jobID: "retried", fileIdx: 0}
	retried1 := fileKey{jobID: "retried", fileIdx: 1}
	bystander := fileKey{jobID: "other", fileIdx: 0}
	completed := map[fileKey]struct{}{
		retried0:  {},
		retried1:  {},
		bystander: {},
	}

	ack := make(chan error, 1)
	a.dispatchRequest(
		WriteRequest{JobID: "", FileIdx: fileIdxForgetJob, MessageID: "retried", ackCh: ack},
		map[fileKey]*openFile{}, completed, map[string]struct{}{}, wc)

	if _, still := completed[retried0]; still {
		t.Error("file 0 of the retried job is still tombstoned, so its re-fetched " +
			"articles will be refused as late duplicates")
	}
	if _, still := completed[retried1]; still {
		t.Error("file 1 of the retried job is still tombstoned; the arm must drop " +
			"every file of the job, not just the first it finds")
	}
	if _, ok := completed[bystander]; !ok {
		t.Error("another job's tombstone was dropped; this is scoped to one retry, " +
			"and clearing a bystander re-opens the finalize race the set closes")
	}

	select {
	case <-ack:
	default:
		t.Error("the control message was not acked, so ForgetJob would block until " +
			"its context expired")
	}
}

// TestForgetJob_DropsTheCancelledMark covers the second tombstone, which fails
// more quietly than the first.
//
// An article for a cancelled job is discarded in dispatchRequest before it
// reaches processRequest at all — no write, no failure, no log at anything
// above debug. A retry of a job that had been cancelled would have every
// article silently dropped, which reads as a download that simply never
// progresses.
func TestForgetJob_DropsTheCancelledMark(t *testing.T) {
	a := newHelperAssembler()
	wc := newWriteCache(0)
	cancelled := map[string]struct{}{"retried": {}, "other": {}}

	ack := make(chan error, 1)
	a.dispatchRequest(
		WriteRequest{JobID: "", FileIdx: fileIdxForgetJob, MessageID: "retried", ackCh: ack},
		map[fileKey]*openFile{}, map[fileKey]struct{}{}, cancelled, wc)

	if _, still := cancelled["retried"]; still {
		t.Error("the retried job is still marked cancelled, so every article of the " +
			"retry is discarded before it reaches processRequest")
	}
	if _, ok := cancelled["other"]; !ok {
		t.Error("another job's cancelled mark was dropped, which would let a " +
			"cancelled job's in-flight articles start being written again")
	}
}

// TestForgetJob_LeavesOpenHandlesAlone pins the deliberate non-action.
//
// This message forgets that files were FINISHED. It asserts nothing about a
// file being written right now, and closing a live handle here would strand
// the bytes its write cache is holding.
func TestForgetJob_LeavesOpenHandlesAlone(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	wc := newWriteCache(1 << 20)

	f := newHelperFile(t, dir, "job_0.dat", 0)
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}

	ack := make(chan error, 1)
	a.dispatchRequest(
		WriteRequest{JobID: "", FileIdx: fileIdxForgetJob, MessageID: "job", ackCh: ack},
		open, map[fileKey]struct{}{}, map[string]struct{}{}, wc)

	if _, ok := open[key]; !ok {
		t.Error("an open file was removed by ForgetJob; the arm must not touch live " +
			"handles, whose cached bytes would be stranded by a close here")
	}
}

// TestForgetJob_LetsAPreviouslyCompletedFileAcceptArticlesAgain is the
// behavioural half: the map assertions above say the entry is gone, this says
// what that buys.
//
// Before the forget, processRequest short-circuits into handleLateDuplicate on
// the tombstone and the article is never handed to the file. After it, the
// same request is admitted.
func TestForgetJob_LetsAPreviouslyCompletedFileAcceptArticlesAgain(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	var unwritten int
	a.opts.OnArticlesUnwritten = func(string, int, []int32) { unwritten++ }
	wc := newWriteCache(1 << 20)

	f := newHelperFile(t, dir, "job_0.dat", 0)
	f.w.wc = wc
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{key: {}}

	req := func() WriteRequest {
		return WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: 0, MessageID: "a",
			Offset: 0, Data: []byte("AAAA"),
		}
	}

	a.processRequest(req(), open, completed, wc)
	if got := f.w.parts(); got != 0 {
		t.Fatalf("the tombstoned file admitted %d parts; the fixture is not "+
			"exercising the late-duplicate path this test is about", got)
	}

	ack := make(chan error, 1)
	a.dispatchRequest(
		WriteRequest{JobID: "", FileIdx: fileIdxForgetJob, MessageID: "job", ackCh: ack},
		open, completed, map[string]struct{}{}, wc)

	a.processRequest(req(), open, completed, wc)
	if got := f.w.parts(); got != 1 {
		t.Errorf("the file admitted %d parts after ForgetJob, want 1: a retry still "+
			"cannot write to a file this process finished earlier", got)
	}
}

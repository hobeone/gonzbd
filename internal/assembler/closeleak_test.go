package assembler

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
)

// The cancel arm closes a job's files without routing anything: the job has
// left the queue, so returning its articles to Outstanding would re-dispatch
// work for a job that is going away. That holds under both dispositions —
// keeping a removed job's bytes does not make its articles wanted again.
//
// "Already left the queue" rests on RemoveJob being the only production caller
// of CancelJob; see closeCancelledFile.

func cancelWithoutRouting(t *testing.T, name string, disposition FileDisposition) (path string, open map[fileKey]*openFile, key fileKey) {
	t.Helper()
	a := newHelperAssembler()
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		t.Errorf("OnArticlesUnwritten(%v) on the cancel path — the job is leaving the "+
			"queue, so returning its articles to Outstanding re-dispatches work for a "+
			"job that is going away", arts)
	}
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		t.Errorf("OnArticleRejected(%d) on the cancel path", artIdx)
	}

	f := newHelperFile(t, t.TempDir(), name, 0)
	if err := f.w.Accept(articleID{msgID: "a5", artIdx: 5}, 0, []byte("AAAA")); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	key = f.w.key
	open = map[fileKey]*openFile{key: f}

	a.dispatchRequest(
		// ackCh is what marks this a control message; CancelJob always sets
		// one, and the worker discriminates on it rather than on the sentinel
		// alone so that no request built outside the package can pose as one.
		//
		// disposition must be stated because this builds the request by hand
		// instead of going through CancelJob, and its zero value is KeepFiles.
		WriteRequest{
			FileIdx:     fileIdxCancelJob,
			MessageID:   key.jobID,
			ackCh:       make(chan error, 1),
			disposition: disposition,
		},
		open, map[fileKey]struct{}{}, map[string]struct{}{})
	return f.info.Path(), open, key
}

// TestDispatchRequest_CancelDeletingFilesRoutesNothing covers the DeleteFiles
// disposition: the handle is released, the partial file is unlinked and no
// callback fires.
func TestDispatchRequest_CancelDeletingFilesRoutesNothing(t *testing.T) {
	path, open, key := cancelWithoutRouting(t, "cancelled.dat", DeleteFiles)

	if _, still := open[key]; still {
		t.Error("the cancelled job's file is still in the open map")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) = %v, want not-exist — a cancelled job's partial file "+
			"must be removed", path, err)
	}
}

// TestDispatchRequest_CancelKeepingFilesRoutesNothing is the KeepFiles half,
// and it is the reason the keep branch calls f.w.Close rather than
// a.drainAndClose: drainAndClose Syncs, which would stall ingest for every
// other job on the one worker goroutine for bytes nobody will read.
func TestDispatchRequest_CancelKeepingFilesRoutesNothing(t *testing.T) {
	path, open, key := cancelWithoutRouting(t, "kept.dat", KeepFiles)

	if _, still := open[key]; still {
		t.Error("the cancelled job's file is still in the open map under KeepFiles — " +
			"the handle must be released whatever happens to the bytes")
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: path is the fixture's own temp dir
	if err != nil {
		t.Fatalf("the kept file is gone: %v — KeepFiles suppresses the unlink, and "+
			"this is the assertion that separates it from DeleteFiles", err)
	}
	if string(got) != "AAAA" {
		t.Errorf("kept file = %q, want the article's bytes", got)
	}
}

// TestDispatchRequest_CancelKeepingFilesLogsAFailedClose pins that a close
// failure under KeepFiles is reported rather than discarded. The file survives
// the cancel, so the same error that is best-effort under DeleteFiles means a
// kept file may be missing bytes.
func TestDispatchRequest_CancelKeepingFilesLogsAFailedClose(t *testing.T) {
	var logs bytes.Buffer
	a := newHelperAssembler()
	a.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	f := newHelperFile(t, t.TempDir(), "kept_close_fault.dat", 0)
	f.w.closeFile = func() error { return syscall.EIO }
	key := f.w.key
	open := map[fileKey]*openFile{key: f}

	a.dispatchRequest(
		WriteRequest{
			FileIdx:     fileIdxCancelJob,
			MessageID:   key.jobID,
			ackCh:       make(chan error, 1),
			disposition: KeepFiles,
		},
		open, map[fileKey]struct{}{}, map[string]struct{}{})

	if !strings.Contains(logs.String(), "failed to close a cancelled job's file that is being kept") {
		t.Errorf("a failed close of a kept file was not reported; log was:\n%s", logs.String())
	}
	if _, err := os.Stat(f.info.Path()); err != nil {
		t.Errorf("the kept file is gone after a failed close: %v", err)
	}
}

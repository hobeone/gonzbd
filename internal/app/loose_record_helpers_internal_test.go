package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"strings"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/storagefault"
	"github.com/hobeone/gonzbd/internal/types"
)

// Direct pins on the loose record's unexported helpers. The end-to-end
// behaviour is in loose_record_test.go; these call each helper on its own.

// lrBuiltJob builds, but does not add, a two-file job from lrNZB.
func lrBuiltJob(t *testing.T, a *lrApp, name string, nA, nB int) *job.Job {
	t.Helper()
	parsed, err := nzb.Parse(strings.NewReader(string(lrNZB(nA, nB))))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	j, _, err := BuildIngestJob(a.config, parsed, name+".nzb", types.FetchOptions{NzbName: name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	return j
}

// TestVerifyAndAttach_InstallsWhatTheRecordProves calls the hydration path for
// a job with no progress: the recorded articles whose bytes match are Done,
// and the rest are Outstanding.
func TestVerifyAndAttach_InstallsWhatTheRecordProves(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a := env.newApp(t)
	j0 := a.addJob(t, "direct", 3, 2)
	env.writeFileA(t, j0, 3, 0, 1)
	a.recordFileA(t, j0.ID(), false, 0, 1)
	m, err := j0.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	fresh := job.New(j0.ID(), j0.Name(), job.Policy{})
	if err := a.residency.verifyAndAttach(t.Context(), fresh, m); err != nil {
		t.Fatalf("verifyAndAttach: %v", err)
	}
	if !fresh.Resident() {
		t.Fatal("the job is not resident after verifyAndAttach")
	}
	for art := range 3 {
		if got, want := articleDone(fresh, art), art < 2; got != want {
			t.Errorf("article %d Done = %v, want %v", art, got, want)
		}
	}
}

// TestVerifyAndAttach_MissingDirectoryParksTheJob is the hydration's half of
// TestRetryHistoryJob_AfterDownloadDirDeleted: a complete=0 file whose
// directory is gone may sit on a share that has not come up, so the hydration
// parks the job on a verify fault naming the directory and keeps every row.
func TestVerifyAndAttach_MissingDirectoryParksTheJob(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a := env.newApp(t)
	j0 := a.addJob(t, "nodir", 3, 2)
	env.writeFileA(t, j0, 3, 0, 1)
	a.recordFileA(t, j0.ID(), false, 0, 1)
	dir := filepath.Dir(env.filePath(j0))
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove the job directory: %v", err)
	}
	m, err := j0.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	fresh := job.New(j0.ID(), j0.Name(), job.Policy{})
	err = a.residency.verifyAndAttach(t.Context(), fresh, m)
	if !errors.Is(err, dispatch.ErrResidencyFault) {
		t.Fatalf("verifyAndAttach = %v, want a residency fault: a missing directory at a hydration is not absence", err)
	}
	if vf, ok := errors.AsType[*errVerifyFault](err); !ok || vf.File != dir {
		t.Errorf("err = %v, want an *errVerifyFault naming %s", err, dir)
	}
	if fresh.Resident() {
		t.Error("the job is resident after a verification that faulted")
	}
	rows, err := a.st.WrittenRows(t.Context(), j0.ID())
	if err != nil {
		t.Fatalf("WrittenRows: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("%d rows in SQLite after the fault, want the 2 recorded: a parked hydration deletes nothing", len(rows))
	}
}

// TestInstallVerification_RestoresThePolicyOnlyWhenAsked pins the
// restorePolicy switch and the finished list: a file a verdict set complete
// is returned, and the stored policy is installed only for hydration.
func TestInstallVerification_RestoresThePolicyOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	a := newLREnv(t).newApp(t)
	files := []durability.FileRow{
		{FileIndex: 0, Filename: "A.bin"},
		{FileIndex: 1, Filename: "B.bin", FetchPolicy: uint8(job.FetchNever)},
	}
	row := durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: lrArtLen, CRC32: 1}
	res := verifyResult{
		Verdicts: []durability.FileVerdict{{FileIdx: 0, SetComplete: true}},
		Verified: map[int][]durability.WrittenRow{0: {row}},
	}
	log := slog.New(slog.DiscardHandler)

	retry := lrBuiltJob(t, a, "retry", 1, 1)
	if got := installVerification(retry, files, []durability.WrittenRow{row}, res, false, log, nil); !slices.Equal(got, []int{0}) {
		t.Errorf("finished = %v, want [0]: the verdict set file A complete", got)
	}
	if got := retry.FileFetchPolicy(1); got != job.FetchAlways {
		t.Errorf("a retry's install moved file B's policy to %v; it must keep the derived one", got)
	}
	if !articleDone(retry, 0) {
		t.Error("the verified row's article is not Done")
	}

	hydrated := lrBuiltJob(t, a, "hydrate", 1, 1)
	installVerification(hydrated, files, []durability.WrittenRow{row}, res, true, log, nil)
	if got := hydrated.FileFetchPolicy(1); got != job.FetchNever {
		t.Errorf("hydration left file B at %v, want the stored FetchNever", got)
	}
}

// TestInstallVerification_SettlesAndFailsBeforeThePeek pins what the peek of a
// finished file sees: the install has failed the intersection's losers and
// settled the file's CRC from its verified rows, releasing them, and the file
// is not yet complete.
func TestInstallVerification_SettlesAndFailsBeforeThePeek(t *testing.T) {
	t.Parallel()
	a := newLREnv(t).newApp(t)
	j := lrBuiltJob(t, a, "settle", 2, 1)
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	lo0, hi0 := m.FileRange(0)
	lo1, hi1 := m.FileRange(1)
	if hi0-lo0 != 2 || hi1-lo1 != 1 {
		t.Fatalf("files have %d and %d articles, want 2 and 1", hi0-lo0, hi1-lo1)
	}
	//nolint:gosec // G115: test article indices
	winner, loser, single := int32(lo0), int32(lo0+1), int32(lo1)
	rows := []durability.WrittenRow{
		{FileIdx: 0, ArtIdx: winner, Offset: 0, Length: lrArtLen, CRC32: 5},
		{FileIdx: 1, ArtIdx: single, Offset: 0, Length: lrArtLen, CRC32: 7},
	}
	files := []durability.FileRow{{FileIndex: 0, Filename: "A.bin"}, {FileIndex: 1, Filename: "B.bin"}}
	res := verifyResult{
		Verdicts: []durability.FileVerdict{{FileIdx: 0, SetComplete: true}, {FileIdx: 1, SetComplete: true}},
		Verified: map[int][]durability.WrittenRow{0: rows[:1], 1: rows[1:]},
		Failed:   map[int][]int32{0: {loser}},
	}

	type atPeek struct {
		loserFailed, complete, rowsResident bool
		crc                                 uint32
	}
	seen := map[int]atPeek{}
	finished := installVerification(j, files, rows, res, true, slog.New(slog.DiscardHandler), func(fi int) {
		p := j.Progress()
		seen[fi] = atPeek{
			loserFailed:  p.ArticleFailed(int(loser)),
			complete:     p.FileComplete(fi),
			rowsResident: j.FileRows(fi) != nil,
			crc:          p.FileAssembledCRC32(fi),
		}
	})
	if !slices.Equal(finished, []int{0, 1}) {
		t.Fatalf("finished = %v, want [0 1]", finished)
	}
	if want := (atPeek{loserFailed: true}); seen[0] != want {
		t.Errorf("file 0 at its peek = %+v, want %+v: loser failed, no CRC, rows released, not complete", seen[0], want)
	}
	if want := (atPeek{loserFailed: true, crc: 7}); seen[1] != want {
		t.Errorf("file 1 at its peek = %+v, want %+v: CRC settled from its row, rows released, not complete", seen[1], want)
	}
}

// TestVerifyRetry_ReadsEveryFileAndReportsWhatItFinished calls the retry's
// verification directly: complete=1 is cleared before the read, a file whose
// every article verifies is finished by path and returned, and its complete
// flag is set again.
func TestVerifyRetry_ReadsEveryFileAndReportsWhatItFinished(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a, id := lrRetryFixture(t, env, 3, 3, true, 0, 1, 2)
	entry, err := a.repo.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("history Get: %v", err)
	}
	j, _, err := a.rebuildJobFromNZB(*entry)
	if err != nil {
		t.Fatalf("rebuildJobFromNZB: %v", err)
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	finished, err := a.verifyRetry(t.Context(), j, m)
	if err != nil {
		t.Fatalf("verifyRetry: %v", err)
	}
	if !slices.Equal(finished, []int{0}) {
		t.Errorf("finished = %v, want [0]", finished)
	}
	for art := range 3 {
		if !articleDone(j, art) {
			t.Errorf("article %d is not Done after the retry's verification", art)
		}
	}
	if !completeFlag(t, a.repo.DB(), id) {
		t.Error("file A's complete flag was not set again after it verified whole")
	}
}

// failingRecordReader fails whichever read it is armed for.
type failingRecordReader struct{ filesErr, rowsErr error }

func (f failingRecordReader) FileRows(context.Context, string) ([]durability.FileRow, error) {
	return nil, f.filesErr
}

func (f failingRecordReader) WrittenRows(context.Context, string) ([]durability.WrittenRow, error) {
	return nil, f.rowsErr
}

// TestReadRecord_ReturnsEitherReadsError pins that a failure of either read
// is returned rather than read as an empty record.
func TestReadRecord_ReturnsEitherReadsError(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk I/O error")
	for name, st := range map[string]failingRecordReader{
		"job_files":        {filesErr: boom},
		"written_articles": {rowsErr: boom},
	} {
		r := &appResidency{store: st}
		if _, _, err := r.readRecord(t.Context(), "j"); !errors.Is(err, boom) {
			t.Errorf("%s: readRecord = %v, want the read's error", name, err)
		}
	}
}

// TestResidencyFault_ParksOnlyADeviceFault pins fault's split: a cancelled
// hydration is returned as itself and parks nothing, and any other error
// parks the job and is marked ErrResidencyFault so the dispatcher does not
// settle it Failed.
func TestResidencyFault_ParksOnlyADeviceFault(t *testing.T) {
	t.Parallel()
	var parked []string
	r := &appResidency{parked: func(id string, _ *storagefault.Fault) { parked = append(parked, id) }}
	j := job.New("fault-job", "fault-job", job.Policy{})
	f := storagefault.Classify("verify", "A.bin", syscall.EIO)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.fault(cancelled, j, f, context.Canceled); errors.Is(err, dispatch.ErrResidencyFault) {
		t.Errorf("a cancelled hydration was marked a residency fault: %v", err)
	}
	if len(parked) != 0 {
		t.Errorf("a cancelled hydration parked %v", parked)
	}
	if err := r.fault(t.Context(), j, f, syscall.EIO); !errors.Is(err, dispatch.ErrResidencyFault) {
		t.Errorf("fault = %v, want ErrResidencyFault", err)
	}
	if !slices.Equal(parked, []string{"fault-job"}) {
		t.Errorf("parked %v, want the job once", parked)
	}
}

// TestRecordHandlers_IgnoreAJobTheDispatcherDoesNotHold pins the departed-job
// branch of the assembler's two record callbacks: nothing is buffered, and the
// instance lookup answers nil.
func TestRecordHandlers_IgnoreAJobTheDispatcherDoesNotHold(t *testing.T) {
	t.Parallel()
	a := newLREnv(t).newApp(t)
	if got := a.lookupCurrent("gone"); got != nil {
		t.Errorf("lookupCurrent(gone) = %v, want nil", got)
	}
	a.handleArticleWritten("gone", 0, 0, 0, lrArtLen, 1)
	a.handleFileUntrusted("gone", 0)
	a.recorder.mu.Lock()
	defer a.recorder.mu.Unlock()
	if len(a.recorder.pending) != 0 || len(a.recorder.dirty) != 0 {
		t.Errorf("a departed job left %d pending and %d dirty entries", len(a.recorder.pending), len(a.recorder.dirty))
	}
}

// TestHandleFileUntrusted_ReturnsArticlesToOutstandingWhenSQLiteFails pins the
// order's failure half: a failed SQLite untrust is logged and the in-memory
// half still runs, so the articles are fetched again in this process. An
// out-of-range file leaves every other file alone.
func TestHandleFileUntrusted_ReturnsArticlesToOutstandingWhenSQLiteFails(t *testing.T) {
	t.Parallel()
	a := newLREnv(t).newApp(t)
	j := a.addJob(t, "untrust", 2, 1)
	if err := j.MarkArticleWritten(durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Length: lrArtLen, CRC32: 1}); err != nil {
		t.Fatalf("MarkArticleWritten: %v", err)
	}
	a.recorder.st = failRecordFor{recordStore: a.recorder.st, id: j.ID()}

	a.handleFileUntrusted(j.ID(), 9)
	if !articleDone(j, 0) {
		t.Error("an untrust of a file past the manifest cleared file A's article")
	}
	a.handleFileUntrusted(j.ID(), 0)
	if articleDone(j, 0) {
		t.Error("a failed SQLite untrust left the article Done in memory")
	}
}

// TestEnqueueResumedCompletion_DeliversWhenTheChannelIsFull pins the
// fallback: a resumed completion that finds the channel full is delivered
// from a goroutine once there is room, not dropped.
func TestEnqueueResumedCompletion_DeliversWhenTheChannelIsFull(t *testing.T) {
	t.Parallel()
	a := newLREnv(t).newApp(t)
	// The app.ctx Start would set; nothing consumes the channel but this test.
	a.ctx, a.cancel = context.WithCancel(t.Context())
	t.Cleanup(a.cancel)
	for len(a.internalFileComplete) < cap(a.internalFileComplete) {
		a.internalFileComplete <- FileComplete{JobID: "filler"}
	}
	a.enqueueResumedCompletion("resumed", 7, "")
	deadline := time.After(10 * time.Second)
	for {
		select {
		case fc := <-a.internalFileComplete:
			if fc.JobID == "resumed" {
				if fc.FileIdx != 7 || !fc.Resumed {
					t.Errorf("delivered %+v, want file 7 marked Resumed", fc)
				}
				return
			}
		case <-deadline:
			t.Fatal("the resumed completion was never delivered")
		}
	}
}

// waitResumedSenders waits for every enqueueResumedCompletion fallback sender
// to finish, and reports how many are left when it gives up.
func waitResumedSenders(a *Application) int32 {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if a.resumedInFlight.Load() == 0 {
			return 0
		}
		time.Sleep(10 * time.Millisecond)
	}
	return a.resumedInFlight.Load()
}

// TestEnqueueResumedCompletion_StartDoesNotWaitForTheConsumer pins that more
// Resumed completions than the channel holds, queued inside Start before
// watchCompletions runs (as hydratePausedJobs queues them), neither block
// Start nor are lost: each is delivered once the consumer runs.
func TestEnqueueResumedCompletion_StartDoesNotWaitForTheConsumer(t *testing.T) {
	t.Parallel()
	const n = 200
	a := newLREnv(t).newApp(t, func(app *Application) {
		app.startedTransitionHook = func() {
			for i := range n {
				app.enqueueResumedCompletion("no-such-job", i, "")
			}
		}
	})
	if c := cap(a.internalFileComplete); n <= c {
		t.Fatalf("fixture: %d completions do not overfill a channel of %d", n, c)
	}
	started := make(chan error, 1)
	go func() { started <- a.Start(t.Context()) }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(10 * time.Second):
		// The blocked Start resumes when t.Context is cancelled; stop it then.
		t.Cleanup(func() {
			if err := <-started; err == nil {
				a.StopAndJoin(t)
			}
		})
		t.Fatalf("Start did not return with %d resumed completions queued before the consumer runs", n)
	}
	t.Cleanup(func() { a.StopAndJoin(t) })
	if left := waitResumedSenders(a.Application); left != 0 {
		t.Errorf("%d resumed senders never delivered once the consumer ran", left)
	}
}

// TestEnqueueResumedCompletion_SenderExitsAtShutdown pins the other way out:
// a dispatcher tick can hydrate after Shutdown has stopped the consumer (the
// dispatcher stops last), and a sender that finds the channel full then gives
// up on app.ctx rather than outliving the app.
func TestEnqueueResumedCompletion_SenderExitsAtShutdown(t *testing.T) {
	t.Parallel()
	a := newLREnv(t).newApp(t)
	if err := a.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := a.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for len(a.internalFileComplete) < cap(a.internalFileComplete) {
		a.internalFileComplete <- FileComplete{JobID: "filler"}
	}
	for i := range 20 {
		a.enqueueResumedCompletion("late", i, "")
	}
	if left := waitResumedSenders(a.Application); left != 0 {
		t.Errorf("%d resumed senders outlived Shutdown: a sender that cannot deliver must give up on app.ctx", left)
	}
}

// TestMarkFileDirty_IgnoresAFileItCannotRead pins the guard: a job without
// progress, or an index past it, buffers nothing.
func TestMarkFileDirty_IgnoresAFileItCannotRead(t *testing.T) {
	t.Parallel()
	a := newLREnv(t).newApp(t)
	a.markFileDirty(job.New("bare", "bare", job.Policy{}), 0)
	j := a.addJob(t, "dirty", 1, 1)
	a.markFileDirty(j, 5)
	a.recorder.mu.Lock()
	defer a.recorder.mu.Unlock()
	if len(a.recorder.dirty) != 0 {
		t.Errorf("markFileDirty buffered %d jobs' states for files it cannot read", len(a.recorder.dirty))
	}
}

// TestHydratePausedJobs_StopsOnACancelledContext pins the abort: a cancelled
// start ends the pass with the context's error rather than loading jobs.
func TestHydratePausedJobs_StopsOnACancelledContext(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a := env.newApp(t)
	a.addJob(t, "paused", 2, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.hydratePausedJobs(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("hydratePausedJobs = %v, want context.Canceled", err)
	}
	if err := (&Application{}).hydratePausedJobs(t.Context()); err != nil {
		t.Errorf("hydratePausedJobs with no dispatcher = %v, want nil", err)
	}
}

// TestHandleFileUntrusted_KeepsTheFilesNameForTheNextFlush pins that an untrust
// leaves the file's buffered state correct: the DeleteAll purge drops the dirty
// FileState, and the untrust re-marks it, so a flush after a later article
// still writes the filename to job_files. Without that, a crash before the file
// completes leaves rows under an empty filename and the next start deletes
// them all.
func TestHandleFileUntrusted_KeepsTheFilesNameForTheNextFlush(t *testing.T) {
	t.Parallel()
	a := newLREnv(t).newApp(t)
	j := a.addJob(t, "untrust-name", 2, 1)
	if err := j.SetFileFilename(0, "A.bin"); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	a.markFileDirty(j, 0)

	a.handleFileUntrusted(j.ID(), 0)
	if err := a.recorder.flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	var name string
	if err := a.repo.DB().QueryRowContext(t.Context(),
		`SELECT COALESCE(filename, '') FROM job_files WHERE job_id = ? AND file_index = 0`, j.ID()).Scan(&name); err != nil {
		t.Fatalf("query job_files: %v", err)
	}
	if name != "A.bin" {
		t.Errorf("job_files.filename = %q after an untrust and a flush, want %q", name, "A.bin")
	}
}

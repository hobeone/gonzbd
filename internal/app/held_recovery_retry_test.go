package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/notifier"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// heldRecoveryFiles is the shape of the test's download: an archive and one
// recovery volume, which on-demand par2 holds back.
var heldRecoveryFiles = []string{"release.rar", "release.vol000+01.par2"}

// heldRecoveryNZB renders heldRecoveryFiles as an NZB of one article each, the
// shape a retry rebuilds the job from.
func heldRecoveryNZB() []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="iso-8859-1" ?>` + "\n")
	b.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	for i, name := range heldRecoveryFiles {
		fmt.Fprintf(&b, `<file poster="p@t" date="1700000000" subject="&quot;%s&quot; yEnc (1/1)">`+"\n", name)
		b.WriteString("<groups><group>alt.bin.test</group></groups>\n<segments>\n")
		fmt.Fprintf(&b, `<segment bytes="1024" number="1">h%d@t</segment>`+"\n", i)
		b.WriteString("</segments>\n</file>\n")
	}
	b.WriteString("</nzb>\n")
	return []byte(b.String())
}

// recordingNotifier records the type of every event it is sent.
type recordingNotifier struct {
	mu   sync.Mutex
	sent []notifier.EventType
}

func (*recordingNotifier) Name() string                    { return "recording" }
func (*recordingNotifier) Accepts(notifier.EventType) bool { return true }
func (n *recordingNotifier) Send(_ context.Context, e notifier.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, e.Type)
	return nil
}

func (n *recordingNotifier) count(t notifier.EventType) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, s := range n.sent {
		if s == t {
			c++
		}
	}
	return c
}

// syncBuffer is a bytes.Buffer safe for a log handler and a reader.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// heldRecoveryApp is an application with on-demand par2 on, a recording
// notifier, a captured log, and a registered job whose recovery volume is
// held, with the NZB backup a retry rebuilds it from.
type heldRecoveryApp struct {
	app    *Application
	repo   *history.Repository
	notes  *recordingNotifier
	logged *syncBuffer
	job    *job.Job
	backup string
}

func newHeldRecoveryApp(t *testing.T, id string) heldRecoveryApp {
	t.Helper()
	application, repo, adminDir := newLifecycleTestApp(t)
	if !application.config.Snapshot().Downloads.OnDemandPar2 {
		t.Fatal("fixture guard: on-demand par2 is off by default, so nothing is held")
	}
	logged := &syncBuffer{}
	application.log = slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	notes := &recordingNotifier{}
	d := notifier.NewDispatcher(slog.New(slog.DiscardHandler))
	d.Register(notes)
	application.SetNotifier(d)

	raw := heldRecoveryNZB()
	backup := id + ".nzb.gz"
	writeRetryNZBBackup(t, adminDir, backup, raw)
	parsed, err := nzb.Parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	j, hdr, err := BuildIngestJob(application.config, parsed, id+".nzb",
		types.FetchOptions{JobID: id, NzbName: "held-" + id}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	hdr.NZBBackup = backup
	if err := application.dispatcher.Add(t.Context(), j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return heldRecoveryApp{app: application, repo: repo, notes: notes, logged: logged, job: j, backup: backup}
}

// failedPar2Run is a post-processing run of j that failed par2, as
// extracted_repair leaves a damaged Layout B extraction it had no recovery
// blocks for.
func (h heldRecoveryApp) failedPar2Run(j *job.Job) *postproc.Job {
	return &postproc.Job{
		Job:       j,
		Filename:  j.ID() + ".nzb",
		NZBBackup: h.backup,
		ParError:  true,
	}
}

// #651: a Layout B job's recovery volumes are held back through download, so
// a damaged extraction fails extracted_repair with nothing to repair from. The
// finalizer must retry the job with those volumes released, rather than file
// it as failed for good.
func TestFinalize_ParErrorWithHeldVolumesRetriesWithThemReleased(t *testing.T) {
	t.Parallel()
	const id = "feedface0651a001"
	h := newHeldRecoveryApp(t, id)
	if !h.job.HasDeferredPar2() {
		t.Fatal("fixture guard: the job holds no recovery volume")
	}

	h.app.finalizer.finalize(h.failedPar2Run(h.job))

	retried, held := h.app.dispatcher.Job(id)
	if !held || retried == h.job {
		t.Fatalf("after a par2 failure with held recovery volumes the job was not retried "+
			"(registered=%v, same instance=%v); log:\n%s", held, retried == h.job, h.logged.String())
	}
	if retried.HasDeferredPar2() {
		t.Errorf("the retry still holds recovery volumes %v; it must fetch them", retried.DeferredRecoveryIndices())
	}
	// Persisted too, or an eviction and re-hydration would hold them again.
	var policy int
	if err := h.app.historyRepo.DB().QueryRowContext(t.Context(),
		`SELECT fetch_policy FROM job_files WHERE job_id = ? AND file_index = 1`, id).Scan(&policy); err != nil {
		t.Fatalf("read the retry's job_files row: %v", err)
	}
	if job.FetchPolicy(policy) != job.FetchAlways {
		t.Errorf("the retry's recovery volume is persisted as %v, want %v", job.FetchPolicy(policy), job.FetchAlways)
	}
	if retried.Par2ReleaseReason() == "" {
		t.Error("the retry records no reason its recovery volumes were released")
	}
	if _, err := h.repo.Get(t.Context(), id); err == nil {
		t.Error("the failed history entry outlived the retry that replaced it")
	}
	if n := h.notes.count(notifier.PostProcessingFailed); n != 0 {
		t.Errorf("%d failure notification(s) sent for a job that is being retried, want 0", n)
	}
}

// The retry has no held volume left, so a par2 failure of the retry is final:
// it is filed Failed and notified, and not retried again.
func TestFinalize_RetriedJobIsNotRetriedAgain(t *testing.T) {
	t.Parallel()
	const id = "feedface0651a002"
	h := newHeldRecoveryApp(t, id)
	h.app.finalizer.finalize(h.failedPar2Run(h.job))
	retried, held := h.app.dispatcher.Job(id)
	if !held || retried == h.job {
		t.Fatalf("fixture guard: the first failure was not retried; log:\n%s", h.logged.String())
	}

	h.app.finalizer.finalize(h.failedPar2Run(retried))

	if again, held := h.app.dispatcher.Job(id); held {
		t.Fatalf("the retried job was retried again (same instance=%v): a par2 failure "+
			"with its volumes already fetched must be final", again == retried)
	}
	entry, err := h.repo.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("the retry's failure was not filed: %v", err)
	}
	if entry.Status != "Failed" {
		t.Errorf("history status = %q, want Failed", entry.Status)
	}
	if n := h.notes.count(notifier.PostProcessingFailed); n != 1 {
		t.Errorf("%d failure notification(s), want 1 for the final failure", n)
	}
}

// A retry that cannot start leaves the failure as it would have been without
// one: the Failed entry, its notification, and a log line saying why.
func TestFinalize_RetryThatCannotStartLeavesTheFailureVisible(t *testing.T) {
	t.Parallel()
	const id = "feedface0651a003"
	h := newHeldRecoveryApp(t, id)
	run := h.failedPar2Run(h.job)
	run.NZBBackup = "" // nothing to rebuild the retry from

	h.app.finalizer.finalize(run)

	if _, held := h.app.dispatcher.Job(id); held {
		t.Fatal("a job with no NZB backup was retried")
	}
	entry, err := h.repo.Get(t.Context(), id)
	if err != nil || entry.Status != "Failed" {
		t.Fatalf("history entry = %+v, %v; want it kept as Failed", entry, err)
	}
	if n := h.notes.count(notifier.PostProcessingFailed); n != 1 {
		t.Errorf("%d failure notification(s), want 1", n)
	}
	logged := h.logged.String()
	if !strings.Contains(logged, "could not retry the job with its held recovery volumes") ||
		!strings.Contains(logged, "no NZB backup") {
		t.Errorf("the failed retry was not logged with its reason:\n%s", logged)
	}
}

// The entry filed for a job the finalizer will retry says the recovery volumes
// were held back and that a retry fetches them. When the automatic retry
// cannot start (here, no NZB backup; at shutdown, a cancelled context and a
// stopped assembler), that note is what stands beside the Failed status.
func TestFinalize_HeldVolumesEntryCarriesTheRetryNote(t *testing.T) {
	t.Parallel()
	const id = "feedface0651a00c"
	h := newHeldRecoveryApp(t, id)
	run := h.failedPar2Run(h.job)
	run.NZBBackup = ""

	h.app.finalizer.finalize(run)

	entry, err := h.repo.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("history entry: %v", err)
	}
	if !strings.Contains(entry.StageLog, heldVolumesRetryNote) {
		t.Errorf("the Failed entry does not carry the held-volumes note %q; stage log:\n%s",
			heldVolumesRetryNote, entry.StageLog)
	}
	if !strings.Contains(h.logged.String(), "retry it to fetch its recovery volumes") {
		t.Errorf("the failed start's warning does not say the job can be retried to fetch its volumes:\n%s",
			h.logged.String())
	}
}

// A prepare that fails aborts the retry before it registers anything, and the
// Failed entry stays for a later retry.
func TestRetryHistoryJob_PrepareErrorAbortsTheRetry(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	const id = "feedface0651a006"
	addRetryableEntry(t, repo, adminDir, id, "")
	boom := errors.New("prepare refused")

	err := application.retryHistoryJob(t.Context(), id, func(*job.Job) error { return boom })

	if !errors.Is(err, boom) {
		t.Errorf("retryHistoryJob = %v, want the prepare's error", err)
	}
	if _, held := application.dispatcher.Job(id); held {
		t.Error("a retry whose prepare failed registered its job")
	}
	if _, err := repo.Get(t.Context(), id); err != nil {
		t.Errorf("a retry whose prepare failed deleted the Failed entry: %v", err)
	}
}

// A retry that releases its held volumes, which marks the job in the
// checkpointer, and then aborts leaves nothing marked, so no later flush
// writes the aborted attempt's rows over a later retry's. Two aborts: the
// queue manifest cannot be written, and a step after the release fails.
func TestRetryHistoryJob_AbortedAfterTheReleaseLeavesNothingMarked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		blockManifest    bool
		failAfterRelease bool
	}{
		{"manifest unwritable", true, false},
		{"a step after the release fails", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			application, repo, adminDir := newLifecycleTestApp(t)
			if application.checkpointer == nil {
				t.Fatal("fixture guard: no checkpointer, so nothing could be marked")
			}
			const id = "feedface0651a00b"
			backup := id + ".nzb.gz"
			writeRetryNZBBackup(t, adminDir, backup, heldRecoveryNZB())
			if err := repo.Add(t.Context(), history.Entry{
				NzoID: id, Name: "held-" + id, NzbName: id + ".nzb",
				NZBBackup: backup, Category: "*", Status: "Failed", Completed: time.Now(),
			}, nil); err != nil {
				t.Fatalf("repo.Add: %v", err)
			}
			if tc.blockManifest {
				mdir := manifestDir(adminDir)
				if err := os.MkdirAll(filepath.Dir(mdir), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(mdir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			released := 0
			err := application.retryHistoryJob(t.Context(), id, func(j *job.Job) error {
				n, err := application.releaseRecoveryVolumes(j, "test")
				released = n
				if err == nil && tc.failAfterRelease {
					err = errors.New("a later step failed")
				}
				return err
			})

			if err == nil {
				t.Fatal("fixture guard: the retry succeeded")
			}
			if tc.failAfterRelease && released == 0 {
				t.Fatal("fixture guard: nothing was released, so nothing was marked")
			}
			if n := application.checkpointer.DirtyCount(); n != 0 {
				t.Errorf("DirtyCount = %d after a retry that aborted (released %d volume(s)), want 0: "+
					"a later flush would write the aborted job's rows", n, released)
			}
		})
	}
}

// A par2 failure with no held volume, and a held volume with no par2 failure,
// are each filed as they always were.
func TestFinalize_RetriesOnlyAParErrorWithHeldVolumes(t *testing.T) {
	t.Parallel()
	t.Run("no par2 failure", func(t *testing.T) {
		t.Parallel()
		const id = "feedface0651a004"
		h := newHeldRecoveryApp(t, id)
		run := h.failedPar2Run(h.job)
		run.ParError = false
		run.UnpackError = true
		h.app.finalizer.finalize(run)
		if _, held := h.app.dispatcher.Job(id); held {
			t.Error("an unpack failure was retried for the held recovery volumes")
		}
	})
	t.Run("no held volume", func(t *testing.T) {
		t.Parallel()
		const id = "feedface0651a005"
		h := newHeldRecoveryApp(t, id)
		if err := h.job.UndeferRecoveryVolumes(h.job.DeferredRecoveryIndices()); err != nil {
			t.Fatalf("UndeferRecoveryVolumes: %v", err)
		}
		h.app.finalizer.finalize(h.failedPar2Run(h.job))
		if _, held := h.app.dispatcher.Job(id); held {
			t.Error("a par2 failure whose volumes were all fetched was retried")
		}
	})
}

// heldVolumesMightRepair needs a job, a ParError and a held volume.
func TestHeldVolumesMightRepair(t *testing.T) {
	t.Parallel()
	h := newHeldRecoveryApp(t, "feedface0651a007")
	cases := []struct {
		name string
		run  *postproc.Job
		want bool
	}{
		{"no job", &postproc.Job{ParError: true}, false},
		{"no par2 failure", &postproc.Job{Job: h.job}, false},
		{"par2 failure with a held volume", &postproc.Job{Job: h.job, ParError: true}, true},
	}
	for _, tc := range cases {
		if got := heldVolumesMightRepair(tc.run); got != tc.want {
			t.Errorf("%s: heldVolumesMightRepair = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// releaseRecoveryVolumes releases every held volume, records the reason, and
// reports how many it released; a job with no manifest resident refuses.
func TestReleaseRecoveryVolumes(t *testing.T) {
	t.Parallel()
	h := newHeldRecoveryApp(t, "feedface0651a008")
	want := len(h.job.DeferredRecoveryIndices())
	n, err := h.app.releaseRecoveryVolumes(h.job, "because")
	if err != nil || n != want || want == 0 {
		t.Fatalf("releaseRecoveryVolumes = %d, %v; want %d (> 0), nil", n, err, want)
	}
	if h.job.HasDeferredPar2() {
		t.Error("a volume is still held after the release")
	}
	if got := h.job.Par2ReleaseReason(); got != "because" {
		t.Errorf("Par2ReleaseReason = %q, want %q", got, "because")
	}

	evicted := newHeldRecoveryApp(t, "feedface0651a009")
	evicted.job.Evict()
	if _, err := evicted.app.releaseRecoveryVolumes(evicted.job, "because"); !errors.Is(err, job.ErrNotResident) {
		t.Errorf("releaseRecoveryVolumes on an evicted job = %v, want ErrNotResident", err)
	}
}

// retryWithHeldVolumes reports false for a job with no Failed entry to retry.
func TestRetryWithHeldVolumes_NoEntry(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	if application.finalizer.retryWithHeldVolumes("feedface0651a00a") {
		t.Error("retryWithHeldVolumes = true for a job with no history entry")
	}
}

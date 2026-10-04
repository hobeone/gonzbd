package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// newNameRaceApp is a lifecycle app with a real dispatcher over the
// application's own database, so queue rows, job_files rows and manifests are
// all where a refused registration could leave them.
func newNameRaceApp(t *testing.T) (*Application, *history.Repository) {
	t.Helper()
	application, repo, _ := newLifecycleTestApp(t)
	application.dispatcher = dispatch.New(
		1, 1, time.Second, time.Now,
		&appWorkers{app: application},
		application.residency,
		store.New(repo.DB()),
		application.runner,
	)
	return application, repo
}

// buildNamedIngestJob builds a one-file job whose name is name, from an NZB
// file called file. Distinct files keep the jobs' NZB backups apart.
func buildNamedIngestJob(t *testing.T, application *Application, name, file string) (*job.Job, dispatch.Header, []byte) {
	t.Helper()
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  file + ".bin",
		Bytes:    100,
		Articles: []nzb.Article{{ID: file + "-0@t", Bytes: 100, Number: 1}},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, file+".nzb", types.FetchOptions{NzbName: name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if hdr.Name != name {
		t.Fatalf("setup: BuildIngestJob named the job %q, want %q", hdr.Name, name)
	}
	return j, hdr, []byte("<nzb></nzb>")
}

func queueNames(application *Application) map[string]string {
	names := map[string]string{}
	for _, row := range application.dispatcher.List() {
		names[row.ID] = row.Header.Name
	}
	return names
}

// TestAddJob_ConcurrentCallsUnderOneNameGetDistinctNames pins that a name is
// one job's download directory even when several ingests choose it at once.
// The hook holds every AddJob after its first choice until all of them have
// chosen, so all of them choose the same free name before any registers; the
// registry admits one and the rest choose again.
func TestAddJob_ConcurrentCallsUnderOneNameGetDistinctNames(t *testing.T) {
	t.Parallel()
	application, repo := newNameRaceApp(t)

	const n = 8
	jobs := make([]*job.Job, n)
	hdrs := make([]dispatch.Header, n)
	raws := make([][]byte, n)
	for i := range n {
		jobs[i], hdrs[i], raws[i] = buildNamedIngestJob(t, application, "same", fmt.Sprintf("same-%d", i))
	}
	var chosen atomic.Int32
	allChosen := make(chan struct{})
	application.jobNameChosenHook = func(name string) {
		switch c := chosen.Add(1); {
		case c < n:
			<-allChosen
		case c == n:
			close(allChosen)
		}
		// A later choice is a retry after a refusal, and passes straight through.
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			errs[i] = application.AddJob(t.Context(), jobs[i], hdrs[i], raws[i], false)
		})
	}
	wg.Wait()
	// Checked last, so a broken retry reports through the assertions below.
	defer func() {
		if c := chosen.Load(); c <= n {
			t.Errorf("%d name choices for %d jobs: no AddJob chose a second time, so the race this test exists for never happened", c, n)
		}
	}()
	for i, err := range errs {
		if err != nil {
			t.Errorf("AddJob %d: %v", i, err)
		}
	}

	names := queueNames(application)
	seen := map[string]string{}
	for id, name := range names {
		if other, dup := seen[name]; dup {
			t.Errorf("jobs %s and %s both named %q: they share one download directory", other, id, name)
		}
		seen[name] = id
	}
	if len(names) != n {
		t.Fatalf("%d jobs queued, want %d: %v", len(names), n, names)
	}
	for _, j := range jobs {
		if j.Name() != names[j.ID()] {
			t.Errorf("job %s: Job.Name() = %q, but its queue row says %q", j.ID(), j.Name(), names[j.ID()])
		}
	}

	// Nothing a refused registration wrote is left without a queued owner.
	adminDir := application.config.GetGeneral().AdminDir
	manifests, err := os.ReadDir(manifestDir(adminDir))
	if err != nil {
		t.Fatalf("ReadDir manifests: %v", err)
	}
	if len(manifests) != n {
		t.Errorf("%d manifests on disk, want %d", len(manifests), n)
	}
	rows, err := repo.DB().QueryContext(t.Context(), "SELECT DISTINCT job_id FROM job_files")
	if err != nil {
		t.Fatalf("query job_files: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, ok := names[id]; !ok {
			t.Errorf("job_files rows for %s, which is not queued", id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	backups, err := os.ReadDir(filepath.Join(adminDir, "nzb"))
	if err != nil {
		t.Fatalf("ReadDir nzb: %v", err)
	}
	queued := application.dispatcher.List()
	owned := make([]string, 0, len(queued))
	for _, row := range queued {
		owned = append(owned, row.Header.NZBBackup)
	}
	for _, b := range backups {
		if !slices.Contains(owned, b.Name()) {
			t.Errorf("NZB backup %s has no queued owner", b.Name())
		}
	}
}

// TestAddJob_ANameTakenAfterItWasChosenIsChosenAgain pins AddJob's half of
// the retry deterministically: a rename takes the name between AddJob
// choosing it and registering, and AddJob takes the next one instead of
// failing.
func TestAddJob_ANameTakenAfterItWasChosenIsChosenAgain(t *testing.T) {
	t.Parallel()
	application, _ := newNameRaceApp(t)
	a, ha, ra := buildNamedIngestJob(t, application, "first", "first")
	if err := application.AddJob(t.Context(), a, ha, ra, false); err != nil {
		t.Fatalf("AddJob(first): %v", err)
	}

	// A flag, not a sync.Once: the rename re-enters this hook, and a nested Do deadlocks.
	var fired atomic.Bool
	var renameErr error
	application.jobNameChosenHook = func(name string) {
		if name != "target" || !fired.CompareAndSwap(false, true) {
			return
		}
		_, renameErr = application.RenameJob(a.ID(), "target")
	}
	b, hb, rb := buildNamedIngestJob(t, application, "target", "second")
	if err := application.AddJob(t.Context(), b, hb, rb, false); err != nil {
		t.Fatalf("AddJob(target) after a rename took the name it chose: %v", err)
	}
	if renameErr != nil {
		t.Fatalf("setup: the racing rename failed: %v", renameErr)
	}
	names := queueNames(application)
	if names[a.ID()] != "target" || names[b.ID()] != "target.1" {
		t.Fatalf("names = %v, want the renamed job at target and the added one at target.1", names)
	}
}

// TestRenameJob_ANameTakenAfterItWasChosenIsChosenAgain is the same race the
// other way round: an AddJob registers the name a rename chose.
func TestRenameJob_ANameTakenAfterItWasChosenIsChosenAgain(t *testing.T) {
	t.Parallel()
	application, _ := newNameRaceApp(t)
	a, ha, ra := buildNamedIngestJob(t, application, "first", "first")
	if err := application.AddJob(t.Context(), a, ha, ra, false); err != nil {
		t.Fatalf("AddJob(first): %v", err)
	}
	b, hb, rb := buildNamedIngestJob(t, application, "target", "second")

	var fired atomic.Bool
	var addErr error
	application.jobNameChosenHook = func(name string) {
		if name != "target" || !fired.CompareAndSwap(false, true) {
			return
		}
		addErr = application.AddJob(t.Context(), b, hb, rb, false)
	}
	got, err := application.RenameJob(a.ID(), "target")
	if addErr != nil {
		t.Fatalf("setup: the racing AddJob failed: %v", addErr)
	}
	if err != nil {
		t.Fatalf("RenameJob after an AddJob took the name it chose: %v", err)
	}
	names := queueNames(application)
	if got != "target.1" || names[a.ID()] != "target.1" || names[b.ID()] != "target" {
		t.Fatalf("rename returned %q, names = %v; want the added job at target and the renamed one at target.1", got, names)
	}
}

// TestRetryHistoryJob_RefusesWhenAnotherJobTookItsName pins that a retry is
// not given another name: its name is the directory its retained bytes are
// in. When a job registers that name first, the retry is refused and leaves
// the history entry, and no queue row or manifest, behind.
func TestRetryHistoryJob_RefusesWhenAnotherJobTookItsName(t *testing.T) {
	t.Parallel()
	application, repo := newNameRaceApp(t)
	adminDir := application.config.GetGeneral().AdminDir
	nzbBackupDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbBackupDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const jobID = "feedfacecafe0001"
	const nzbBackup = "name-retry.nzb.gz"
	rawNZB := []byte(`<?xml version="1.0" encoding="utf-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="1700000000" subject="name-retry.bin yEnc (1/1)">
    <groups><group>alt.binaries.test</group></groups>
    <segments><segment bytes="100" number="1">name-retry-0@t</segment></segments>
  </file>
</nzb>`)
	if err := fsutil.WriteGzAtomicBytes(filepath.Join(nzbBackupDir, nzbBackup), rawNZB); err != nil {
		t.Fatalf("WriteGzAtomicBytes: %v", err)
	}
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     jobID,
		Name:      "name-retry",
		NzbName:   "name-retry.nzb",
		NZBBackup: nzbBackup,
		Category:  "*",
		Status:    "Failed",
		Completed: time.Now(),
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	other, ho, ro := buildNamedIngestJob(t, application, "name-retry", "other")
	application.retryRegisteringHook = func(string) {
		if err := application.AddJob(t.Context(), other, ho, ro, false); err != nil {
			t.Errorf("setup: AddJob(name-retry): %v", err)
		}
	}
	err := application.RetryHistoryJob(t.Context(), jobID)
	if !errors.Is(err, dispatch.ErrJobNameTaken) || !errors.Is(err, errRetryDirConflict) {
		t.Fatalf("RetryHistoryJob after another job took its name = %v, want errRetryDirConflict wrapping ErrJobNameTaken", err)
	}
	if names := queueNames(application); len(names) != 1 || names[other.ID()] != "name-retry" {
		t.Fatalf("queue = %v, want only the other job, at name-retry", names)
	}
	if _, err := repo.Get(t.Context(), jobID); err != nil {
		t.Errorf("the refused retry lost its history entry: %v", err)
	}
	mpath, err := manifestPath(adminDir, jobID)
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	if _, err := os.Stat(mpath); !os.IsNotExist(err) {
		t.Errorf("the refused retry left its manifest %s (stat err = %v)", mpath, err)
	}
}

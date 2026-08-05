package queue

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// discardFixture adds the #287 on-demand par2 job (one content file, one par2
// index, one deferred recovery volume), discards the deferred volume, and
// returns the queue, the live job, and the post-discard file count.
//
// Every test below turns on the same premise: the discard shrinks the job in
// memory, so anything still describing the pre-discard shape is stale.
func discardFixture(t *testing.T, name string) (*Queue, *Job, int) {
	t.Helper()
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := addOnDemandPar2Job(t, q, name)

	before, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	beforeFiles := before.NumFiles()
	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	after, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest after discard: %v", err)
	}
	if after.NumFiles() >= beforeFiles {
		t.Fatalf("fixture guard: discard did not shrink the manifest (%d -> %d), so nothing below is about staleness",
			beforeFiles, after.NumFiles())
	}
	return q, job, after.NumFiles()
}

// TestDiscardDeferredPar2_RewritesTheStoredManifest pins the first half of
// #294: the rebuilt manifest must reach manifests/<id>.json.gz.
//
// Until it did, evicting a discarded job left the pre-discard manifest as the
// only copy on disk. #293's guard then correctly refused to pair it with the
// job's shrunk progress, so the job became permanently un-hydratable — its
// per-file detail unavailable for the rest of its life in the queue. The
// guard was working; there was simply nothing left for it to serve.
func TestDiscardDeferredPar2_RewritesTheStoredManifest(t *testing.T) {
	q, job, wantFiles := discardFixture(t, "discard-rewrites")

	// Evicting drops the in-memory manifest, so the next read has to come
	// from disk. That is the whole point: it is the only way to observe what
	// was actually written.
	if err := q.SetStatus(job.ID, constants.StatusPaused); err != nil {
		t.Fatalf("SetStatus(Paused): %v", err)
	}
	if manifestResident(job) {
		t.Fatal("fixture guard: expected eviction to drop the manifest")
	}

	snap := q.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	m, mErr := snap.Manifest()
	if mErr != nil {
		t.Fatalf("Manifest() = %v, want the rebuilt manifest: the discard was never written to disk", mErr)
	}
	if m.NumFiles() != wantFiles {
		t.Errorf("stored manifest has %d files, want %d: the file on disk still describes the pre-discard job",
			m.NumFiles(), wantFiles)
	}
	for i := range m.NumFiles() {
		if snap.Progress().FileDeferred(i) {
			t.Errorf("file %d is still deferred after the discard", i)
		}
	}
}

// TestDiscardDeferredPar2_HydratesBackIntoAResidentStatus is the same
// property reached through hydrateJobLocked rather than a snapshot clone.
// Promotion must be able to bring a discarded job back, not fail closed on
// its own stale manifest.
func TestDiscardDeferredPar2_HydratesBackIntoAResidentStatus(t *testing.T) {
	q, job, wantFiles := discardFixture(t, "discard-rehydrates")

	if err := q.SetStatus(job.ID, constants.StatusPaused); err != nil {
		t.Fatalf("SetStatus(Paused): %v", err)
	}
	if manifestResident(job) {
		t.Fatal("fixture guard: expected eviction")
	}

	if err := q.SetStatus(job.ID, constants.StatusDownloading); err != nil {
		t.Fatalf("SetStatus(Downloading) = %v, want the job to hydrate from its rewritten manifest", err)
	}
	if !manifestResident(job) {
		t.Fatal("the job was not made resident")
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.NumFiles() != wantFiles {
		t.Errorf("re-hydrated manifest has %d files, want %d", m.NumFiles(), wantFiles)
	}
}

// TestDiscardDeferredPar2_ReportsAPersistenceFailureAndKeepsTheRebuild pins
// what a failed ReplaceManifest does.
//
// Two halves, and they pull in opposite directions. The error must reach the
// caller: silently returning nil is what made #287's sibling bug invisible,
// and app.maybeReleaseRecoveryVolumes logs on this. But the in-memory rebuild
// must stay, because it is correct — the caller has already verified the data
// is clean, so re-deferring volumes here would hold back files nothing needs
// and stall a job that is ready to finalize. The durable state reverts on a
// restart, which is the pre-#294 behaviour and no worse than it.
func TestDiscardDeferredPar2_ReportsAPersistenceFailureAndKeepsTheRebuild(t *testing.T) {
	real, dir := setupResidencyTestStore(t)
	fs := &failingStore{Store: real}
	q := New(WithStore(fs), WithStateDir(dir))
	job := addOnDemandPar2Job(t, q, "discard-persist-fail")

	before, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	beforeFiles := before.NumFiles()

	fs.failReplaceMani = true
	err = q.DiscardDeferredPar2(job.ID)
	if err == nil {
		t.Fatal("DiscardDeferredPar2 returned nil after the store write failed; the caller cannot tell the discard was not persisted")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("DiscardDeferredPar2 = %v, want it to wrap the store's error", err)
	}

	after, mErr := job.Manifest()
	if mErr != nil {
		t.Fatalf("Manifest after the failed persist: %v", mErr)
	}
	if after.NumFiles() >= beforeFiles {
		t.Errorf("the in-memory rebuild was rolled back (%d -> %d files): the volumes the caller verified as unnecessary are held again",
			beforeFiles, after.NumFiles())
	}
	if n := deferredCount(job); n != 0 {
		t.Errorf("the live job still defers %d files after a discard it was told to perform", n)
	}
}

// TestDiscardDeferredPar2_ReplacesJobFilesRows pins the second half of the
// same fix, and the consequence #294 does not name.
//
// job_files.deferred is persisted from the job's real state rather than a
// literal zero — that was #287's fix. DiscardDeferredPar2 left those rows
// alone, so the rows and the on-disk manifest agreed with each other at the
// *pre-discard* shape. Nothing detects that: the staleness guard compares the
// manifest against in-memory progress, and both artifacts on disk were wrong
// together. The discard was therefore silently reverted by a restart, and the
// recovery volumes the job was told to drop came back as pending.
//
// A targeted DELETE of the discarded rows would not be enough: dropping a
// file renumbers every file_index after it, so the rows have to be rewritten
// wholesale.
func TestDiscardDeferredPar2_ReplacesJobFilesRows(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))

	// addOnDemandPar2Job's fixture cannot show the renumbering: par2NZB's
	// deferred volume always sorts last, so discarding it shifts nothing and
	// a per-index assertion would hold whether or not the rows moved. Place
	// the deferred file in the middle instead, and download the file after it
	// — the surviving file's stored state has to travel from row 2 to row 1
	// for the reload below to find it.
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "content-1.bin", Bytes: 1000, Articles: []nzb.Article{{ID: "c1@x", Bytes: 1000, Number: 1}}},
			{Subject: `"content.vol000+01.par2" yEnc`, Bytes: 500, Articles: []nzb.Article{{ID: "v@x", Bytes: 500, Number: 1}}},
			{Subject: "content-2.bin", Bytes: 1000, Articles: []nzb.Article{{ID: "c2@x", Bytes: 1000, Number: 1}}},
		},
	}
	job, err := NewJob(parsed, AddOptions{Filename: "discard-rows.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if pre := mustManifest(t, job); !pre.FileIsPar2Recovery(1) {
		t.Fatalf("fixture guard: expected the deferred recovery volume at index 1, so discarding it shifts content-2 down")
	}
	if err := q.MarkArticlesDone(job.ID, []string{"c2@x"}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.FileSubject(1) != "content-2.bin" {
		t.Fatalf("fixture guard: content-2 did not shift to index 1 (got %q)", m.FileSubject(1))
	}

	counts, err := store.ArticleCountsByJob(t.Context())
	if err != nil {
		t.Fatalf("ArticleCountsByJob: %v", err)
	}
	got := counts[job.ID]
	if len(got) != m.NumFiles() {
		t.Fatalf("job_files holds %d rows, want %d: the discarded file's row was left behind, so a restart resurrects it",
			len(got), m.NumFiles())
	}
	// Indices must be contiguous and match the rebuilt manifest file for
	// file — a renumbering error here silently pairs one file's stored
	// progress with another file's articles.
	for i := range m.NumFiles() {
		lo, hi := m.FileRange(i)
		if got[i].ArticleCount != hi-lo {
			t.Errorf("job_files[%d].article_count = %d, want %d: rows were not renumbered against the rebuilt manifest",
				i, got[i].ArticleCount, hi-lo)
		}
	}

	// Reloading is the direct statement of the consequence: a fresh Queue
	// built from the store must not bring the discarded volume back.
	reloaded, err := Load(dir, WithStore(store))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rj, err := reloaded.Get(job.ID)
	if err != nil {
		t.Fatalf("the job did not survive the reload: %v", err)
	}
	if n := deferredCount(rj); n != 0 {
		t.Errorf("after a reload the job defers %d files, want 0: the discard did not survive a restart", n)
	}
	if rj.NumFiles() != m.NumFiles() {
		t.Errorf("after a reload the job has %d files, want %d", rj.NumFiles(), m.NumFiles())
	}
	// content-2's downloaded article has to come back attached to content-2.
	// Rows written at the pre-discard indices would land its articles_done on
	// the recovery volume's old slot, so the reloaded job would report the
	// wrong file as downloaded rather than reporting nothing.
	rsnap := reloaded.SnapshotJob(job.ID)
	if rsnap == nil {
		t.Fatal("SnapshotJob after reload returned nil")
	}
	rm, rErr := rsnap.Manifest()
	if rErr != nil {
		t.Fatalf("Manifest after reload: %v", rErr)
	}
	lo, _ := rm.FileRange(1)
	if !rsnap.Progress().ArticleDone(lo) {
		t.Error("content-2's downloaded article is not Done after the reload: job_files rows kept their pre-discard indices")
	}
	if rsnap.Progress().ArticleDone(0) {
		t.Error("content-1 reads as downloaded after the reload, but only content-2 was: stored progress landed on the wrong row")
	}
}

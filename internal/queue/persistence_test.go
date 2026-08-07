package queue

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// ---------- Store round trip ----------

// TestJobStateSurvivesStoreRoundTrip pins that a job's per-article and
// per-file state survives being written down and read back.
//
// The channel is the SQLite store, not a serialized job document: SaveJob and
// LoadJob existed only for the history retry payload, which #298 replaced
// with the NZB backup plus retained per-file progress.
//
// Server stats are deliberately not asserted. They are job-level and the
// store has never persisted them — the payload carried them only as far as
// the history entry's Meta string, which buildHistoryEntry still writes.
func TestJobStateSurvivesStoreRoundTrip(t *testing.T) {
	// File 0 carries the per-article states; file 1 carries the complete
	// flag. Keeping them on separate files is deliberate: a complete file's
	// failed bits are dropped on restore (#300), so seeding both on one
	// file would make this test assert that pre-existing bug rather than
	// the round trip.
	j := makeMultiFileJob(t, "store-roundtrip", 2, 3)
	j.progress.done.Set(0)
	j.progress.done.Set(1)
	j.progress.failed.Set(1)
	j.progress.files[1].Complete = true

	loaded := storeRoundTrip(t, j)

	if loaded.ID != j.ID {
		t.Errorf("ID = %q, want %q", loaded.ID, j.ID)
	}
	if loaded.Name != j.Name {
		t.Errorf("Name = %q, want %q", loaded.Name, j.Name)
	}
	if !loaded.Progress().ArticleDone(0) {
		t.Error("article 0,0 should be Done")
	}
	if !loaded.Progress().ArticleFailed(1) {
		t.Error("article 0,1 should be Failed")
	}
	if loaded.Progress().FileComplete(0) {
		t.Error("file 0 should NOT be Complete")
	}
	if !loaded.Progress().FileComplete(1) {
		t.Error("file 1 should be Complete")
	}
	// Emitted is transient by design (B.6): it is excluded from persistence
	// so a restart re-offers articles that were dispatched but never
	// resolved.
	if loaded.Progress().ArticleEmitted(0) {
		t.Error("Emitted should not survive a round trip")
	}
}

// ---------- Prune ----------

// ---------- Load edge cases ----------

// ---------- Save dirty-flag semantics ----------

func TestSave_ClearsDirtyOnSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q := New()
	_ = q.Add(makeJob(t, "dirty-clear", constants.NormalPriority))

	if !q.IsDirty() {
		t.Fatal("should be dirty after Add")
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if q.IsDirty() {
		t.Error("dirty flag should be cleared after successful Save")
	}
}

func TestSave_RestoresDirtyOnError(t *testing.T) {
	// A failing Store, not an unwritable directory: since #266 a Queue with
	// no Store has nothing to persist and Save cannot fail, so the error
	// path only exists for the store-backed configuration production uses.
	// The property is unchanged — a Save that did not land must leave the
	// queue dirty, or the next checkpoint will skip it and the write is lost
	// for good.
	real, dir := setupResidencyTestStore(t)
	fs := &failingStore{Store: real}
	q := New(WithStore(fs), WithStateDir(dir))
	if err := q.Add(makeJob(t, "dirty-restore", constants.NormalPriority)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	fs.failUpdateBatch = true
	if err := q.Save(dir); err == nil {
		t.Fatal("expected Save to report the store failure")
	}
	if !q.IsDirty() {
		t.Error("dirty flag should be restored after a failed Save; leaving it clear means the next checkpoint skips this state entirely")
	}

	// And it clears again once the store recovers, or the queue would
	// checkpoint on every tick forever.
	fs.failUpdateBatch = false
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save after recovery: %v", err)
	}
	if q.IsDirty() {
		t.Error("dirty flag still set after a successful Save")
	}
}

// ---------- saveStore ----------

// TestSaveStore_Success pins the happy path: jobs and the paused flag both
// land in the store, and Prune runs afterward without error.
func TestSaveStore_Success(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	j := makeMultiFileJob(t, "savestore-ok", 1, 2)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.mu.Lock()
	q.paused = true
	q.mu.Unlock()

	if err := q.saveStore(dir); err != nil {
		t.Fatalf("saveStore: %v", err)
	}

	got, err := store.IsPaused(t.Context())
	if err != nil {
		t.Fatalf("IsPaused: %v", err)
	}
	if !got {
		t.Error("paused flag did not reach the store")
	}
}

// TestSaveStore_JobsErrorPropagates pins that a failed UpdateBatch is
// reported (wrapped), and that Prune — which trusts the just-written jobs
// row as the live set — never runs when that write failed.
func TestSaveStore_JobsErrorPropagates(t *testing.T) {
	real, dir := setupResidencyTestStore(t)
	fs := &failingStore{Store: real, failUpdateBatch: true}
	q := New(WithStore(fs), WithStateDir(dir))
	j := makeMultiFileJob(t, "savestore-jobs-err", 1, 2)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := q.saveStore(dir)
	if err == nil {
		t.Fatal("saveStore: want error when UpdateBatch fails, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want it to wrap the injected store failure", err)
	}
	if !strings.Contains(err.Error(), "save jobs") {
		t.Errorf("error = %v, want it to mention saving jobs", err)
	}
	if strings.Contains(err.Error(), "prune store") {
		t.Error("Prune ran (or its error surfaced) despite the jobs write failing; Prune must be skipped until the live set is durable")
	}
	// The string check above only pins Prune's *error* not surfacing, which
	// stays true whenever Prune runs and simply succeeds. The actual safety
	// property is that Prune must not run at all on this path — it deletes
	// rows absent from the live set, so running it after a failed jobs write
	// is destructive even if Prune itself reports no error. Assert the call
	// count directly.
	if fs.pruneCalls != 0 {
		t.Errorf("Prune called %d times, want 0 — it must not run when the jobs write failed", fs.pruneCalls)
	}
}

// TestSaveStore_BothWritesFailAreJoined pins that saveStore attempts BOTH
// the jobs write and SetPaused even when the first fails, joining their
// errors rather than returning early after the jobs write — the two writes
// are independent per the errors.Join in saveStore, and a caller diagnosing
// a checkpoint failure needs to see both problems, not just the first one
// hit.
func TestSaveStore_BothWritesFailAreJoined(t *testing.T) {
	real, dir := setupResidencyTestStore(t)
	fs := &failingStore{Store: real, failUpdateBatch: true, failSetPaused: true}
	q := New(WithStore(fs), WithStateDir(dir))
	j := makeMultiFileJob(t, "savestore-both-err", 1, 2)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := q.saveStore(dir)
	if err == nil {
		t.Fatal("saveStore: want error when both UpdateBatch and SetPaused fail, got nil")
	}
	if !strings.Contains(err.Error(), "save jobs") {
		t.Errorf("error = %v, want it to mention saving jobs", err)
	}
	if !strings.Contains(err.Error(), "save paused state") {
		t.Errorf("error = %v, want it to mention saving paused state", err)
	}
	if fs.pruneCalls != 0 {
		t.Errorf("Prune called %d times, want 0 — both writes failed, so the live set is not durable", fs.pruneCalls)
	}
}

// TestSaveStore_PausedErrorPropagates pins that a failed SetPaused is
// reported (wrapped) even though the jobs write succeeded — the two writes
// are attempted independently and their errors joined, rather than the
// first success hiding the second failure.
func TestSaveStore_PausedErrorPropagates(t *testing.T) {
	real, dir := setupResidencyTestStore(t)
	fs := &failingStore{Store: real, failSetPaused: true}
	q := New(WithStore(fs), WithStateDir(dir))
	j := makeMultiFileJob(t, "savestore-paused-err", 1, 2)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := q.saveStore(dir)
	if err == nil {
		t.Fatal("saveStore: want error when SetPaused fails, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want it to wrap the injected store failure", err)
	}
	if !strings.Contains(err.Error(), "save paused state") {
		t.Errorf("error = %v, want it to mention saving paused state", err)
	}

	// The job write itself must still have landed — only the paused flag
	// failed.
	loaded, err := real.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, lj := range loaded {
		if lj.ID == j.ID {
			found = true
		}
	}
	if !found {
		t.Error("job write did not land even though only SetPaused was made to fail")
	}

	// The skip-Prune guard is errors.Join(jobsErr, pausedErr), so it fires
	// when EITHER write fails. This is the half the other two tests do not
	// reach, and it is the half a narrowing to `if jobsErr != nil` would
	// break silently: the jobs write above provably landed, so the live set
	// is durable and the narrower "only safe once the live set is written"
	// reading would permit Prune here. The guard is deliberately broader
	// than that — see its comment in saveStore — and this pins the breadth.
	if fs.pruneCalls != 0 {
		t.Errorf("pruneCalls = %d, want 0: Prune must not run in a cycle where either write failed", fs.pruneCalls)
	}
}

// TestSaveStore_PruneErrorPropagates pins that a Prune failure, reached only
// once both writes above succeed, is reported wrapped rather than swallowed.
func TestSaveStore_PruneErrorPropagates(t *testing.T) {
	real, dir := setupResidencyTestStore(t)
	fs := &failingStore{Store: real, failPrune: true}
	q := New(WithStore(fs), WithStateDir(dir))
	j := makeMultiFileJob(t, "savestore-prune-err", 1, 2)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := q.saveStore(dir)
	if err == nil {
		t.Fatal("saveStore: want error when Prune fails, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want it to wrap the injected store failure", err)
	}
	if !strings.Contains(err.Error(), "prune store") {
		t.Errorf("error = %v, want it to mention pruning the store", err)
	}
}

// ---------- readGzJSON ----------

// TestReadGzJSON_RoundTrip pins the success path shared with writeGzJSON:
// what was encoded comes back equal.
func TestReadGzJSON_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roundtrip.json.gz")
	type payload struct {
		Name  string
		Count int
	}
	want := payload{Name: "widget", Count: 7}
	if err := writeGzJSON(path, want); err != nil {
		t.Fatalf("writeGzJSON: %v", err)
	}

	var got payload
	if err := readGzJSON(path, &got); err != nil {
		t.Fatalf("readGzJSON: %v", err)
	}
	if got != want {
		t.Errorf("readGzJSON = %+v, want %+v", got, want)
	}
}

// TestReadGzJSON_MissingFile pins that a missing path surfaces as (wrapped)
// os.ErrNotExist, letting callers like hydrateJobLocked distinguish "never
// persisted" from a real I/O error.
func TestReadGzJSON_MissingFile(t *testing.T) {
	dir := t.TempDir()
	var v map[string]int
	err := readGzJSON(filepath.Join(dir, "missing.json.gz"), &v)
	if err == nil {
		t.Fatal("readGzJSON: want error for a missing file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want it to wrap os.ErrNotExist", err)
	}
}

// TestReadGzJSON_NotGzip pins the gunzip-failure branch: a file that exists
// but is not valid gzip data must error rather than hand back garbage.
func TestReadGzJSON_NotGzip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-gzip.json.gz")
	if err := os.WriteFile(path, []byte("not actually gzip data"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var v map[string]int
	err := readGzJSON(path, &v)
	if err == nil {
		t.Fatal("readGzJSON: want error for non-gzip content, got nil")
	}
	if !strings.Contains(err.Error(), "gzip") {
		t.Errorf("error = %v, want it to mention gzip", err)
	}
}

// TestReadGzJSON_CorruptJSON pins the decode-failure branch: valid gzip
// wrapping invalid JSON must error rather than silently leave v untouched.
func TestReadGzJSON_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json.gz")
	// writeGzJSON only ever encodes valid JSON, so build the gzip stream by
	// hand around a payload that is not valid JSON.
	f, err := os.Create(path) //nolint:gosec // test fixture path under t.TempDir()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write([]byte("{not valid json")); err != nil {
		t.Fatalf("gzip Write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var v map[string]int
	err = readGzJSON(path, &v)
	if err == nil {
		t.Fatal("readGzJSON: want error for corrupt JSON, got nil")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %v, want it to mention decode failure", err)
	}
}

// ---------- Save/Load no-leftover temp files ----------

// TestManifestWrite_NoLeftoverTempFiles pins that repeated manifest writes
// leave no temp files behind. The manifest is now the only gzipped JSON the
// store writes, so it is where the atomic temp+fsync+rename pattern has to
// hold.
func TestManifestWrite_NoLeftoverTempFiles(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)

	for i := range 5 {
		j := makeMultiFileJob(t, fmt.Sprintf("clean-%d", i), 1, 2)
		if err := store.Add(t.Context(), j); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(dir, "manifests"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

// TestQueueSaveLoad_TransientCountersRecomputed verifies that transient fields
// excluded from JSON (Pending, PendingArticles, BytesDownloaded, FileIdx,
// Emitted) are correctly recomputed by recomputePending after a Save+Load
// cycle. These fields have json:"-" tags and are the canonical in-memory
// counters that drive dispatch and early-abort checks — if they drift from
// ground truth after a restart the downloader hangs or over-dispatches.
//
// Test state intentionally covers all article lifecycle states:
//   - Done + !Failed  → counted in BytesDownloaded, not Pending
//   - Done + Failed   → not in BytesDownloaded, not Pending
//   - Emitted         → not Pending (Emitted is cleared on load — treated as pending)
//   - !Done + !Emitted → Pending
func TestQueueSaveLoad_TransientCountersRecomputed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Build a 2-file, 3-article-each job so we have room to put every state.
	q := New()
	q.stateDir = dir
	j := makeMultiFileJob(t, "round-trip", 2, 3)
	if err := q.Add(j); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Apply mutations through the public API to establish a known state.
	//
	// File 0:
	//   art[0]: Done (success) → BytesDownloaded += Bytes
	//   art[1]: Done + Failed  → FailedBytes += Bytes, not BytesDownloaded
	//   art[2]: Emitted        → not Pending (will be cleared on load → pending after reload)
	// File 1:
	//   art[0]: !Done, !Emitted → Pending
	//   art[1]: !Done, !Emitted → Pending
	//   art[2]: !Done, !Emitted → Pending

	id := j.ID
	m := mustManifest(t, j)
	// File 0 occupies global article indices [0,3); file 1 is left pristine.

	if err := q.MarkArticlesDone(id, []string{m.ArticleID(0)}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if _, err := q.MarkArticlesFailed(id, []string{m.ArticleID(1)}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}
	if err := q.MarkArticleEmitted(id, m.ArticleID(2)); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}

	// After load, Emitted flags are always cleared (json:"-") so arts0[2]
	// becomes pending again — it was in-flight and needs to be re-dispatched.
	// Compute expected post-load counters from first principles:
	//   File 0: art[0] Done success, art[1] Done failed, art[2] becomes pending
	//           → Pending=1, BytesDownloaded=art[0].Bytes
	//   File 1: all 3 articles untouched → Pending=3, BytesDownloaded=0
	//   PendingArticles = 1 + 3 = 4
	artBytes := int64(m.ArticleBytes(0)) // all articles same size
	wantPendingFile0 := 1                // arts0[2] emitted→cleared→pending
	wantPendingFile1 := 3                // all pristine
	wantPendingArticles := 4             // 1 + 3
	wantBytesDownloaded0 := artBytes     // only arts0[0] (successful Done)

	// Persist and reload through the store, which is now the only thing
	// that writes a live job down: the whole-queue JSON engine went in #266
	// and the per-job document went in #298. The property under test —
	// transient counters recomputed and emitted cleared on load — is
	// unchanged by either.
	got := storeRoundTrip(t, j)

	gm, gp := mustManifest(t, got), got.Progress()

	// Emitted must always be cleared on load — ClearAllEmitted is called by
	// app.Start and recompute resets the in-memory bit.
	for i := range gm.NumArticles() {
		if gp.ArticleEmitted(i) {
			t.Errorf("article %d Emitted survived load (should be cleared)", i)
		}
	}

	// Transient pending counters must equal the pre-save values.
	// (The emitted article is now treated as pending since Emitted was cleared.)
	if gp.FilePending(0) != wantPendingFile0 {
		t.Errorf("Files[0].Pending: got %d, want %d", gp.FilePending(0), wantPendingFile0)
	}
	if gp.FilePending(1) != wantPendingFile1 {
		t.Errorf("Files[1].Pending: got %d, want %d", gp.FilePending(1), wantPendingFile1)
	}
	if gp.PendingArticles() != wantPendingArticles {
		t.Errorf("PendingArticles: got %d, want %d", gp.PendingArticles(), wantPendingArticles)
	}

	// BytesDownloaded: only successful Done articles (not failed ones).
	if gp.FileBytesDownloaded(0) != wantBytesDownloaded0 {
		t.Errorf("Files[0].BytesDownloaded: got %d, want %d", gp.FileBytesDownloaded(0), wantBytesDownloaded0)
	}
	if gp.FileBytesDownloaded(1) != 0 {
		t.Errorf("Files[1].BytesDownloaded: got %d, want 0 (no articles done)", gp.FileBytesDownloaded(1))
	}

	// Every article must resolve back to its correct file via
	// fileIndexForArticle — now a pure function of FileRange rather than a
	// cached per-article back-pointer, so this pins FileRange consistency.
	for fi := range gm.NumFiles() {
		lo, hi := gm.FileRange(fi)
		for i := lo; i < hi; i++ {
			if got := gm.fileIndexForArticle(i); got != fi {
				t.Errorf("fileIndexForArticle(%d) = %d, want %d", i, got, fi)
			}
		}
	}
}

// TestPersistenceRoundTrip_AccessorParity is the first-class acceptance
// criterion for the #205 Manifest/JobProgress split: build a job, drive it
// through a representative set of mutations (mark articles done/failed,
// undefer one recovery volume, discard the other still-deferred one),
// marshal it, unmarshal into a fresh Job (plus the post-unmarshal recompute
// Load/LoadJob perform), and assert every accessor matches — a correctness
// round-trip, not a byte-compatibility one. Also confirms messageIDIndex/
// emitted/the derived counters never appear in the marshaled bytes.
func TestPersistenceRoundTrip_AccessorParity(t *testing.T) {
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "content1.bin", Bytes: 200, Articles: []nzb.Article{
			{ID: "c1a@x", Bytes: 100, Number: 1},
			{ID: "c1b@x", Bytes: 100, Number: 2},
		}},
		{Subject: "content2.bin", Bytes: 100, Articles: []nzb.Article{{ID: "c2@x", Bytes: 100, Number: 1}}},
		{Subject: `"set.par2" yEnc`, Bytes: 50, Articles: []nzb.Article{{ID: "idx@x", Bytes: 50, Number: 1}}},
		{Subject: `"set.vol000+01.par2" yEnc`, Bytes: 300, Articles: []nzb.Article{{ID: "v1@x", Bytes: 300, Number: 1}}},
		{Subject: `"set.vol001+02.par2" yEnc`, Bytes: 400, Articles: []nzb.Article{{ID: "v2@x", Bytes: 400, Number: 1}}},
	}}
	job, err := NewJob(parsed, AddOptions{Filename: "roundtrip.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	q := New()
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Representative mutation set: done + failed articles, one recovery
	// volume explicitly undeferred, the other discarded while still
	// deferred. Order matters: MarkArticlesFailed auto-releases any
	// still-deferred recovery volumes when Par2Recovered is false, so the
	// explicit undefer+discard must run first (setting Par2Recovered=true)
	// to keep the discard path exercised rather than pre-empted.
	if err := q.MarkArticlesDone(job.ID, []string{"c1a@x"}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if err := q.UndeferRecoveryVolumes(job.ID, []int{3}); err != nil { // set.vol000+01.par2
		t.Fatalf("UndeferRecoveryVolumes: %v", err)
	}
	if err := q.DiscardDeferredPar2(job.ID); err != nil { // discards set.vol001+02.par2
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	if _, err := q.MarkArticlesFailed(job.ID, []string{"c1b@x"}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}

	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// The transient/excluded fields must never appear in the marshaled bytes.
	raw := string(data)
	for _, forbidden := range []string{"messageIDIndex", "emitted", "pendingArticles", "articlesResolved", "articlesFailed", "earlyAborted"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("marshaled JSON unexpectedly contains %q:\n%s", forbidden, raw)
		}
	}

	var loaded Job
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// Load/LoadJob call this after UnmarshalJSON; UnmarshalJSON itself
	// deliberately does not, so replicate that real usage here.
	loaded.progress.recompute(loaded.manifest)

	m, lm := mustManifest(t, job), mustManifest(t, &loaded)
	p, lp := job.Progress(), loaded.Progress()

	if lm.NumFiles() != m.NumFiles() {
		t.Fatalf("NumFiles = %d, want %d", lm.NumFiles(), m.NumFiles())
	}
	for fi := range m.NumFiles() {
		if lm.FileSubject(fi) != m.FileSubject(fi) {
			t.Errorf("file %d Subject = %q, want %q", fi, lm.FileSubject(fi), m.FileSubject(fi))
		}
		if lm.FileBytes(fi) != m.FileBytes(fi) {
			t.Errorf("file %d Bytes = %d, want %d", fi, lm.FileBytes(fi), m.FileBytes(fi))
		}
		if lm.FileIsPar2Recovery(fi) != m.FileIsPar2Recovery(fi) {
			t.Errorf("file %d IsPar2Recovery = %v, want %v", fi, lm.FileIsPar2Recovery(fi), m.FileIsPar2Recovery(fi))
		}
		if lp.FileComplete(fi) != p.FileComplete(fi) {
			t.Errorf("file %d Complete = %v, want %v", fi, lp.FileComplete(fi), p.FileComplete(fi))
		}
		if lp.FileFetchPolicy(fi) != p.FileFetchPolicy(fi) {
			t.Errorf("file %d FetchPolicy = %v, want %v", fi, lp.FileFetchPolicy(fi), p.FileFetchPolicy(fi))
		}
	}
	if lm.NumArticles() != m.NumArticles() {
		t.Fatalf("NumArticles = %d, want %d", lm.NumArticles(), m.NumArticles())
	}
	for i := range m.NumArticles() {
		if lm.ArticleID(i) != m.ArticleID(i) {
			t.Errorf("article %d ID = %q, want %q", i, lm.ArticleID(i), m.ArticleID(i))
		}
		if lp.ArticleDone(i) != p.ArticleDone(i) {
			t.Errorf("article %d Done = %v, want %v", i, lp.ArticleDone(i), p.ArticleDone(i))
		}
		if lp.ArticleFailed(i) != p.ArticleFailed(i) {
			t.Errorf("article %d Failed = %v, want %v", i, lp.ArticleFailed(i), p.ArticleFailed(i))
		}
	}
	if lm.TotalBytes() != m.TotalBytes() {
		t.Errorf("TotalBytes = %d, want %d", lm.TotalBytes(), m.TotalBytes())
	}
	if lm.RecoveryBytes() != m.RecoveryBytes() {
		t.Errorf("Par2Bytes = %d, want %d", lm.RecoveryBytes(), m.RecoveryBytes())
	}
	if lm.RecoveryFiles() != m.RecoveryFiles() {
		t.Errorf("Par2Files = %d, want %d", lm.RecoveryFiles(), m.RecoveryFiles())
	}
	if lp.RemainingBytes() != p.RemainingBytes() {
		t.Errorf("RemainingBytes = %d, want %d", lp.RemainingBytes(), p.RemainingBytes())
	}
	if lp.FailedBytes() != p.FailedBytes() {
		t.Errorf("FailedBytes = %d, want %d", lp.FailedBytes(), p.FailedBytes())
	}
	if lp.Par2Recovered() != p.Par2Recovered() {
		t.Errorf("Par2Recovered = %v, want %v", lp.Par2Recovered(), p.Par2Recovered())
	}
	if lp.PendingArticles() != p.PendingArticles() {
		t.Errorf("PendingArticles = %d, want %d (recomputed)", lp.PendingArticles(), p.PendingArticles())
	}
	if lp.ArticlesResolved() != p.ArticlesResolved() {
		t.Errorf("ArticlesResolved = %d, want %d (recomputed)", lp.ArticlesResolved(), p.ArticlesResolved())
	}
	if lp.ArticlesFailed() != p.ArticlesFailed() {
		t.Errorf("ArticlesFailed = %d, want %d (recomputed)", lp.ArticlesFailed(), p.ArticlesFailed())
	}
}

// ---------- Direct Persistence/Job Helpers ----------

func TestJob_RecomputePendingAndLazyArticleByID_Direct(t *testing.T) {
	job := &Job{ID: "test-job"}
	job.manifest = newManifest([]JobFile{
		{
			Subject: "file1",
			Articles: []JobArticle{
				{ID: "art1"},
				{ID: "art2", Bytes: 100},
			},
		},
	})
	job.progress = newJobProgress(job.manifest)
	job.progress.done.Set(1) // art2 done, not failed

	job.progress.recompute(job.manifest)

	if job.progress.files[0].Pending != 1 {
		t.Errorf("expected file 0 pending 1, got %d", job.progress.files[0].Pending)
	}
	if job.progress.files[0].BytesDownloaded != 100 {
		t.Errorf("expected file 0 bytes downloaded 100, got %d", job.progress.files[0].BytesDownloaded)
	}

	// No manual buildMessageIDIndex call: articleIndexByID must build the
	// index lazily on its own from a manifest whose index was never touched.
	idx, ok := job.manifest.articleIndexByID("art1")
	if !ok || job.manifest.ArticleID(idx) != "art1" {
		t.Fatal("expected to find art1")
	}
}

// TestStoreRoundTrip_RecomputesPending pins that the transient pending
// counters are rebuilt on load. They are excluded from persistence, so a job
// that came back with them at zero would never dispatch.
func TestStoreRoundTrip_RecomputesPending(t *testing.T) {
	j := makeMultiFileJob(t, "load-recompute", 1, 3)
	// Articles start !Done, !Emitted from NewJob — all pending by default.

	loaded := storeRoundTrip(t, j)

	if loaded.Progress().PendingArticles() != 3 {
		t.Errorf("loaded.PendingArticles = %d, want 3", loaded.Progress().PendingArticles())
	}
	if loaded.Progress().FilePending(0) != 3 {
		t.Errorf("loaded.Files[0].Pending = %d, want 3", loaded.Progress().FilePending(0))
	}
}

func TestSave_SetsStateDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q := New(WithStateDir(dir))
	j := makeJob(t, "backup-cleanup", constants.NormalPriority)
	if err := q.Add(j); err != nil {
		t.Fatal(err)
	}

	if err := q.Save(dir); err != nil {
		t.Fatal(err)
	}

	// Save records the state directory, which is where manifests are read
	// from and is the only thing it persists for a store-less queue.
	q.mu.RLock()
	got := q.stateDir
	q.mu.RUnlock()
	if got != dir {
		t.Errorf("stateDir = %q after Save, want %q", got, dir)
	}

	// This used to go on to assert that Save wrote jobs/<id>.json.gz and that
	// Remove deleted it. Both belonged to the whole-queue JSON engine removed
	// in #266. Manifest unlinking on Remove is store-gated, so it is not the
	// equivalent property for a store-less queue and is covered where it
	// actually happens rather than asserted speculatively here.
}

func TestWriteGzJSON_EncodeError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "encode_err.json.gz")
	// Channels cannot be JSON-marshaled, causing enc.Encode to fail.
	err := writeGzJSON(path, make(chan int))
	if err == nil {
		t.Fatal("expected encode error when marshaling channel, got nil")
	}
}

func TestLoad_WithLogger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l := slog.Default()
	q, err := Load(dir, WithLogger(l))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if q.log == nil {
		t.Error("expected logger to be set on reloaded queue, got nil")
	}
}

// Load's restart path for a job that was active when the daemon stopped: the
// store hands it back with its manifest already read and its per-article
// progress restored, so the daemon resumes the download instead of starting
// it over.
func TestLoad_RehydratesResidentJob(t *testing.T) {
	store, dir := setupResidencyTestStore(t)

	seed := New(WithStore(store), WithStateDir(dir))
	job := makeMultiFileJob(t, "load-resident", 1, 2)
	if err := seed.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := seed.SetStatus(job.ID, constants.StatusDownloading); err != nil {
		t.Fatalf("SetStatus(Downloading): %v", err)
	}
	if err := seed.MarkArticlesDone(job.ID, []string{articleID(0, 0)}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	// Save while resident: SQLiteStore.Update's per-file flush is guarded on
	// the manifest being readable, so a save after eviction never writes
	// articles_done.
	if err := seed.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir, WithStore(store), WithStateDir(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := loaded.Get(job.ID)
	if err != nil {
		t.Fatalf("Get after Load: %v", err)
	}
	if !manifestResident(got) {
		t.Fatal("a job that was downloading came back non-resident; it cannot be dispatched")
	}
	if !got.Progress().ArticleDone(0) {
		t.Error("the already-downloaded article came back undone, so the restart would re-fetch it")
	}
	m, err := got.Manifest()
	if err != nil {
		t.Fatalf("Manifest after Load: %v", err)
	}
	if _, ok := m.articleIndexByID(articleID(0, 1)); !ok {
		t.Error("the message-ID index was not built, so article lookups by ID will miss")
	}
}

// Load fails closed when the store cannot answer. Every one of these queries
// feeds progress sizing or byte accounting for the whole queue, so degrading
// to a partial answer would put the daemon back in the silent-wrong-numbers
// state the residency work removed.
func TestLoad_StoreQueryFailuresPropagate(t *testing.T) {
	real, dir := setupResidencyTestStore(t)
	for _, tc := range []struct {
		name string
		set  func(*failingStore)
	}{
		{"List", func(f *failingStore) { f.failList = true }},
		{"IsPaused", func(f *failingStore) { f.failIsPaused = true }},
		{"ArticleCountsByJob", func(f *failingStore) { f.failArticleCounts = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &failingStore{Store: real}
			tc.set(fs)
			if _, err := Load(dir, WithStore(fs), WithStateDir(dir)); err == nil {
				t.Errorf("Load returned nil after %s failed; the queue would come up with silently wrong state", tc.name)
			}
		})
	}
}

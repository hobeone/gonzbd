package queue

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// ---------- LoadJob / SaveJob ----------

func TestSaveJobLoadJob_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs", "testjob.json.gz")

	j := makeMultiFileJob(t, "savejob-roundtrip", 2, 3)
	j.progress.done[0] = true
	j.progress.failed[1] = true
	j.progress.files[0].Complete = true
	j.progress.remainingBytes = 400_000
	j.progress.failedBytes = 100_000
	j.progress.serverStats = map[string]int64{"srv-a": 50_000}

	if err := SaveJob(path, j); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}

	loaded, err := LoadJob(path)
	if err != nil {
		t.Fatalf("LoadJob: %v", err)
	}

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
	if !loaded.Progress().FileComplete(0) {
		t.Error("file 0 should be Complete")
	}
	if loaded.Progress().FileComplete(1) {
		t.Error("file 1 should NOT be Complete")
	}
	if loaded.Progress().RemainingBytes() != 400_000 {
		t.Errorf("RemainingBytes = %d, want 400000", loaded.Progress().RemainingBytes())
	}
	if loaded.Progress().FailedBytes() != 100_000 {
		t.Errorf("FailedBytes = %d, want 100000", loaded.Progress().FailedBytes())
	}
	if loaded.Progress().ServerStats()["srv-a"] != 50_000 {
		t.Errorf("ServerStats[srv-a] = %d, want 50000", loaded.Progress().ServerStats()["srv-a"])
	}
	// Emitted flag must NOT survive serialization.
	if loaded.Progress().ArticleEmitted(0) {
		t.Error("Emitted should not survive load")
	}
}

func TestSaveJob_CreatesDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "job.json.gz")

	j := makeJob(t, "mkdirs", constants.NormalPriority)
	if err := SaveJob(path, j); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should exist at %s: %v", path, err)
	}
}

func TestLoadJob_NotExist(t *testing.T) {
	t.Parallel()
	_, err := LoadJob("/nonexistent/path/job.json.gz")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestLoadJob_CorruptGzip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json.gz")
	if err := os.WriteFile(path, []byte("not gzip data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadJob(path)
	if err == nil {
		t.Error("expected error for corrupt gzip")
	}
}

func TestSaveJobLoadJob_Overwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "overwrite.json.gz")

	j1 := makeJob(t, "first", constants.NormalPriority)
	if err := SaveJob(path, j1); err != nil {
		t.Fatalf("SaveJob 1: %v", err)
	}

	j2 := makeJob(t, "second", constants.HighPriority)
	if err := SaveJob(path, j2); err != nil {
		t.Fatalf("SaveJob 2: %v", err)
	}

	loaded, err := LoadJob(path)
	if err != nil {
		t.Fatalf("LoadJob: %v", err)
	}
	if loaded.ID != j2.ID {
		t.Errorf("loaded job should be the second one (overwritten)")
	}
}

// ---------- Prune ----------

func TestPrune_RemovesOrphans(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q := New()
	j := makeJob(t, "keeper", constants.NormalPriority)
	_ = q.Add(j)

	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Create orphaned job files that aren't in the index.
	jobsDir := filepath.Join(dir, "jobs")
	orphanPath := filepath.Join(jobsDir, "orphan-id.json.gz")
	if err := os.WriteFile(orphanPath, []byte("junk"), 0o644); err != nil {
		t.Fatalf("create orphan: %v", err)
	}

	// Create a crash-orphaned temp file.
	tmpPath := filepath.Join(jobsDir, "somefile.json.gz.tmp.12345")
	if err := os.WriteFile(tmpPath, []byte("temp"), 0o644); err != nil {
		t.Fatalf("create tmp: %v", err)
	}

	// Load and prune.
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Prune is called by Load, so orphans should already be gone.
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("orphan should have been pruned, err = %v", err)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temp file should have been pruned, err = %v", err)
	}

	// The valid job should still be loaded.
	if loaded.Len() != 1 {
		t.Errorf("Len = %d, want 1", loaded.Len())
	}
}

func TestPrune_PreservesNonJobFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q := New()
	j := makeJob(t, "valid", constants.NormalPriority)
	_ = q.Add(j)
	_ = q.Save(dir)

	// Create a non-.json.gz file — should NOT be pruned.
	jobsDir := filepath.Join(dir, "jobs")
	otherFile := filepath.Join(jobsDir, "readme.txt")
	if err := os.WriteFile(otherFile, []byte("hello"), 0o644); err != nil {
		t.Fatalf("create other: %v", err)
	}

	loaded, _ := Load(dir)
	_ = loaded // trigger Prune

	if _, err := os.Stat(otherFile); err != nil {
		t.Errorf("non-job file should be preserved: %v", err)
	}
}

func TestPrune_NoStateDir(t *testing.T) {
	t.Parallel()
	q := New()
	// Prune with empty stateDir should be a no-op (no panic).
	q.Prune()
}

// ---------- Load edge cases ----------

func TestLoad_SkipsOrphanedIndexEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Save a queue with two jobs.
	q := New()
	a := makeJob(t, "job-a", constants.NormalPriority)
	b := makeJob(t, "job-b", constants.NormalPriority)
	_ = q.Add(a)
	_ = q.Add(b)
	_ = q.Save(dir)

	// Delete job-b's file to simulate a crash.
	os.Remove(filepath.Join(dir, "jobs", b.ID+".json.gz"))

	// Load should skip the missing job without error.
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Len() != 1 {
		t.Errorf("Len = %d, want 1 (orphaned entry skipped)", loaded.Len())
	}
	if _, err := loaded.Get(a.ID); err != nil {
		t.Errorf("surviving job should be loadable: %v", err)
	}
}

func TestLoad_CorruptJobFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	q := New()
	j1 := makeJob(t, "good", constants.NormalPriority)
	j2 := makeJob(t, "corrupt", constants.NormalPriority)
	_ = q.Add(j1)
	_ = q.Add(j2)
	_ = q.Save(dir)

	// Corrupt job-b's file
	jobPath := filepath.Join(dir, "jobs", j2.ID+".json.gz")
	if err := os.WriteFile(jobPath, []byte("corrupt data"), 0o644); err != nil {
		t.Fatalf("corrupt job file: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Good job should be loaded, corrupt one skipped
	if loaded.Len() != 1 {
		t.Errorf("expected 1 job, got %d", loaded.Len())
	}
	if _, err := loaded.Get(j1.ID); err != nil {
		t.Errorf("good job should be loaded: %v", err)
	}

	// Assert corrupt job file was renamed
	if _, err := os.Stat(jobPath); !os.IsNotExist(err) {
		t.Errorf("expected original job file to be gone, got: %v", err)
	}
	if _, err := os.Stat(jobPath + ".corrupt"); err != nil {
		t.Errorf("expected corrupt job file to exist, got: %v", err)
	}
}

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
	t.Parallel()
	q := New()
	_ = q.Add(makeJob(t, "dirty-restore", constants.NormalPriority))

	// Save to a nonexistent directory to trigger an error.
	err := q.Save("/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Fatal("expected Save error for nonexistent directory")
	}
	if !q.IsDirty() {
		t.Error("dirty flag should be restored after failed Save")
	}
}

// ---------- Save/Load no-leftover temp files ----------

func TestSaveJob_NoLeftoverTempFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	jobsDir := filepath.Join(dir, "jobs")

	j := makeMultiFileJob(t, "clean", 1, 2)
	path := filepath.Join(jobsDir, j.ID+".json.gz")

	// Save multiple times.
	for i := range 5 {
		if err := SaveJob(path, j); err != nil {
			t.Fatalf("SaveJob %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(jobsDir)
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
	m := j.Manifest()
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

	// Persist.
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload.
	q2, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := q2.SnapshotJob(id)
	if got == nil {
		t.Fatal("SnapshotJob returned nil after Load")
	}

	gm, gp := got.Manifest(), got.Progress()

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

	m, lm := job.Manifest(), loaded.Manifest()
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
		if lp.FileDeferred(fi) != p.FileDeferred(fi) {
			t.Errorf("file %d Deferred = %v, want %v", fi, lp.FileDeferred(fi), p.FileDeferred(fi))
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
	if lm.Par2Bytes() != m.Par2Bytes() {
		t.Errorf("Par2Bytes = %d, want %d", lm.Par2Bytes(), m.Par2Bytes())
	}
	if lm.Par2Files() != m.Par2Files() {
		t.Errorf("Par2Files = %d, want %d", lm.Par2Files(), m.Par2Files())
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

func TestReadWriteGzJSONRaw_Direct(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.json.gz")

	type dummy struct {
		Name string `json:"name"`
		Val  int    `json:"val"`
	}
	original := dummy{Name: "hello", Val: 42}

	data, jsonErr := json.Marshal(original)
	if jsonErr != nil {
		t.Fatal(jsonErr)
	}

	if err := writeGzJSONRaw(path, data); err != nil {
		t.Fatalf("writeGzJSONRaw failed: %v", err)
	}

	var loaded dummy
	if err := readGzJSON(path, &loaded); err != nil {
		t.Fatalf("readGzJSON failed: %v", err)
	}

	if loaded != original {
		t.Errorf("loaded %+v, want %+v", loaded, original)
	}
}

func TestQueue_SaveInner_Direct(t *testing.T) {
	tmp := t.TempDir()
	q := New()
	job1 := &Job{ID: "job1", Name: "Job 1"}
	job1.manifest = newManifest([]JobFile{
		{
			Subject:  "subject1",
			Articles: []JobArticle{{ID: "art1", Bytes: 100}},
		},
	})
	job1.progress = newJobProgress(job1.manifest)
	_ = q.Add(job1)

	if err := q.saveInner(tmp); err != nil {
		t.Fatalf("saveInner failed: %v", err)
	}

	var loadedJob Job
	if err := readGzJSON(filepath.Join(tmp, "jobs", "job1.json.gz"), &loadedJob); err != nil {
		t.Fatalf("failed to read job: %v", err)
	}
	if loadedJob.ID != "job1" {
		t.Errorf("loaded job mismatch: %+v", &loadedJob)
	}
}

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
	job.progress.done[1] = true // art2 done, not failed

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

func TestLoadJob_RecomputesPending(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs", "pending.json.gz")

	j := makeMultiFileJob(t, "load-recompute", 1, 3)
	// Articles start !Done, !Emitted from NewJob — all pending by default.

	if err := SaveJob(path, j); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}

	loaded, err := LoadJob(path)
	if err != nil {
		t.Fatalf("LoadJob: %v", err)
	}

	// Verify that loaded job has recomputed pending counters
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
	q := New()
	j := makeJob(t, "backup-cleanup", constants.NormalPriority)
	if err := q.Add(j); err != nil {
		t.Fatal(err)
	}

	if err := q.Save(dir); err != nil {
		t.Fatal(err)
	}

	jobPath := filepath.Join(dir, "jobs", j.ID+".json.gz")
	if _, err := os.Stat(jobPath); err != nil {
		t.Fatalf("expected job backup file to exist at %s, got err: %v", jobPath, err)
	}

	if err := q.Remove(j.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(jobPath); !os.IsNotExist(err) {
		t.Errorf("expected job backup file to be deleted, but it still exists (err: %v)", err)
	}
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

func TestWriteGzJSONRaw_WriteError(t *testing.T) {
	// Do NOT call t.Parallel() because Setrlimit mutates process-global state.
	var oldRlim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &oldRlim); err != nil {
		t.Skipf("Getrlimit not supported: %v", err)
	}

	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)

	newRlim := syscall.Rlimit{Cur: 10, Max: oldRlim.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &newRlim); err != nil {
		t.Skipf("Setrlimit not supported: %v", err)
	}
	defer func() {
		_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &oldRlim)
	}()

	dir := t.TempDir()
	path := filepath.Join(dir, "fsize_err.json.gz")
	// Writing a large incompressible payload exceeds the 10-byte file size limit during gz.Write, causing it to fail.
	data := make([]byte, 500_000)
	val := uint32(1)
	for i := range data {
		val = val*1103515245 + 12345
		data[i] = byte(val >> 16)
	}
	err := writeGzJSONRaw(path, data)
	if err == nil {
		t.Fatal("expected write error when exceeding RLIMIT_FSIZE, got nil")
	}
}

func TestQuarantineFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "testfile.json.gz")
	if err := os.WriteFile(path, []byte("test data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	l := &Loader{}
	err := l.quarantineFile(path)
	if err != nil {
		t.Fatalf("quarantineFile failed: %v", err)
	}

	// Assert original is gone
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected original file to be gone, got: %v", err)
	}

	// Assert corrupt file exists and has correct content
	corruptPath := path + ".corrupt"
	data, err := os.ReadFile(corruptPath)
	if err != nil {
		t.Errorf("failed to read corrupt file: %v", err)
	} else if string(data) != "test data" {
		t.Errorf("corrupt file content = %q, want %q", string(data), "test data")
	}
}

func TestQuarantineFile_NotExist(t *testing.T) {
	t.Parallel()
	l := &Loader{}
	err := l.quarantineFile("/nonexistent/path/file.json.gz")
	if err == nil {
		t.Error("expected error when quarantining nonexistent file, got nil")
	}
}

func TestLoad_CorruptIndexQuarantined(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create a corrupt index file
	idxPath := filepath.Join(dir, "queue.json.gz")
	if err := os.WriteFile(idxPath, []byte("corrupt gzip data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	q, err := Load(dir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if q.Len() != 0 {
		t.Errorf("expected empty queue, got len %d", q.Len())
	}
	if q.stateDir != dir {
		t.Errorf("expected stateDir = %q, got %q", dir, q.stateDir)
	}

	// Assert index was renamed
	if _, err := os.Stat(idxPath); !os.IsNotExist(err) {
		t.Errorf("expected original index to be gone, got: %v", err)
	}
	if _, err := os.Stat(idxPath + ".corrupt"); err != nil {
		t.Errorf("expected corrupt index to exist, got: %v", err)
	}
}

func TestLoad_CorruptIndexQuarantineFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	idxPath := filepath.Join(dir, "queue.json.gz")
	if err := os.WriteFile(idxPath, []byte("corrupt gzip data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Mock rename to fail via Loader dependency injection
	l := &Loader{
		Rename: func(oldpath, newpath string) error {
			return errors.New("mock rename error")
		},
	}

	_, err := l.Load(dir)
	if err == nil {
		t.Fatal("expected error due to quarantine failure, got nil")
	}
	if !strings.Contains(err.Error(), "failed and could not quarantine") {
		t.Errorf("expected error to mention quarantine failure, got: %v", err)
	}

	// Verify the original file is still there
	if _, err := os.Stat(idxPath); err != nil {
		t.Errorf("expected original corrupt file to still exist, got: %v", err)
	}
}

func TestLoad_CorruptJobQuarantineFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	q := New()
	j1 := makeJob(t, "good", constants.NormalPriority)
	j2 := makeJob(t, "corrupt", constants.NormalPriority)
	_ = q.Add(j1)
	_ = q.Add(j2)
	_ = q.Save(dir)

	// Corrupt job-b's file
	jobPath := filepath.Join(dir, "jobs", j2.ID+".json.gz")
	if err := os.WriteFile(jobPath, []byte("corrupt data"), 0o644); err != nil {
		t.Fatalf("corrupt job file: %v", err)
	}

	// Mock rename to fail via Loader dependency injection
	l := &Loader{
		Rename: func(oldpath, newpath string) error {
			return errors.New("mock rename error")
		},
	}

	_, err := l.Load(dir)
	if err == nil {
		t.Fatal("expected error due to quarantine failure, got nil")
	}
	if !strings.Contains(err.Error(), "failed and could not quarantine") {
		t.Errorf("expected error to mention quarantine failure, got: %v", err)
	}

	// Verify the original file is still there
	if _, err := os.Stat(jobPath); err != nil {
		t.Errorf("expected original corrupt job file to still exist, got: %v", err)
	}
}

func TestLoad_PermissionError(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("permission tests are unreliable when running as root")
	}

	t.Run("IndexPermissionError", func(t *testing.T) {
		dir := t.TempDir()
		idxPath := filepath.Join(dir, "queue.json.gz")
		if err := os.WriteFile(idxPath, []byte("some data"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Make index unreadable
		if err := os.Chmod(idxPath, 0000); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = os.Chmod(idxPath, 0644) // restore for cleanup
		}()

		_, err := Load(dir)
		if err == nil {
			t.Error("expected error for unreadable index")
		}
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("expected permission error, got: %v", err)
		}
		// Verify it was NOT quarantined
		if _, err := os.Stat(idxPath + ".corrupt"); !os.IsNotExist(err) {
			t.Error("index should not have been quarantined")
		}
	})

	t.Run("JobPermissionError", func(t *testing.T) {
		dir := t.TempDir()
		q := New()
		j := makeJob(t, "perm-job", constants.NormalPriority)
		_ = q.Add(j)
		_ = q.Save(dir)

		jobPath := filepath.Join(dir, "jobs", j.ID+".json.gz")
		// Make job file unreadable
		if err := os.Chmod(jobPath, 0000); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = os.Chmod(jobPath, 0644) // restore for cleanup
		}()

		_, err := Load(dir)
		if err == nil {
			t.Error("expected error for unreadable job file")
		}
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("expected permission error, got: %v", err)
		}
		// Verify it was NOT quarantined
		if _, err := os.Stat(jobPath + ".corrupt"); !os.IsNotExist(err) {
			t.Error("job file should not have been quarantined")
		}
	})
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

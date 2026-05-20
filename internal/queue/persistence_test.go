package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// ---------- LoadJob / SaveJob ----------

func TestSaveJobLoadJob_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs", "testjob.json.gz")

	j := makeMultiFileJob(t, "savejob-roundtrip", 2, 3)
	j.Files[0].Articles[0].Done = true
	j.Files[0].Articles[1].Failed = true
	j.Files[0].Complete = true
	j.RemainingBytes = 400_000
	j.FailedBytes = 100_000
	j.ServerStats = map[string]int64{"srv-a": 50_000}

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
	if !loaded.Files[0].Articles[0].Done {
		t.Error("article 0,0 should be Done")
	}
	if !loaded.Files[0].Articles[1].Failed {
		t.Error("article 0,1 should be Failed")
	}
	if !loaded.Files[0].Complete {
		t.Error("file 0 should be Complete")
	}
	if loaded.Files[1].Complete {
		t.Error("file 1 should NOT be Complete")
	}
	if loaded.RemainingBytes != 400_000 {
		t.Errorf("RemainingBytes = %d, want 400000", loaded.RemainingBytes)
	}
	if loaded.FailedBytes != 100_000 {
		t.Errorf("FailedBytes = %d, want 100000", loaded.FailedBytes)
	}
	if loaded.ServerStats["srv-a"] != 50_000 {
		t.Errorf("ServerStats[srv-a] = %d, want 50000", loaded.ServerStats["srv-a"])
	}
	// Emitted flag must NOT survive serialization.
	if loaded.Files[0].Articles[0].Emitted {
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
	j := makeJob(t, "corrupt", constants.NormalPriority)
	_ = q.Add(j)
	_ = q.Save(dir)

	// Corrupt the job file.
	jobPath := filepath.Join(dir, "jobs", j.ID+".json.gz")
	if err := os.WriteFile(jobPath, []byte("corrupt data"), 0o644); err != nil {
		t.Fatalf("corrupt job file: %v", err)
	}

	_, err := Load(dir)
	if err == nil {
		t.Error("expected error for corrupt job file")
	}
	if !strings.Contains(err.Error(), "load job") {
		t.Errorf("error should mention 'load job': %v", err)
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

// ---------- SnapshotJobByName ----------

func TestSnapshotJobByName(t *testing.T) {
	t.Parallel()
	q := New()
	j := makeJob(t, "my-download", constants.NormalPriority)
	_ = q.Add(j)

	t.Run("found", func(t *testing.T) {
		snap := q.SnapshotJobByName("my-download")
		if snap == nil {
			t.Fatal("expected non-nil snapshot")
		}
		if snap.ID != j.ID {
			t.Errorf("ID = %q, want %q", snap.ID, j.ID)
		}
		// Verify it's a deep copy.
		snap.Name = "mutated"
		original, _ := q.Get(j.ID)
		if original.Name == "mutated" {
			t.Error("snapshot mutation should not affect queue")
		}
	})

	t.Run("not found", func(t *testing.T) {
		snap := q.SnapshotJobByName("nonexistent")
		if snap != nil {
			t.Errorf("expected nil, got %+v", snap)
		}
	})

	t.Run("empty name", func(t *testing.T) {
		snap := q.SnapshotJobByName("")
		if snap != nil {
			t.Errorf("expected nil for empty name, got %+v", snap)
		}
	})
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
	arts0 := j.Files[0].Articles
	_ = j.Files[1].Articles // File 1 articles are all left pristine (!Done, !Emitted)

	if err := q.MarkArticlesDone(id, []string{arts0[0].ID}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if _, err := q.MarkArticlesFailed(id, []string{arts0[1].ID}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}
	if err := q.MarkArticleEmitted(id, arts0[2].ID); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}

	// After load, Emitted flags are always cleared (json:"-") so arts0[2]
	// becomes pending again — it was in-flight and needs to be re-dispatched.
	// Compute expected post-load counters from first principles:
	//   File 0: art[0] Done success, art[1] Done failed, art[2] becomes pending
	//           → Pending=1, BytesDownloaded=art[0].Bytes
	//   File 1: all 3 articles untouched → Pending=3, BytesDownloaded=0
	//   PendingArticles = 1 + 3 = 4
	artBytes := int64(j.Files[0].Articles[0].Bytes) // all articles same size
	wantPendingFile0 := 1                           // arts0[2] emitted→cleared→pending
	wantPendingFile1 := 3                           // all pristine
	wantPendingArticles := 4                        // 1 + 3
	wantBytesDownloaded0 := artBytes                // only arts0[0] (successful Done)

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

	// Emitted must always be cleared on load — ClearAllEmitted is called by
	// app.Start and recomputePending resets the in-memory bit.
	for fi := range got.Files {
		for ai := range got.Files[fi].Articles {
			if got.Files[fi].Articles[ai].Emitted {
				t.Errorf("Files[%d].Articles[%d].Emitted survived load (should be cleared)", fi, ai)
			}
		}
	}

	// Transient pending counters must equal the pre-save values.
	// (The emitted article is now treated as pending since Emitted was cleared.)
	if got.Files[0].Pending != wantPendingFile0 {
		t.Errorf("Files[0].Pending: got %d, want %d", got.Files[0].Pending, wantPendingFile0)
	}
	if got.Files[1].Pending != wantPendingFile1 {
		t.Errorf("Files[1].Pending: got %d, want %d", got.Files[1].Pending, wantPendingFile1)
	}
	if got.PendingArticles != wantPendingArticles {
		t.Errorf("PendingArticles: got %d, want %d", got.PendingArticles, wantPendingArticles)
	}

	// BytesDownloaded: only successful Done articles (not failed ones).
	if got.Files[0].BytesDownloaded != wantBytesDownloaded0 {
		t.Errorf("Files[0].BytesDownloaded: got %d, want %d", got.Files[0].BytesDownloaded, wantBytesDownloaded0)
	}
	if got.Files[1].BytesDownloaded != 0 {
		t.Errorf("Files[1].BytesDownloaded: got %d, want 0 (no articles done)", got.Files[1].BytesDownloaded)
	}

	// FileIdx back-pointers: every article must point to its correct file.
	// (FileIdx is json:"-", rebuilt by recomputePending via buildArtIndex)
	for fi := range got.Files {
		for ai := range got.Files[fi].Articles {
			art := &got.Files[fi].Articles[ai]
			if art.FileIdx != fi {
				t.Errorf("Files[%d].Articles[%d].FileIdx = %d, want %d",
					fi, ai, art.FileIdx, fi)
			}
		}
	}
}

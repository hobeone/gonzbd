package queue

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The contract: every job in the queue has progress, whatever its status
// and whether or not its manifest is resident. This is what makes every
// reporting path total.
func TestProgressResidentAcrossEvictionAndRestart(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	active := makeMultiFileJob(t, "active", 1, 4)
	if err := q.Add(active); err != nil {
		t.Fatalf("Add active: %v", err)
	}
	queued := makeMultiFileJob(t, "queued", 1, 4)
	if err := q.Add(queued); err != nil {
		t.Fatalf("Add queued: %v", err)
	}

	// queued is beyond maxActive, so its manifest is evicted. Its progress
	// must not be.
	q.mu.RLock()
	qj := q.byID[queued.ID]
	manifestEvicted := qj.manifest == nil
	progressPresent := qj.progress != nil
	q.mu.RUnlock()

	if !manifestEvicted {
		t.Fatal("fixture guard: queued job is still resident, eviction is not being exercised")
	}
	if !progressPresent {
		t.Error("progress was dropped along with the manifest")
	}

	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	q2, err := Load(dir, WithStore(store), WithMaxActiveJobs(1))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	q2.mu.RLock()
	defer q2.mu.RUnlock()
	for id, j := range q2.byID {
		if j.progress == nil {
			t.Errorf("job %s came back from restart with nil progress", id)
			continue
		}
		if got := j.progress.done.Len(); got != 4 {
			t.Errorf("job %s: progress sized to %d articles, want 4 — it was "+
				"allocated without the real article count", id, got)
		}
	}
}

// TestArticleCountsAreLegacy exercises all three shapes ArticleCountsByJob
// can return: a genuinely empty job (no files, not legacy — there is no
// manifest to fall back to), a legacy row predating Task 4's article_count
// column (every count zero), and a normal populated row.
func TestArticleCountsAreLegacy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		counts []int
		want   bool
	}{
		{"empty slice is not legacy", nil, false},
		{"all zero is legacy", []int{0, 0, 0}, true},
		{"any non-zero is not legacy", []int{4, 0, 2}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := articleCountsAreLegacy(tc.counts); got != tc.want {
				t.Errorf("articleCountsAreLegacy(%v) = %v, want %v", tc.counts, got, tc.want)
			}
		})
	}
}

// TestLoad_LegacyArticleCountFallback pins Step 4's fallback: a job_files
// row whose article_count is 0 for every file (the shape every row had
// before Task 4's migration backfilled the column to 0, not the real count)
// must be sized from the job's manifest on restart, not left under-sized at
// zero articles.
func TestLoad_LegacyArticleCountFallback(t *testing.T) {
	store, dir, db := setupResidencyTestStoreWithDB(t)

	// maxActive=1 with two jobs: the first occupies the only active slot, so
	// the second stays Queued/non-resident — WithMaxActiveJobs(0) would be a
	// no-op (0 is not > 0) and leave the default capacity of 4 in effect,
	// which would promote a lone job to resident and make store.Get already
	// populate its progress, skipping the fallback this test targets.
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))
	active := makeMultiFileJob(t, "legacy-active", 1, 1)
	if err := q.Add(active); err != nil {
		t.Fatalf("Add active: %v", err)
	}
	job := makeMultiFileJob(t, "legacy-row", 1, 4)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Simulate a row that predates the article_count column: the migration
	// backfills existing rows to 0, not the true per-file article count.
	if _, err := db.ExecContext(t.Context(), "UPDATE job_files SET article_count = 0 WHERE job_id = ?", job.ID); err != nil {
		t.Fatalf("zero article_count: %v", err)
	}

	q2, err := Load(dir, WithStore(store), WithMaxActiveJobs(1))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	q2.mu.RLock()
	got, ok := q2.byID[job.ID]
	q2.mu.RUnlock()
	if !ok {
		t.Fatalf("job %s missing after reload", job.ID)
	}
	if got.progress == nil {
		t.Fatal("progress is nil after the legacy fallback")
	}
	if n := got.progress.done.Len(); n != 4 {
		t.Errorf("progress sized to %d articles, want 4 — the legacy fallback "+
			"should have read the manifest instead of trusting the zero count", n)
	}
}

// TestLoad_LegacyArticleCountFallbackHealsOnce pins finding 3 from the final
// whole-branch review: the legacy fallback above must write the recovered
// counts back to job_files, so it runs once per job rather than on every
// boot.
//
// Proof strategy: after the first Load heals the row, corrupt (not delete)
// the on-disk manifest content — Get's admission check only os.Stats a
// resident job's manifest path (unaffected by corrupt content), but the
// legacy fallback's readGzJSON would fail loudly and log a warning if it ran
// again. So on a second Load: no error, progress sized correctly from
// article_count alone, and — the key assertion — no "could not read
// manifest" warning, proving the fallback did not re-read the manifest at
// all.
func TestLoad_LegacyArticleCountFallbackHealsOnce(t *testing.T) {
	store, dir, db := setupResidencyTestStoreWithDB(t)

	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))
	active := makeMultiFileJob(t, "heals-once-active", 1, 1)
	if err := q.Add(active); err != nil {
		t.Fatalf("Add active: %v", err)
	}
	job := makeMultiFileJob(t, "heals-once-row", 1, 4)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := db.ExecContext(t.Context(), "UPDATE job_files SET article_count = 0 WHERE job_id = ?", job.ID); err != nil {
		t.Fatalf("zero article_count: %v", err)
	}

	// First Load: exercises the fallback and (per the fix under test) writes
	// the recovered counts back to job_files.
	if _, err := Load(dir, WithStore(store), WithMaxActiveJobs(1)); err != nil {
		t.Fatalf("first Load: %v", err)
	}

	// Confirm the row actually healed: article_count must no longer be the
	// legacy all-zero shape.
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT article_count FROM job_files WHERE job_id = ?", job.ID).Scan(&count); err != nil {
		t.Fatalf("query article_count after first Load: %v", err)
	}
	if count != 4 {
		t.Fatalf("article_count after first Load = %d, want 4 — the fallback did not "+
			"persist the recovered count back to job_files", count)
	}

	// Corrupt (not delete) the manifest the fallback used the first time.
	// The file must still exist for Get's unconditional admission check, but
	// any attempt to actually parse it as gzip/JSON must fail.
	manifestPath := filepath.Join(dir, "manifests", job.ID+".json.gz")
	if err := os.WriteFile(manifestPath, []byte("NOT_A_VALID_GZIP_FILE"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("corrupt manifest fixture: %v", err)
	}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	q3, err := Load(dir, WithStore(store), WithMaxActiveJobs(1))
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	q3.mu.RLock()
	got3, ok := q3.byID[job.ID]
	q3.mu.RUnlock()
	if !ok {
		t.Fatalf("job %s missing after second reload", job.ID)
	}
	if got3.progress == nil {
		t.Fatal("progress is nil on second Load")
	}
	if n := got3.progress.done.Len(); n != 4 {
		t.Errorf("second Load: progress sized to %d articles, want 4 — the healed "+
			"job_files row should have made this correct without ever parsing "+
			"the (now corrupt) manifest", n)
	}
	if logged := logBuf.String(); strings.Contains(logged, "could not read manifest") {
		t.Errorf("second Load logged a manifest-read failure, meaning it re-ran the legacy "+
			"fallback instead of trusting the healed article_count: %q", logged)
	}
}

// TestLoad_LegacyArticleCountFallbackCorruptManifestDegrades pins the CRITICAL
// fix: a corrupt manifest on a legacy-row job must not fail Load. Before this
// fix, the fallback's readGzJSON error propagated straight out of Load, and
// since Application.New calls queue.Load at startup, a single damaged
// manifest — the exact condition every row is in during the upgrade window
// that first exercises this path — took down the daemon's boot. Recovery
// paths must degrade, not die (docs/queue-lifecycle.md): the pre-existing
// behavior of surfacing a corrupt manifest as a claim failure later, at
// PromoteNext, must be preserved instead.
func TestLoad_LegacyArticleCountFallbackCorruptManifestDegrades(t *testing.T) {
	store, dir, db := setupResidencyTestStoreWithDB(t)

	// Same maxActive=1/two-job shape as TestLoad_LegacyArticleCountFallback,
	// so the target job stays Queued/non-resident and actually exercises the
	// fallback rather than being sized by store.Get's normal resident path.
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))
	active := makeMultiFileJob(t, "legacy-corrupt-active", 1, 1)
	if err := q.Add(active); err != nil {
		t.Fatalf("Add active: %v", err)
	}
	job := makeMultiFileJob(t, "legacy-corrupt-row", 1, 4)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := db.ExecContext(t.Context(), "UPDATE job_files SET article_count = 0 WHERE job_id = ?", job.ID); err != nil {
		t.Fatalf("zero article_count: %v", err)
	}

	manifestPath := filepath.Join(dir, "manifests", job.ID+".json.gz")
	if err := os.WriteFile(manifestPath, []byte("NOT_A_VALID_GZIP_FILE"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("corrupt manifest fixture: %v", err)
	}

	q2, err := Load(dir, WithStore(store), WithMaxActiveJobs(1))
	if err != nil {
		t.Fatalf("Load: want no error from a corrupt manifest on a legacy-row job, got: %v", err)
	}

	q2.mu.RLock()
	got, ok := q2.byID[job.ID]
	q2.mu.RUnlock()
	if !ok {
		t.Fatalf("job %s missing after reload", job.ID)
	}
	if got.progress == nil {
		t.Error("progress is nil — a degraded fallback must still leave progress non-nil")
	}
}

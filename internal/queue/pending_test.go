package queue

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

// verifyPending checks that Pending, PendingArticles, and
// BytesDownloaded match the ground truth computed from article flags.
// Call after every state mutation in tests to detect counter drift.
func verifyPending(t *testing.T, q *Queue, label string) {
	t.Helper()
	q.mu.RLock()
	defer q.mu.RUnlock()
	for _, job := range q.jobs {
		m, p := job.manifest, job.progress
		wantJob := 0
		for fi := range m.NumFiles() {
			wantFile := 0
			var wantDownloaded int64
			// Files not being fetched (on-demand par2) contribute zero pending
			// work by design — mirror recompute's rule in the ground truth.
			fetching := p.files[fi].Fetch == FetchAlways
			lo, hi := m.FileRange(fi)
			for i := lo; i < hi; i++ {
				if fetching && !p.done.Get(i) && !p.emitted.Get(i) {
					wantFile++
				}
				if p.done.Get(i) && !p.failed.Get(i) {
					wantDownloaded += int64(m.ArticleBytes(i))
				}
			}
			if p.files[fi].Pending != wantFile {
				t.Errorf("%s: job %s file %d: Pending=%d want %d",
					label, job.ID, fi, p.files[fi].Pending, wantFile)
			}
			if p.files[fi].BytesDownloaded != wantDownloaded {
				t.Errorf("%s: job %s file %d: BytesDownloaded=%d want %d",
					label, job.ID, fi, p.files[fi].BytesDownloaded, wantDownloaded)
			}
			wantJob += wantFile
		}
		if p.pendingArticles != wantJob {
			t.Errorf("%s: job %s: PendingArticles=%d want %d",
				label, job.ID, p.pendingArticles, wantJob)
		}
	}
}

// makeTestJob builds a Job with nFiles files, each having nArtsPerFile
// articles. Article IDs are formatted as "art-{fileIdx}-{artIdx}". Each
// file's Bytes is set to the sum of its articles' bytes: RemainingBytes
// derives from file.Bytes minus downloaded/failed article bytes (see
// derivedRemainingBytes), so a fixture whose file-level total disagrees
// with its article-level total would make that derivation silently wrong
// for every caller of this helper.
func makeTestJob(id string, nFiles, nArtsPerFile int) *Job {
	files := make([]JobFile, 0, nFiles)
	for fi := range nFiles {
		file := JobFile{Subject: "file", Bytes: int64(nArtsPerFile) * 1000}
		for ai := range nArtsPerFile {
			file.Articles = append(file.Articles, JobArticle{
				ID:    artID(fi, ai),
				Bytes: 1000,
			})
		}
		files = append(files, file)
	}
	job := &Job{
		ID:     id,
		Name:   id,
		Status: constants.StatusQueued,
	}
	job.manifest = newManifest(files)
	job.progress = newJobProgress(job.manifest)
	return job
}

func artID(fi, ai int) string {
	return "art-" + string(rune('0'+fi)) + "-" + string(rune('0'+ai))
}

func TestPendingCounter_Add(t *testing.T) {
	q := New()
	job := makeTestJob("j1", 2, 3)
	if err := q.Add(job); err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after Add")
	if job.Progress().PendingArticles() != 6 {
		t.Errorf("PendingArticles=%d want 6", job.Progress().PendingArticles())
	}
}

func TestPendingCounter_EmitAndClear(t *testing.T) {
	q := New()
	job := makeTestJob("j1", 2, 3)
	_ = q.Add(job)
	verifyPending(t, q, "after Add")

	// Emit one article
	if err := q.MarkArticleEmitted("j1", artID(0, 0)); err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after Emit")

	// Emit same article again (idempotent)
	if err := q.MarkArticleEmitted("j1", artID(0, 0)); err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after Emit idempotent")

	// Clear emitted → article returns to pending
	if err := q.ClearArticleEmitted("j1", artID(0, 0)); err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after ClearEmitted")

	// Clear again (idempotent)
	if err := q.ClearArticleEmitted("j1", artID(0, 0)); err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after ClearEmitted idempotent")
}

func TestPendingCounter_MarkDone(t *testing.T) {
	q := New()
	job := makeTestJob("j1", 2, 3)
	_ = q.Add(job)

	// Mark one article done (not emitted first)
	if err := q.MarkArticleDone("j1", artID(0, 0)); err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after MarkDone unemitted")

	// Emit then mark done (normal path)
	_ = q.MarkArticleEmitted("j1", artID(0, 1))
	verifyPending(t, q, "after Emit")
	if err := q.MarkArticleDone("j1", artID(0, 1)); err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after MarkDone emitted")

	// Mark already-done article (idempotent)
	if err := q.MarkArticleDone("j1", artID(0, 0)); err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after MarkDone idempotent")
}

func TestPendingCounter_MarkFailed(t *testing.T) {
	q := New()
	_ = q.Add(makeTestJob("j1", 1, 3))

	// Fail unemitted article
	_, err := q.MarkArticleFailed("j1", artID(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after MarkFailed unemitted")

	// Emit then fail
	_ = q.MarkArticleEmitted("j1", artID(0, 1))
	_, err = q.MarkArticleFailed("j1", artID(0, 1))
	if err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after MarkFailed emitted")
}

func TestPendingCounter_BatchDone(t *testing.T) {
	q := New()
	_ = q.Add(makeTestJob("j1", 2, 3))

	// Emit some, leave others unemitted, then batch-mark done
	_ = q.MarkArticleEmitted("j1", artID(0, 0))
	_ = q.MarkArticleEmitted("j1", artID(1, 2))

	err := q.MarkArticlesDone("j1", []string{
		artID(0, 0), // emitted
		artID(0, 1), // unemitted
		artID(1, 2), // emitted
	})
	if err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after BatchDone")
}

func TestPendingCounter_BatchFailed(t *testing.T) {
	q := New()
	_ = q.Add(makeTestJob("j1", 1, 4))

	_ = q.MarkArticleEmitted("j1", artID(0, 1))

	_, err := q.MarkArticlesFailed("j1", []string{
		artID(0, 0), // unemitted
		artID(0, 1), // emitted
	})
	if err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after BatchFailed")
}

func TestPendingCounter_ClearAllEmitted(t *testing.T) {
	q := New()
	_ = q.Add(makeTestJob("j1", 2, 3))

	// Emit several articles and mark some done
	_ = q.MarkArticleEmitted("j1", artID(0, 0))
	_ = q.MarkArticleEmitted("j1", artID(0, 1))
	_ = q.MarkArticleEmitted("j1", artID(1, 0))
	_ = q.MarkArticleDone("j1", artID(0, 0))
	_, _ = q.MarkArticleFailed("j1", artID(1, 0))
	verifyPending(t, q, "before ClearAllEmitted")

	// ClearAllEmitted resets Emitted and Failed
	q.ClearAllEmitted()
	verifyPending(t, q, "after ClearAllEmitted")
}

func TestPendingCounter_ForEachSkipsCompletedFiles(t *testing.T) {
	q := New()
	_ = q.Add(makeTestJob("j1", 2, 2))

	// Mark all articles in file 0 as done
	_ = q.MarkArticleDone("j1", artID(0, 0))
	_ = q.MarkArticleDone("j1", artID(0, 1))
	verifyPending(t, q, "after completing file 0")

	// ForEach should only yield articles from file 1
	var seen []string
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		seen = append(seen, a.MessageID)
		return true
	})
	if len(seen) != 2 {
		t.Errorf("expected 2 articles from file 1, got %d: %v", len(seen), seen)
	}
	for _, id := range seen {
		if id == artID(0, 0) || id == artID(0, 1) {
			t.Errorf("ForEach yielded completed file 0 article: %s", id)
		}
	}
}

func TestPendingCounter_ForEachSkipsCompletedJobs(t *testing.T) {
	q := New()
	_ = q.Add(makeTestJob("j1", 1, 2))
	_ = q.Add(makeTestJob("j2", 1, 1))

	// Complete all articles in j1
	_ = q.MarkArticleDone("j1", artID(0, 0))
	_ = q.MarkArticleDone("j1", artID(0, 1))
	verifyPending(t, q, "after completing j1")

	var seen []string
	q.ForEachUnfinishedArticle(func(a UnfinishedArticle) bool {
		seen = append(seen, a.JobID+"/"+a.MessageID)
		return true
	})
	if len(seen) != 1 {
		t.Errorf("expected 1 article from j2, got %d: %v", len(seen), seen)
	}
}

func TestPendingCounter_PersistenceRoundTrip(t *testing.T) {
	// Create queue, add job, mark some articles, save
	q := New()
	_ = q.Add(makeTestJob("j1", 2, 3))
	_ = q.MarkArticleDone("j1", artID(0, 0))
	_ = q.MarkArticleDone("j1", artID(0, 1))
	_ = q.MarkArticleDone("j1", artID(0, 2))
	verifyPending(t, q, "before save")

	// Round-trip through the store — the only path that writes a live job
	// down since #266 removed the whole-queue engine and #298 the per-job
	// document. The property under test, pending counters rebuilt from the
	// persisted done flags, is unchanged by either.
	job := storeRoundTrip(t, q.jobs[0])
	if job.Progress().PendingArticles() != 3 {
		t.Errorf("PendingArticles=%d want 3", job.Progress().PendingArticles())
	}
	if job.Progress().FilePending(0) != 0 {
		t.Errorf("file 0 Pending=%d want 0", job.Progress().FilePending(0))
	}
	if job.Progress().FilePending(1) != 3 {
		t.Errorf("file 1 Pending=%d want 3", job.Progress().FilePending(1))
	}
}

func TestBytesDownloaded_TrackedByMarkArticlesDone(t *testing.T) {
	q := New()
	job := makeTestJob("j1", 2, 3) // 6 articles × 1000 bytes = 6000 bytes total
	_ = q.Add(job)

	// Complete two articles in file 0.
	if err := q.MarkArticlesDone("j1", []string{artID(0, 0), artID(0, 1)}); err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after partial done")

	if got := job.Progress().FileBytesDownloaded(0); got != 2000 {
		t.Errorf("file 0 BytesDownloaded = %d; want 2000 (2×1000)", got)
	}
	if got := job.Progress().FileBytesDownloaded(1); got != 0 {
		t.Errorf("file 1 BytesDownloaded = %d; want 0 (no completions)", got)
	}

	// Failed articles do NOT add to BytesDownloaded — they failed,
	// they weren't actually written.
	if _, err := q.MarkArticleFailed("j1", artID(1, 0)); err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q, "after one failure")
	if got := job.Progress().FileBytesDownloaded(1); got != 0 {
		t.Errorf("file 1 BytesDownloaded = %d; want 0 (failed article excluded)", got)
	}

	// Re-marking an already-done article is a no-op (idempotent).
	if err := q.MarkArticlesDone("j1", []string{artID(0, 0)}); err != nil {
		t.Fatal(err)
	}
	if got := job.Progress().FileBytesDownloaded(0); got != 2000 {
		t.Errorf("file 0 BytesDownloaded = %d; want 2000 (idempotent)", got)
	}
}

func TestBytesDownloaded_RecomputeAfterLoad(t *testing.T) {
	q := New()
	job := makeTestJob("j1", 2, 2)
	_ = q.Add(job)
	_ = q.MarkArticlesDone("j1", []string{artID(0, 0), artID(1, 0)})

	// Through the store, for the same reason as above. The property —
	// per-file BytesDownloaded rebuilt from the persisted done flags — is
	// unchanged.
	loaded := storeRoundTrip(t, job)
	if got := loaded.Progress().FileBytesDownloaded(0); got != 1000 {
		t.Errorf("file 0 BytesDownloaded = %d; want 1000 (recomputed on load)", got)
	}
	if got := loaded.Progress().FileBytesDownloaded(1); got != 1000 {
		t.Errorf("file 1 BytesDownloaded = %d; want 1000 (recomputed on load)", got)
	}
}

// TestCounterConsistencyUnderRandomMutations is a property-style test that
// applies a randomized sequence of MarkArticle* operations to a 100-article
// job and calls verifyPending after each step. A fixed seed makes failures
// deterministic and reproducible. This guards against the counter-drift
// documented in CLAUDE.md where incremental Pending/BytesDownloaded
// updates diverge from ground truth over a sequence of mutations.
func TestCounterConsistencyUnderRandomMutations(t *testing.T) {
	const seed1, seed2 = 0xdeadbeef, 0xcafef00d
	rng := rand.New(rand.NewPCG(seed1, seed2))

	const nFiles, nArts = 4, 5
	q := New()
	job := makeTestJob("prop", nFiles, nArts)
	_ = q.Add(job)

	// Collect all article IDs for random selection.
	ids := make([]string, 0, nFiles*nArts)
	for fi := range nFiles {
		for ai := range nArts {
			ids = append(ids, artID(fi, ai))
		}
	}
	pick := func() string { return ids[rng.IntN(len(ids))] }

	const rounds = 500
	for i := range rounds {
		label := fmt.Sprintf("round %d", i)
		op := rng.IntN(4)
		id := pick()
		switch op {
		case 0: // MarkArticlesDone
			_ = q.MarkArticlesDone("prop", []string{id})
		case 1: // MarkArticlesFailed
			_, _ = q.MarkArticlesFailed("prop", []string{id})
		case 2: // MarkArticleEmitted (no-op if already Done)
			_ = q.MarkArticleEmitted("prop", id)
		case 3: // ClearArticleEmitted
			_ = q.ClearArticleEmitted("prop", id)
		}
		verifyPending(t, q, label)
	}
}

package queue

import (
	"testing"
)

// verifyPending checks that Pending and PendingArticles match the
// ground truth computed from article flags. Call after every state
// mutation in tests to detect counter drift.
func verifyPending(t *testing.T, q *Queue, label string) {
	t.Helper()
	q.mu.RLock()
	defer q.mu.RUnlock()
	for _, job := range q.jobs {
		wantJob := 0
		for fi := range job.Files {
			wantFile := 0
			for ai := range job.Files[fi].Articles {
				art := &job.Files[fi].Articles[ai]
				if !art.Done && !art.Emitted {
					wantFile++
				}
			}
			if job.Files[fi].Pending != wantFile {
				t.Errorf("%s: job %s file %d: Pending=%d want %d",
					label, job.ID, fi, job.Files[fi].Pending, wantFile)
			}
			wantJob += wantFile
		}
		if job.PendingArticles != wantJob {
			t.Errorf("%s: job %s: PendingArticles=%d want %d",
				label, job.ID, job.PendingArticles, wantJob)
		}
	}
}

// makeTestJob builds a Job with nFiles files, each having nArtsPerFile
// articles. Article IDs are formatted as "art-{fileIdx}-{artIdx}".
func makeTestJob(id string, nFiles, nArtsPerFile int) *Job {
	job := &Job{
		ID:   id,
		Name: id,
	}
	for fi := 0; fi < nFiles; fi++ {
		file := JobFile{Subject: "file"}
		for ai := 0; ai < nArtsPerFile; ai++ {
			file.Articles = append(file.Articles, JobArticle{
				ID:    artID(fi, ai),
				Bytes: 1000,
			})
			job.TotalBytes += 1000
			job.RemainingBytes += 1000
		}
		job.Files = append(job.Files, file)
	}
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
	if job.PendingArticles != 6 {
		t.Errorf("PendingArticles=%d want 6", job.PendingArticles)
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
	dir := t.TempDir()

	// Create queue, add job, mark some articles, save
	q := New()
	_ = q.Add(makeTestJob("j1", 2, 3))
	_ = q.MarkArticleDone("j1", artID(0, 0))
	_ = q.MarkArticleDone("j1", artID(0, 1))
	_ = q.MarkArticleDone("j1", artID(0, 2))
	verifyPending(t, q, "before save")

	if err := q.Save(dir); err != nil {
		t.Fatal(err)
	}

	// Load from disk — pending counters should be recomputed
	q2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	verifyPending(t, q2, "after Load")

	// PendingArticles should be 3 (file 1 has all 3 pending)
	job := q2.jobs[0]
	if job.PendingArticles != 3 {
		t.Errorf("PendingArticles=%d want 3", job.PendingArticles)
	}
	if job.Files[0].Pending != 0 {
		t.Errorf("file 0 Pending=%d want 0", job.Files[0].Pending)
	}
	if job.Files[1].Pending != 3 {
		t.Errorf("file 1 Pending=%d want 3", job.Files[1].Pending)
	}
}

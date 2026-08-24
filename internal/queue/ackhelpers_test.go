package queue

import (
	"context"
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// The helpers here replace the queue's deleted ack verbs (MarkArticlesDone,
// MarkArticlesFailed, and their ByIdx forms) in fixtures that used them to
// advance a job rather than to test them.
//
// Success and failure take deliberately different routes, because the design
// makes them different kinds of claim:
//
//   - A success asserts bytes are on stable storage, so the only production
//     path is a DurableProof minted by a barrier, or SeedFromRuns replaying
//     the runs a barrier recorded. ackDone uses the latter: it is exported, it
//     needs no proof, and it is the real resume path rather than a test-only
//     shortcut. Deliberately NOT an exported test constructor for
//     DurableProof — that would move R9's compiler enforcement into a CI grep.
//   - A failure asserts nothing about disk (R10), so AckPermanentFailure is
//     callable directly and the helper is a thin index-resolving wrapper.
//
// Both take message IDs where the deleted API did, so the migration of a call
// site is a rename rather than a rewrite of its fixture.

// artIdxFor resolves a message ID to its global article index.
func artIdxFor(t *testing.T, q *Queue, jobID, msgID string) int32 {
	t.Helper()
	q.mu.RLock()
	defer q.mu.RUnlock()
	job, ok := q.byID[jobID]
	if !ok {
		t.Fatalf("artIdxFor: job %s not in queue", jobID)
	}
	if job.manifest == nil {
		t.Fatalf("artIdxFor: job %s is not resident, so message IDs cannot be resolved", jobID)
	}
	// A linear scan, matching the copies in internal/api, internal/app,
	// internal/downloader and internal/postproc. There is no by-ID lookup to
	// call any more, and building one here would reintroduce the map F2
	// deleted. Fixture manifests are small, and the parser rejects duplicate
	// Message-IDs (A7), so first-match is the only match.
	for i := range job.manifest.NumArticles() {
		if job.manifest.ArticleID(i) == msgID {
			return int32(i) //nolint:gosec // G115: article counts are far below int32
		}
	}
	t.Fatalf("artIdxFor: job %s has no article %s", jobID, msgID)
	return 0
}

// ackDoneIdx marks articles durable the way a barrier does: it RECORDS the
// runs first, then installs them in the live work set.
//
// Both halves are needed and the recording half is the one that is easy to
// forget. Per-article resolution is no longer a column the queue re-serialises
// on Update — it is derived from durable_runs and failed_articles — so a
// fixture that only touched memory would vanish across every reload, which is
// precisely what production does when no barrier has run.
//
// The in-memory install uses one single-article Run per named article rather
// than merged spans: a run's article range is all SeedFromRuns reads, and
// markDone is idempotent. The real merging is RunStore.Commit's job and is
// pinned in internal/durability.
func ackDoneIdx(t *testing.T, q *Queue, jobID string, artIdxs ...int32) {
	t.Helper()
	if len(artIdxs) == 0 {
		return
	}

	q.mu.RLock()
	job, ok := q.byID[jobID]
	if !ok {
		q.mu.RUnlock()
		t.Fatalf("ackDoneIdx: job %s not in queue", jobID)
	}
	m := job.manifest
	if m == nil {
		q.mu.RUnlock()
		t.Fatalf("ackDoneIdx: job %s is not resident", jobID)
	}
	runs := make([]durability.Run, 0, len(artIdxs))
	for _, a := range artIdxs {
		i := int(a)
		if i < 0 || i >= m.NumArticles() {
			q.mu.RUnlock()
			t.Fatalf("ackDoneIdx: article %d out of range for job %s", i, jobID)
		}
		runs = append(runs, durability.Run{
			FileIdx:     int32(m.fileIndexForArticle(i)), //nolint:gosec // G115: file counts are far below int32
			FirstArtIdx: a,
			LastArtIdx:  a,
			Length:      int64(m.ArticleBytes(i)),
		})
	}
	q.mu.RUnlock()

	if err := q.SeedFromRuns(jobID, runs); err != nil {
		t.Fatalf("ackDoneIdx: SeedFromRuns: %v", err)
	}
	// Recorded AFTER the install, from the job's resulting progress, so the
	// stored record and the live work set cannot disagree about which
	// articles this fixture claims.
	if s, ok := q.store.(*SQLiteStore); ok {
		q.mu.RLock()
		live := q.byID[jobID]
		q.mu.RUnlock()
		commitBarrierRuns(t, s.db, live)
	}
}

// discardRuns drops one file's durable runs, which is the ONLY mutation
// durability.Resumer performs: a file missing or shorter than its runs claim
// has disproved them, so they go and the file is fetched again.
//
// A fixture that wants to model a resume disproving a file has to do this, not
// merely withhold the runs from ReplaceFromRuns. The queue re-derives every
// article's state from the record on each re-hydration, so a withheld-but-
// stored run comes straight back.
func discardRuns(t *testing.T, q *Queue, jobID string, fileIdx int32) {
	t.Helper()
	s, ok := q.store.(*SQLiteStore)
	if !ok {
		t.Fatalf("discardRuns: queue for %s has no SQLite store", jobID)
	}
	if err := durability.NewSQLiteRunStore(s.db).DeleteFile(context.Background(), jobID, fileIdx); err != nil {
		t.Fatalf("discardRuns: %v", err)
	}
}

// ackDone is ackDoneIdx keyed by message ID.
func ackDone(t *testing.T, q *Queue, jobID string, msgIDs ...string) {
	t.Helper()
	idxs := make([]int32, 0, len(msgIDs))
	for _, id := range msgIDs {
		idxs = append(idxs, artIdxFor(t, q, jobID, id))
	}
	ackDoneIdx(t, q, jobID, idxs...)
}

// ackFailed is AckPermanentFailure keyed by message ID.
func ackFailed(t *testing.T, q *Queue, jobID string, msgIDs ...string) {
	t.Helper()
	idxs := make([]int32, 0, len(msgIDs))
	for _, id := range msgIDs {
		idxs = append(idxs, artIdxFor(t, q, jobID, id))
	}
	if err := q.AckPermanentFailure(jobID, idxs); err != nil {
		t.Fatalf("ackFailed: %v", err)
	}
}

package queue

import (
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
//     path is a DurableProof minted by a barrier, or SeedFromExtents replaying
//     a barrier's committed Class B cache. ackDone uses the latter: it is
//     exported, it needs no proof, and it is the real resume path rather than
//     a test-only shortcut. Deliberately NOT an exported test constructor for
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
	i, ok := job.manifest.articleIndexByID(msgID)
	if !ok {
		t.Fatalf("artIdxFor: job %s has no article %s", jobID, msgID)
	}
	return int32(i) //nolint:gosec // G115: article counts are far below int32
}

// ackDoneIdx marks articles durable through SeedFromExtents, the path a
// resumed job uses to adopt a barrier's committed durable bitmap.
//
// It rebuilds each affected file's extent from the job's CURRENT durable bits
// plus the newly named articles, because SeedFromExtents installs a bitmap
// rather than merging one: seeding only the new articles would still be
// correct (markDone is idempotent and never clears a bit), but rebuilding
// keeps the extent a truthful picture of the file, which is what a barrier
// would have committed.
func ackDoneIdx(t *testing.T, q *Queue, jobID string, artIdxs ...int32) {
	t.Helper()
	if len(artIdxs) == 0 {
		return
	}
	add := make(map[int]bool, len(artIdxs))
	for _, a := range artIdxs {
		add[int(a)] = true
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
	touched := make(map[int]bool)
	for i := range add {
		if i < 0 || i >= m.NumArticles() {
			q.mu.RUnlock()
			t.Fatalf("ackDoneIdx: article %d out of range for job %s", i, jobID)
		}
		touched[m.fileIndexForArticle(i)] = true
	}
	var exts []durability.FileExtent
	for fi := range touched {
		lo, hi := m.FileRange(fi)
		bm := durability.NewBitmap(hi - lo)
		for i := lo; i < hi; i++ {
			if add[i] || (job.progress.ArticleDone(i) && !job.progress.ArticleFailed(i)) {
				bm.Set(i - lo)
			}
		}
		exts = append(exts, durability.FileExtent{
			FileIdx: int32(fi), //nolint:gosec // G115: file counts are far below int32
			Durable: bm,
		})
	}
	q.mu.RUnlock()

	if err := q.SeedFromExtents(jobID, exts); err != nil {
		t.Fatalf("ackDoneIdx: SeedFromExtents: %v", err)
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

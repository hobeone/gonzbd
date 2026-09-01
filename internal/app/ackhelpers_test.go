package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/queue"
)

// The helpers here replace the queue's deleted ack verbs (MarkArticleDone,
// MarkArticleFailed, MarkArticlesDone, MarkArticlesFailed, and their ByIdx
// forms) in fixtures that used them to advance a job rather than to test the
// ack verbs themselves.
//
// Success and failure take deliberately different routes, mirroring
// internal/queue/ackhelpers_test.go (the worked example for this migration):
//
//   - A success asserts bytes are on stable storage, so the only production
//     path is a DurableProof minted by a barrier, or SeedFromRuns replaying
//     the runs a barrier recorded. ackDone uses the latter: it is exported, it
//     needs no proof, and it is the real resume path rather than a test-only
//     shortcut.
//   - A failure asserts nothing about disk, so AckPermanentFailure is callable
//     directly and the helper is a thin index-resolving wrapper.
//
// Unlike internal/queue's helpers, these can only reach job.manifest and
// job.progress through exported accessors (Job.Manifest, Job.Progress,
// JobProgress.ArticleDone/ArticleFailed), since job's fields are unexported
// outside package queue.

// artIdxFor resolves a message ID to its global article index.
func artIdxFor(t *testing.T, q *queue.Queue, jobID, msgID string) int32 {
	t.Helper()
	return artIdxsFor(t, q, jobID, msgID)[0]
}

// artIdxsFor resolves several message IDs against ONE snapshot.
//
// SnapshotJob deep-copies JobProgress, including its bitsets, so calling it
// per message ID made a fixture that acks N articles clone the job N times to
// read N immutable strings out of a Manifest that never changes between the
// calls. One snapshot answers the whole batch.
//
//dupcomment:ok per-package copies of one test helper; the automatic same-basename exemption misses them because the app_test copy is named ackhelpers_external_test.go
func artIdxsFor(t *testing.T, q *queue.Queue, jobID string, msgIDs ...string) []int32 {
	t.Helper()
	job := q.SnapshotJob(jobID)
	if job == nil {
		t.Fatalf("artIdxsFor: job %s not in queue", jobID)
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("artIdxsFor: job %s manifest: %v", jobID, err)
	}

	byID := make(map[string]int32, m.NumArticles())
	for i := range m.NumArticles() {
		byID[m.ArticleID(i)] = int32(i) //nolint:gosec // G115: article counts are far below int32
	}

	out := make([]int32, 0, len(msgIDs))
	for _, id := range msgIDs {
		idx, ok := byID[id]
		if !ok {
			t.Fatalf("artIdxsFor: job %s has no article %s", jobID, id)
		}
		out = append(out, idx)
	}
	return out
}

// ackDoneIdx marks articles durable through SeedFromRuns, the path a resumed
// job uses to adopt the runs a barrier recorded.
//
// One single-article Run per named article rather than merged spans: a run's
// [FirstArtIdx, LastArtIdx] range is all SeedFromRuns reads, and markDone is
// idempotent. The real merging is RunStore.Commit's job and is pinned in
// internal/durability.
func ackDoneIdx(t *testing.T, q *queue.Queue, jobID string, artIdxs ...int32) {
	t.Helper()
	if len(artIdxs) == 0 {
		return
	}

	job := q.SnapshotJob(jobID)
	if job == nil {
		t.Fatalf("ackDoneIdx: job %s not in queue", jobID)
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("ackDoneIdx: job %s manifest: %v", jobID, err)
	}

	runs := make([]durability.Run, 0, len(artIdxs))
	for _, a := range artIdxs {
		i := int(a)
		if i < 0 || i >= m.NumArticles() {
			t.Fatalf("ackDoneIdx: article %d out of range for job %s", i, jobID)
		}
		fi, ok := fileIdxForArticle(m, i)
		if !ok {
			t.Fatalf("ackDoneIdx: article %d not owned by any file in job %s", i, jobID)
		}
		runs = append(runs, durability.Run{
			FileIdx:     int32(fi), //nolint:gosec // G115: file counts are far below int32
			FirstArtIdx: a,
			LastArtIdx:  a,
			Length:      int64(m.ArticleBytes(i)),
		})
	}

	if err := q.SeedFromRuns(jobID, runs); err != nil {
		t.Fatalf("ackDoneIdx: SeedFromRuns: %v", err)
	}
}

// ackDone is ackDoneIdx keyed by message ID.
func ackDone(t *testing.T, q *queue.Queue, jobID string, msgIDs ...string) {
	t.Helper()
	idxs := artIdxsFor(t, q, jobID, msgIDs...)
	ackDoneIdx(t, q, jobID, idxs...)
}

// ackFailed is AckPermanentFailure keyed by message ID.
func ackFailed(t *testing.T, q *queue.Queue, jobID string, msgIDs ...string) {
	t.Helper()
	idxs := artIdxsFor(t, q, jobID, msgIDs...)
	if err := q.AckPermanentFailure(jobID, idxs); err != nil {
		t.Fatalf("ackFailed: %v", err)
	}
}

// dupcomment:ok four packages each need their own copy of this helper;
// Manifest.fileIndexForArticle is unexported outside package queue.
//
// fileIdxForArticle returns the manifest file index owning global article
// index i, using only exported Manifest accessors (Manifest.fileIndexForArticle
// is unexported outside package queue).
func fileIdxForArticle(m *queue.Manifest, i int) (int, bool) {
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		if i >= lo && i < hi {
			return fi, true
		}
	}
	return 0, false
}

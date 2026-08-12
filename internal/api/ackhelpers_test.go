package api

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/queue"
)

// The helpers here replace the queue's deleted ack verbs (MarkArticleDone,
// MarkArticleFailed, MarkArticlesDone, MarkArticlesFailed) in fixtures that
// used them to advance a job rather than to test the ack verbs themselves.
//
// Success and failure take deliberately different routes, mirroring
// internal/queue/ackhelpers_test.go (the worked example for this migration):
//
//   - A success asserts bytes are on stable storage, so the only production
//     path is a DurableProof minted by a barrier, or SeedFromExtents replaying
//     a barrier's committed Class B cache. ackDone uses the latter: it is
//     exported, it needs no proof, and it is the real resume path rather than
//     a test-only shortcut.
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
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("artIdxFor: job %s not in queue: %v", jobID, err)
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("artIdxFor: job %s manifest: %v", jobID, err)
	}
	for i := range m.NumArticles() {
		if m.ArticleID(i) == msgID {
			return int32(i) //nolint:gosec // G115: article counts are far below int32
		}
	}
	t.Fatalf("artIdxFor: job %s has no article %s", jobID, msgID)
	return 0
}

// ackDoneIdx marks articles durable through SeedFromExtents, the path a
// resumed job uses to adopt a barrier's committed durable bitmap.
//
// It rebuilds each affected file's extent from the job's CURRENT durable bits
// plus the newly named articles, because SeedFromExtents installs a bitmap
// rather than merging one.
func ackDoneIdx(t *testing.T, q *queue.Queue, jobID string, artIdxs ...int32) {
	t.Helper()
	if len(artIdxs) == 0 {
		return
	}
	add := make(map[int]bool, len(artIdxs))
	for _, a := range artIdxs {
		add[int(a)] = true
	}

	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("ackDoneIdx: job %s not in queue: %v", jobID, err)
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("ackDoneIdx: job %s manifest: %v", jobID, err)
	}
	progress := job.Progress()

	touched := make(map[int]bool)
	for i := range add {
		if i < 0 || i >= m.NumArticles() {
			t.Fatalf("ackDoneIdx: article %d out of range for job %s", i, jobID)
		}
		fi, ok := fileIdxForArticle(m, i)
		if !ok {
			t.Fatalf("ackDoneIdx: article %d not owned by any file in job %s", i, jobID)
		}
		touched[fi] = true
	}
	var exts []durability.FileExtent
	for fi := range touched {
		lo, hi := m.FileRange(fi)
		bm := durability.NewBitmap(hi - lo)
		for i := lo; i < hi; i++ {
			if add[i] || (progress.ArticleDone(i) && !progress.ArticleFailed(i)) {
				bm.Set(i - lo)
			}
		}
		exts = append(exts, durability.FileExtent{
			FileIdx: int32(fi), //nolint:gosec // G115: file counts are far below int32
			Durable: bm,
		})
	}

	if err := q.SeedFromExtents(jobID, exts); err != nil {
		t.Fatalf("ackDoneIdx: SeedFromExtents: %v", err)
	}
}

// ackDone is ackDoneIdx keyed by message ID.
func ackDone(t *testing.T, q *queue.Queue, jobID string, msgIDs ...string) {
	t.Helper()
	idxs := make([]int32, 0, len(msgIDs))
	for _, id := range msgIDs {
		idxs = append(idxs, artIdxFor(t, q, jobID, id))
	}
	ackDoneIdx(t, q, jobID, idxs...)
}

// ackFailed is AckPermanentFailure keyed by message ID.
func ackFailed(t *testing.T, q *queue.Queue, jobID string, msgIDs ...string) {
	t.Helper()
	idxs := make([]int32, 0, len(msgIDs))
	for _, id := range msgIDs {
		idxs = append(idxs, artIdxFor(t, q, jobID, id))
	}
	if err := q.AckPermanentFailure(jobID, idxs); err != nil {
		t.Fatalf("ackFailed: %v", err)
	}
}

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

package app_test

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/queue"
)

// See ackhelpers_test.go (package app) for the rationale — this is the same
// set of helpers, duplicated because the external test package (app_test)
// cannot see the internal one's unexported identifiers.

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

func ackDone(t *testing.T, q *queue.Queue, jobID string, msgIDs ...string) {
	t.Helper()
	idxs := artIdxsFor(t, q, jobID, msgIDs...)
	ackDoneIdx(t, q, jobID, idxs...)
}

func ackFailed(t *testing.T, q *queue.Queue, jobID string, msgIDs ...string) {
	t.Helper()
	idxs := artIdxsFor(t, q, jobID, msgIDs...)
	if err := q.AckPermanentFailure(jobID, idxs); err != nil {
		t.Fatalf("ackFailed: %v", err)
	}
}

func fileIdxForArticle(m *queue.Manifest, i int) (int, bool) {
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		if i >= lo && i < hi {
			return fi, true
		}
	}
	return 0, false
}

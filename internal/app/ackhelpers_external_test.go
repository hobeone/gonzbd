package app_test

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/queue"
)

// See ackhelpers_test.go (package app) for the rationale — this is the same
// set of helpers, duplicated because the external test package (app_test)
// cannot see the internal one's unexported identifiers.

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

func ackDone(t *testing.T, q *queue.Queue, jobID string, msgIDs ...string) {
	t.Helper()
	idxs := make([]int32, 0, len(msgIDs))
	for _, id := range msgIDs {
		idxs = append(idxs, artIdxFor(t, q, jobID, id))
	}
	ackDoneIdx(t, q, jobID, idxs...)
}

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

func fileIdxForArticle(m *queue.Manifest, i int) (int, bool) {
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		if i >= lo && i < hi {
			return fi, true
		}
	}
	return 0, false
}

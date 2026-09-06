package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// artIdxsFor resolves several message IDs against ONE job.
//
//dupcomment:ok per-package copies of one test helper; the automatic same-basename exemption misses them because the app_test copy is named ackhelpers_external_test.go
func artIdxsFor(t *testing.T, disp *dispatch.Dispatcher, jobID string, msgIDs ...string) []int32 {
	t.Helper()
	j, ok := disp.Job(jobID)
	if !ok {
		t.Fatalf("artIdxsFor: job %s not in dispatcher", jobID)
	}
	m, err := j.Manifest()
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
func ackDoneIdx(t *testing.T, disp *dispatch.Dispatcher, jobID string, artIdxs ...int32) {
	t.Helper()
	if len(artIdxs) == 0 {
		return
	}

	j, ok := disp.Job(jobID)
	if !ok {
		t.Fatalf("ackDoneIdx: job %s not in dispatcher", jobID)
	}
	m, err := j.Manifest()
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

	if err := j.SeedFromRuns(runs); err != nil {
		t.Fatalf("ackDoneIdx: SeedFromRuns: %v", err)
	}
}

// ackDone is ackDoneIdx keyed by message ID.
func ackDone(t *testing.T, disp *dispatch.Dispatcher, jobID string, msgIDs ...string) {
	t.Helper()
	idxs := artIdxsFor(t, disp, jobID, msgIDs...)
	ackDoneIdx(t, disp, jobID, idxs...)
}

// ackFailed is MarkArticleFailed keyed by message ID.
func ackFailed(t *testing.T, disp *dispatch.Dispatcher, jobID string, msgIDs ...string) {
	t.Helper()
	j, ok := disp.Job(jobID)
	if !ok {
		t.Fatalf("ackFailed: job %s not in dispatcher", jobID)
	}
	idxs := artIdxsFor(t, disp, jobID, msgIDs...)
	for _, a := range idxs {
		if err := j.MarkArticleFailed(int(a)); err != nil {
			t.Fatalf("ackFailed: %v", err)
		}
	}
}

// fileIdxForArticle returns the manifest file index owning global article
// index i.
func fileIdxForArticle(m *job.Manifest, i int) (int, bool) {
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		if i >= lo && i < hi {
			return fi, true
		}
	}
	return 0, false
}

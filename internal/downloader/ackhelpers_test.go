package downloader

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
)

// artIdxFor resolves a message ID to its global article index.
func artIdxFor(t *testing.T, disp *dispatch.Dispatcher, jobID, msgID string) int32 {
	t.Helper()
	return artIdxsFor(t, disp, jobID, msgID)[0]
}

// artIdxsFor resolves several message IDs against ONE job.
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
		byID[m.ArticleID(i)] = int32(i)
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

// ackDoneIdx marks articles done directly on the job.
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
	for _, a := range artIdxs {
		i := int(a)
		if err := j.MarkArticleDone(i, int64(m.ArticleBytes(i)), "test"); err != nil {
			t.Fatalf("MarkArticleDone: %v", err)
		}
	}
}

// ackDone is ackDoneIdx keyed by message ID.
func ackDone(t *testing.T, disp *dispatch.Dispatcher, jobID string, msgIDs ...string) {
	t.Helper()
	idxs := artIdxsFor(t, disp, jobID, msgIDs...)
	ackDoneIdx(t, disp, jobID, idxs...)
}

// ackFailed marks articles permanently failed on the job.
func ackFailed(t *testing.T, disp *dispatch.Dispatcher, jobID string, msgIDs ...string) {
	t.Helper()
	j, ok := disp.Job(jobID)
	if !ok {
		t.Fatalf("ackFailed: job %s not in dispatcher", jobID)
	}
	idxs := artIdxsFor(t, disp, jobID, msgIDs...)
	for _, a := range idxs {
		if err := j.MarkArticleFailed(int(a)); err != nil {
			t.Fatalf("MarkArticleFailed: %v", err)
		}
	}
}

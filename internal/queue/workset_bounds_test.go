package queue

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// newTestQueueWithJobLogging is newTestQueueWithJob with the queue's log
// captured, because the out-of-range paths below are deliberately non-fatal:
// the warning IS the observable behaviour, and A2 is what makes it so. An
// assertion on state alone cannot tell "the index was rejected and reported"
// from "the index was rejected silently", which is the distinction these
// tests exist to hold.
func newTestQueueWithJobLogging(t *testing.T, jobID string, n int) (*Queue, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir),
		WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))))
	job := makeMultiFileJob(t, jobID, 1, n)
	job.ID = jobID
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return q, &buf
}

// TestAckDurable_RejectsAnArticleIndexAtTheArticleCount pins the first invalid
// index rather than an arbitrarily large one.
//
// TestArtIdx_EdgeCases covers out-of-range handling with -1 and nArt+10, and
// asserts only that no error comes back. Three mutations survive that:
// `i >= nArt` becoming `i > nArt` (nArt+10 is rejected either way), the
// invalidCount++ becoming --, and the `invalidCount > 0` warn guard — because
// nothing looks at the count or the log.
//
// The index that separates them is nArt itself. Under `i > nArt` it reaches
// markDone, whose manifest lookup runs off the end of the per-file slice, so
// the mutant is loud; the counter and warn-guard mutants are silent, and only
// the log assertion catches those.
func TestAckDurable_RejectsAnArticleIndexAtTheArticleCount(t *testing.T) {
	const n = 4
	q, logs := newTestQueueWithJobLogging(t, "bounds-ack", n)

	// A proof naming article n. The stub target is given n+1 articles so the
	// barrier can place the index and actually mint it; the QUEUE still has
	// only n, which is what makes the proof out of range on arrival.
	p := mintProof(t, "bounds-ack", []int32{int32(n)})

	if err := q.AckDurable(p); err != nil {
		t.Fatalf("AckDurable returned %v; an out-of-range index must not fail the "+
			"whole ack — the in-range articles were made durable by a real fsync", err)
	}

	snap, err := q.Get("bounds-ack")
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Progress().ArticlesResolved(); got != 0 {
		t.Errorf("ArticlesResolved = %d, want 0 — no article of this job was named", got)
	}

	out := logs.String()
	if !strings.Contains(out, "AckDurable: out-of-range article index in proof") {
		t.Fatalf("no warning was logged for an out-of-range index. A2 forbids dropping "+
			"it silently: this is the only record that an upstream numbering defect "+
			"reached the queue.\nlog was:\n%s", out)
	}
	if !strings.Contains(out, "invalid_count=1") {
		t.Errorf("warning did not report invalid_count=1, so the count is not being "+
			"accumulated as the message claims.\nlog was:\n%s", out)
	}
	if !strings.Contains(out, "num_articles=4") {
		t.Errorf("warning did not report num_articles=4.\nlog was:\n%s", out)
	}
}

// TestAckPermanentFailure_RejectsAnArticleIndexAtTheArticleCount is the same
// boundary for the failure door, which has its own copy of the bounds check,
// its own counter and its own warn guard — and so its own three mutants.
func TestAckPermanentFailure_RejectsAnArticleIndexAtTheArticleCount(t *testing.T) {
	const n = 4
	q, logs := newTestQueueWithJobLogging(t, "bounds-fail", n)

	if err := q.AckPermanentFailure("bounds-fail", []int32{int32(n)}); err != nil {
		t.Fatalf("AckPermanentFailure returned %v; an out-of-range index must not "+
			"fail the whole call", err)
	}

	snap, err := q.Get("bounds-fail")
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Progress().ArticlesResolved(); got != 0 {
		t.Errorf("ArticlesResolved = %d, want 0", got)
	}
	if got := snap.Progress().ArticlesFailed(); got != 0 {
		t.Errorf("ArticlesFailed = %d, want 0 — index %d is not an article of this job", got, n)
	}

	out := logs.String()
	if !strings.Contains(out, "AckPermanentFailure: out-of-bounds article index received") {
		t.Fatalf("no warning was logged for an out-of-range index (A2).\nlog was:\n%s", out)
	}
	if !strings.Contains(out, "invalid_count=1") {
		t.Errorf("warning did not report invalid_count=1.\nlog was:\n%s", out)
	}
	if !strings.Contains(out, "num_articles=4") {
		t.Errorf("warning did not report num_articles=4.\nlog was:\n%s", out)
	}
}

// TestAckPermanentFailure_StillFailsTheInRangeArticlesBesideAnInvalidOne pins
// the half the boundary tests above cannot see: rejecting the invalid index
// must not discard the valid ones travelling with it. The comment on
// AckDurable's guard states this explicitly, and nothing held it.
func TestAckPermanentFailure_StillFailsTheInRangeArticlesBesideAnInvalidOne(t *testing.T) {
	const n = 4
	q, _ := newTestQueueWithJobLogging(t, "bounds-mixed", n)

	if err := q.AckPermanentFailure("bounds-mixed", []int32{-1, 1, int32(n)}); err != nil {
		t.Fatalf("AckPermanentFailure: %v", err)
	}
	snap, err := q.Get("bounds-mixed")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Progress().ArticleFailed(1) {
		t.Error("article 1 is in range and was named; it must still be marked failed " +
			"even though invalid indices travelled in the same call")
	}
	if got := snap.Progress().ArticlesFailed(); got != 1 {
		t.Errorf("ArticlesFailed = %d, want exactly 1", got)
	}
}

// TestAck_ValidIndicesLogNoOutOfRangeWarning is the negative half of the two
// boundary tests above, and it is needed for a reason worth stating: they
// assert the warning FIRES on a bad index, which `invalidCount > 0` and the
// mutant `invalidCount >= 0` both satisfy when the count is 1. Only a call
// with nothing invalid separates them — there the mutant reports an
// out-of-range index on every healthy ack in the system.
func TestAck_ValidIndicesLogNoOutOfRangeWarning(t *testing.T) {
	const n = 4

	t.Run("AckDurable", func(t *testing.T) {
		q, logs := newTestQueueWithJobLogging(t, "clean-ack", n)
		p := mintProof(t, "clean-ack", []int32{0, 1})
		if err := q.AckDurable(p); err != nil {
			t.Fatalf("AckDurable: %v", err)
		}
		snap, err := q.Get("clean-ack")
		if err != nil {
			t.Fatal(err)
		}
		// Grounding: if the ack did not land, "no warning" would be true for
		// a reason that has nothing to do with the guard under test.
		if got := snap.Progress().ArticlesResolved(); got != 2 {
			t.Fatalf("ArticlesResolved = %d, want 2; the fixture never performed a "+
				"successful ack, so it cannot observe the guard it names", got)
		}
		if out := logs.String(); strings.Contains(out, "out-of-range") {
			t.Errorf("an ack naming only valid indices logged an out-of-range warning, "+
				"so the warning does not distinguish healthy traffic from a "+
				"numbering defect.\nlog was:\n%s", out)
		}
	})

	t.Run("AckPermanentFailure", func(t *testing.T) {
		q, logs := newTestQueueWithJobLogging(t, "clean-fail", n)
		if err := q.AckPermanentFailure("clean-fail", []int32{0, 1}); err != nil {
			t.Fatalf("AckPermanentFailure: %v", err)
		}
		snap, err := q.Get("clean-fail")
		if err != nil {
			t.Fatal(err)
		}
		if got := snap.Progress().ArticlesFailed(); got != 2 {
			t.Fatalf("ArticlesFailed = %d, want 2; the fixture never performed a "+
				"successful call, so it cannot observe the guard it names", got)
		}
		if out := logs.String(); strings.Contains(out, "out-of-bounds") {
			t.Errorf("a call naming only valid indices logged an out-of-bounds "+
				"warning.\nlog was:\n%s", out)
		}
	})
}

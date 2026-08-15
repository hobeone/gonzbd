package queue

import (
	"encoding/hex"
	"testing"
)

// TestDecodeArticleFlags pins the manifest-free unpacking directly, because
// its refusal cases are the ones that decide what a corrupt column means, and
// end-to-end they are indistinguishable from a job that has downloaded nothing.
//
// The format is two equal-length halves — [done bits][failed bits], each
// ceil(N/8) bytes. decodeArticlesDone reads the same bytes against a live job,
// taking N from the manifest; this reads it against job_files.article_count,
// which is where the non-resident path has to get N from.
func TestDecodeArticleFlags(t *testing.T) {
	// Ten articles: 0 and 3 done, 7 done AND failed, the rest untouched.
	// Two bytes per half.
	pack := func(doneBits, failedBits []int, n int) string {
		numBytes := (n + 7) / 8
		buf := make([]byte, numBytes*2)
		for _, i := range doneBits {
			buf[i/8] |= 1 << (i % 8)
		}
		for _, i := range failedBits {
			buf[numBytes+i/8] |= 1 << (i % 8)
		}
		return hex.EncodeToString(buf)
	}

	t.Run("round-trips done and failed independently", func(t *testing.T) {
		done, failed := decodeArticleFlags(pack([]int{0, 3, 7}, []int{7}, 10), 10)
		if len(done) != 10 || len(failed) != 10 {
			t.Fatalf("lengths = %d/%d, want 10/10", len(done), len(failed))
		}
		for i, want := range []bool{true, false, false, true, false, false, false, true, false, false} {
			if done[i] != want {
				t.Errorf("done[%d] = %v, want %v", i, done[i], want)
			}
		}
		for i, want := range []bool{false, false, false, false, false, false, false, true, false, false} {
			if failed[i] != want {
				t.Errorf("failed[%d] = %v, want %v — a failed article is done AND failed, "+
					"and the two halves must not be read at the same offset", i, failed[i], want)
			}
		}
	})

	// Every refusal returns nil rather than a partial answer. Nil reads as
	// "nothing resolved", which is what an absent column already produced: the
	// articles come back Outstanding and hydration re-derives them. A partial
	// answer would resolve the WRONG articles, which is not recoverable.
	for _, tc := range []struct {
		name    string
		encoded string
		count   int
		why     string
	}{
		{"an empty column", "", 10, "a job that has never been saved has no bits yet"},
		{"no articles", pack([]int{0}, nil, 8), 0, "a file with no articles has no state to restore"},
		{"not hex", "zzzz", 10, "the column did not come from encodeArticlesDone"},
		{"too short for the count", pack([]int{0}, nil, 8), 64, "guessing which prefix is the done half would resolve the wrong articles"},
		{"one half only", hex.EncodeToString([]byte{0xFF, 0xFF}), 10, "the historical done-only format is not this one"},
	} {
		t.Run(tc.name+" restores nothing", func(t *testing.T) {
			done, failed := decodeArticleFlags(tc.encoded, tc.count)
			if done != nil || failed != nil {
				t.Errorf("decodeArticleFlags(%q, %d) = %v/%v, want nil/nil — %s",
					tc.encoded, tc.count, done, failed, tc.why)
			}
		})
	}

	// A width that is an exact multiple of 8 is the boundary the ceil
	// arithmetic gets wrong in both directions.
	t.Run("an exact byte multiple", func(t *testing.T) {
		done, failed := decodeArticleFlags(pack([]int{7}, []int{7}, 8), 8)
		if len(done) != 8 || !done[7] || !failed[7] {
			t.Errorf("done = %v, failed = %v, want the last article of an 8-article file "+
				"resolved", done, failed)
		}
	})
}

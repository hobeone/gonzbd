package job

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
)

// TestAckDurable_ExternallyConstructibleEmptyProofAcksNothing pins the early
// return in AckDurable.
//
// internal/durability's proof.go cited this test by name as what holds that
// return in place. It did not exist. The argument it was cited in still
// stands, and it is worth restating because it is what makes the guard
// load-bearing rather than tidy: durability.DurableProof is constructible
// outside internal/durability, but only as a zero value, because there is no
// exported way to populate its article list. So the compiler bounds the
// PAYLOAD — an externally built proof is necessarily empty — and this early
// return is what converts that empty payload into a no-op.
//
// If the branch ever became "an empty proof acks the whole job", the
// compile-time bound would buy nothing: any package that can name the type
// could mark every article of any job durable without a single fsync having
// happened.
func TestAckDurable_ExternallyConstructibleEmptyProofAcksNothing(t *testing.T) {
	t.Parallel()

	j := New("job-1", "test-job", Policy{})
	m := newManifest([]JobFile{{
		Subject: "file1.rar",
		Bytes:   200,
		Articles: []JobArticle{
			{ID: "<1@x>", Bytes: 100, Number: 1},
			{ID: "<2@x>", Bytes: 100, Number: 2},
		},
	}})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	before, err := j.CountUnfinishedArticles(0)
	if err != nil {
		t.Fatalf("CountUnfinishedArticles: %v", err)
	}
	if before != 2 {
		t.Fatalf("CountUnfinishedArticles before the ack = %d, want 2; the fixture is not what this test assumes", before)
	}

	// The zero value is the only DurableProof a package outside
	// internal/durability can build.
	var external durability.DurableProof
	if n := len(external.Articles()); n != 0 {
		t.Fatalf("a zero DurableProof names %d articles, want 0 — the compile-time bound this "+
			"test rests on is gone, and an external caller can now populate a proof", n)
	}

	invalid, nArt, err := j.AckDurable(external)
	if err != nil {
		t.Fatalf("AckDurable with an empty proof: %v", err)
	}
	if invalid != 0 || nArt != 0 {
		t.Errorf("AckDurable(empty) = (invalid:%d, nArt:%d), want (0, 0)", invalid, nArt)
	}

	after, err := j.CountUnfinishedArticles(0)
	if err != nil {
		t.Fatalf("CountUnfinishedArticles: %v", err)
	}
	if after != before {
		t.Errorf("CountUnfinishedArticles after acking an EMPTY proof = %d, want %d unchanged: "+
			"an empty proof acked %d article(s) that no fsync ever covered", after, before, before-after)
	}
}

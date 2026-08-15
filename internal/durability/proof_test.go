package durability

import "testing"

// TestDurableProof_CarriesItsPayload is the only thing a proof must do.
// Its real guarantee — that no package outside this one can construct it —
// is a language property, not a testable one: no in-package test can assert
// the absence, because newProof is reachable from anywhere in this package.
// It is enforced at the package boundary by the compiler.
func TestDurableProof_CarriesItsPayload(t *testing.T) {
	p := newProof("job-1", []int32{3, 7, 11})
	if p.JobID() != "job-1" {
		t.Errorf("JobID() = %q, want %q", p.JobID(), "job-1")
	}
	if got := p.Articles(); len(got) != 3 || got[0] != 3 || got[2] != 11 {
		t.Errorf("Articles() = %v, want [3 7 11]", got)
	}
}

// TestDurableProof_ArticlesIsNotAliased pins that a caller mutating the
// returned slice cannot corrupt the proof, which the barrier may still hold.
func TestDurableProof_ArticlesIsNotAliased(t *testing.T) {
	p := newProof("job-1", []int32{3, 7, 11})
	got := p.Articles()
	got[0] = 99
	if p.Articles()[0] != 3 {
		t.Fatal("mutating the returned slice mutated the proof")
	}
}

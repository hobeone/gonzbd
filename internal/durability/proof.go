package durability

import "slices"

// DurableProof witnesses that a completed fsync covers the articles it names.
//
// It has no exported fields and no exported constructor, so no package
// outside internal/durability can create one. Since the queue's ack entry
// point takes a DurableProof, "ack before fsync" is not a rule anyone must
// remember — it is code that does not compile.
//
// The guarantee is package-scoped: within internal/durability, newProof is
// reachable from anywhere. Exactly two functions call it — Barrier.Run and
// Barrier.FinalizeFile — and both are methods on Barrier, reached only after a
// Sync returned nil. That pair is what review has to hold. Outside this package
// the guarantee is absolute.
type DurableProof struct {
	jobID string
	arts  []int32
}

// newProof is unexported deliberately. See the type doc.
func newProof(jobID string, arts []int32) DurableProof {
	return DurableProof{jobID: jobID, arts: slices.Clone(arts)}
}

// JobID returns the job the proof's articles belong to.
func (p DurableProof) JobID() string { return p.jobID }

// Articles returns a copy, so a consumer cannot mutate a proof the barrier
// may still hold.
func (p DurableProof) Articles() []int32 { return slices.Clone(p.arts) }

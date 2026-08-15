package durability

import "slices"

// DurableProof witnesses that a completed fsync covers the articles it names.
//
// It has no exported fields and no exported constructor, so no package
// outside internal/durability can create a proof that NAMES ANY ARTICLE.
// Since the queue's ack entry point takes a DurableProof, "ack before fsync"
// is not a rule anyone must remember for that entry point.
//
// # The precise bound, because an earlier version of this comment overstated it
//
// It is NOT true that an outside package cannot create a DurableProof at all.
// Go permits a composite literal with no field values even when every field is
// unexported, so `durability.DurableProof{}` and `var p durability.DurableProof`
// both compile anywhere. This was verified by compiling them from a scratch
// package, not reasoned about.
//
// What holds is that such a proof is necessarily EMPTY — arts is nil, because
// there is no exported way to populate it — and Queue.AckDurable returns nil
// without touching any article when Articles() is empty. So the compiler bounds
// the PAYLOAD, and a one-line early return converts an empty payload into a
// no-op. That early return is load-bearing, not a tidy guard: if it ever became
// "an empty proof acks the whole job", the compile-time bound would buy nothing.
// TestAckDurable_ExternallyConstructibleEmptyProofAcksNothing pins it.
//
// The guarantee is package-scoped: within internal/durability, newProof is
// reachable from anywhere. Exactly two functions call it — Barrier.Run and
// Barrier.FinalizeFile — and both are methods on Barrier, reached only after a
// Sync returned nil. That pair is what review has to hold. Outside this package
// the guarantee is absolute FOR THE ARTICLES A PROOF CAN NAME — which is the
// bound stated above, not a broader one.
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

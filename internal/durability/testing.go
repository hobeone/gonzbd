package durability

// NewTestDurableProof constructs a DurableProof for cross-package unit tests.
// It is intended solely for test harnesses that must construct a valid proof
// carrying articles to verify acker and job behaviors.
func NewTestDurableProof(jobID string, arts []int32) DurableProof {
	return newProof(jobID, arts)
}

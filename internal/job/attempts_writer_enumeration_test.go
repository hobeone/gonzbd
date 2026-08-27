package job

import (
	"slices"
	"testing"
)

// BeginAttempt's boundary guard (job.go) checks only the most recent
// element of j.attempts, on the premise that BeginAttempt is the only place
// that ever appends (or otherwise assigns) to that field. A grep result
// frozen in a comment records that someone once checked; it does not keep
// enforcing the claim as the package changes. This test does — it uses
// scanWriters (writer_enumeration_test.go), the same scan
// TestOutcomeWrites_MatchTheEnumerationStatedInProse uses for
// Attempt.outcome, applied to a different population (every write site for
// Job.attempts, not Attempt.outcome).
var attemptsWriters = []string{"BeginAttempt"}

func TestAttemptsWrites_MatchTheEnumerationStatedInProse(t *testing.T) {
	writers := scanWriters(t, "attempts")
	if !slices.Equal(writers, attemptsWriters) {
		t.Errorf("functions writing j.attempts = %v, want %v\n\n"+
			"BeginAttempt's doc comment claims it is the only place "+
			"j.attempts is appended to, which is what lets the boundary "+
			"guard check just the last element. If a second writer is "+
			"correct, say so at that comment AND update this list "+
			"together — a comment that still says \"the only place\" once "+
			"a second writer exists is worse than no comment at all.",
			writers, attemptsWriters)
	}
}

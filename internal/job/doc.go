// Package job is the lifecycle machine for a download.
//
// # Three axes
//
// State is where a job's current attempt is and what may happen next.
// Activity is what is executing right now and nothing branches on it.
// Outcome is the attempt's verdict, assigned once on the edge into Finished
// and never revised — TestOutcomeWrites_MatchTheEnumerationStatedInProse
// enumerates Outcome's writers and pins that finish is the only one. Keeping
// the three apart is what collapses the transition table: "still producing,
// doing something else now" is an Activity write rather than a state change.
//
// # The boundary
//
// Fetching, Assessing and Repairing form the Correctness zone: reversible,
// idempotent, touching nothing outside the job's own working directory.
// Extracting and Finalizing form Production: forward-only and destructive —
// they delete archives, move files and run user scripts.
//
// A job crosses from Correctness to Production exactly once and never
// returns. TestBoundaryIsOneWay enumerates AllStates() and fails if any edge
// violates that, so the invariant is pinned by a test rather than by this
// sentence.
//
// That one property defines four others: pause granularity, cancel
// semantics, the acquisition lease's lifetime, and which failures are
// recoverable.
//
// # One decider
//
// Within the Correctness zone, Assessing is the only state with more than
// one non-pause, non-cancel work successor — Fetching and Repairing each
// have exactly one. Every path through a job's Correctness zone is therefore
// Fetching → Assessing → one of Assessing's own destinations, and the test
// surface there is the verdict function rather than the graph.
// TestOnlyAssessingBranchesWithinCorrectness pins this for the Correctness
// zone specifically — it does not examine Production (Extracting,
// Finalizing), where the same property happens to hold today but is
// unenforced. TestEdgeCountsMatchTheStatedPartition pins the edge counts the
// partition implies.
//
// # Attempts
//
// The machine lives on the current Attempt, not on the Job. A Job holds a
// list of attempts, each with its own write-once Outcome, so a retry appends
// a verdict instead of revising one. An attempt opens when a lease is first
// issued and no attempt is open, and closes when it reaches Finished — pause
// and resume inside it do not end it.
//
// A Job with no attempts has never run. That is what HasRun reports, and it
// is exact where any predicate over byte counters would conflate "did not
// start" with "started and got nowhere".
//
// # What this package does not do
//
// No I/O, no locking beyond its own Job.mu, and no import of any other
// package in this repository except internal/constants — which, among this
// package's non-test sources, appears in sabnzbd.go alone, for the one-way
// translation to the legacy API vocabulary.
// `go list -deps ./internal/job/ | grep hobeone` names exactly
// internal/constants and internal/job itself, but that check is blind to
// test files by construction — sabnzbd_test.go is package job and also
// imports internal/constants, which `go list -deps` cannot see either way.
// TestOnlyOneNonTestFileImportsConstants (sabnzbd_test.go) enforces the
// narrower, actual claim: sabnzbd.go is the sole NON-TEST file importing
// internal/constants. It explicitly skips every "_test.go" file while
// scanning, so a _test.go file's own import of internal/constants — as
// sabnzbd_test.go has, to write its constants.Status table — is outside that
// test's scope, and outside what `go list -deps` can see either way, for the
// same reason.
//
// A Job method never calls a Queue method. That holds structurally today
// because this package cannot see a Queue — nothing here imports it. Whether
// a Queue, once built, always locks its own mutex before calling into Job is
// a design intent for a later plan, not something this package can enforce
// or has enforced.
//
// The design this implements is docs/superpowers/specs/2026-08-25-job-lifecycle-design.md.
package job

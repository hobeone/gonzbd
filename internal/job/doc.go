// Package job is the lifecycle machine for a download.
//
// # Four axes
//
// State is where a job's current attempt is and what may happen next.
// Activity is what is executing right now and nothing branches on it.
// Outcome is the attempt's verdict, assigned once by finish
// and never revised — TestOutcomeWrites_MatchTheEnumerationStatedInProse
// enumerates Outcome's writers and pins that finish is the only one. Intent
// is what a person has asked of this job — IntentRun, IntentPause or the
// latching IntentCancel — independent of where the job is, what it is
// executing, or how its last attempt ended;
// TestIntentWrites_MatchTheEnumerationStatedInProse pins SetIntent as its
// sole writer. Keeping the four apart is what collapses the transition
// table: "still producing, doing something else now" is an Activity write
// rather than a state change, and "stop at the next gate" is an Intent write
// rather than a state one.
//
// State lives on the current Attempt; Intent lives on the Job, because a
// paused job that is retried stays paused — the request was about the job,
// not about one run of it.
//
// # The boundary
//
// Fetching, Assessing and Repairing form the Correctness zone: reversible,
// idempotent, touching nothing outside the job's own working directory.
// Extracting and Finalizing form Production: forward-only and destructive —
// they delete archives, move files and run user scripts.
//
// A job crosses from Correctness to Production exactly once and never
// returns — and that holds across ATTEMPTS, not merely within one, because a
// Job holds a list of them and a fresh attempt opens in Fetching.
//
// Two tests pin it, and the distinction between them is the whole reason the
// second exists. TestBoundaryIsOneWay (transition_test.go) enumerates
// AllStates() and fails if any single EDGE runs Production→Correctness: that
// is a property of legalEdges. The invariant above is a property of
// REACHABILITY, and two individually legal edges can compose into the transit
// the edge map forbids directly — which is exactly how both escapes found on
// this branch got through, with TestBoundaryIsOneWay green throughout.
// TestBoundaryIsUnreachableByAnyPath (reachability_test.go) is what actually
// pins the sentence above: it replays action sequences through the exported
// doors and asserts the invariant at every reachable configuration. Cite that
// one when you mean the invariant; cite TestBoundaryIsOneWay only when you
// mean the edge map.
//
// That one property defines four others: pause granularity, cancel
// semantics, the acquisition lease's lifetime, and which failures are
// recoverable.
//
// # next, and the boundary's own door
//
// next records that the open attempt's current state has ENDED and where it
// continues to — the marker the removed Waiting state used to carry. Work
// that ends without continuing settles via Finish instead and leaves next
// unset; Finalizing is not an exception to that rule. SetNext writes it once
// per visit; Transition, Cross and Finish each clear it when they take the
// move they were asked for — see ErrNextAlreadySet.
//
// Transition, SetNext, SetActivity, Cross and Finish share one precondition —
// an open attempt (see ErrNoOpenAttempt) — and all five get it from one place,
// withOpenAttempt. The two that yield a lease reach it through
// withOpenAttemptLease, an adapter over that helper rather than a second copy
// of it. Cross is also the sole door across the irreversible boundary, and it
// yields the lease inside the same locked callback that mutates the attempt —
// entering Production and giving up the lease cannot happen as two separate
// calls without a window where one could be forgotten. Transition refuses the one Correctness→Production edge
// outright (ErrCrossRequired) precisely so Cross is the only way to take it.
// TestOutcomeWrites_MatchTheEnumerationStatedInProse
// (outcome_writer_enumeration_test.go) pins the sole writer of outcome. Note
// what it names: the unexported Attempt method finish, not the exported Job
// door Finish. The door takes the lock and yields the lease; the method is
// what actually assigns the field, and the enumeration asserts against the
// assignment, so it is the method name that appears in it.
//
// There was a matching TestCrossedWrites_MatchTheEnumerationStatedInProse for
// a crossed FIELD. Change 03 deleted the field — crossed() is now derived from
// IsProduction(a.state) — and the test went with it, because a derived value
// has no writers to enumerate. What replaced it is not another writer
// enumeration but TestBoundaryIsUnreachableByAnyPath, which checks the
// property the latch existed to provide.
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
// unenforced. TestLegalEdgesIsTheWorkSpine pins legalEdges' exact contents.
//
// # Attempts
//
// The machine lives on the current Attempt, not on the Job. A Job holds a
// list of attempts, each with its own write-once Outcome, so a retry appends
// a verdict instead of revising one. An attempt opens on the first call to
// BeginAttempt while none is open, in Fetching, holding nothing —
// BeginAttempt does not take a lease. Fetching holding nothing is a legal,
// representable state: it is exactly what a paused or restarted fetch looks
// like, and requiring a lease to reach it (rather than to actually fetch)
// would contradict that. An attempt closes when finish assigns its verdict — pause
// and resume, which surrender and later re-take a lease, do not end it.
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
// The design this implements is
// docs/superpowers/specs/2026-08-25-job-lifecycle-design.md, as amended by
// docs/superpowers/specs/2026-08-26-lifecycle-intents-design.md — the source
// of Intent, next, Cross and the removal of Waiting described above.
package job

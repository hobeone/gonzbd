package job

// LeaseID identifies one issuance. It exists so a pool can tell two
// outstanding leases apart, verify that a lease handed back is one it issued,
// and name a lease in a log line.
//
// Identity cannot come from the pointer. Lease had no fields while its
// manifest and barrier waited for Half B, and Go gives distinct zero-size
// allocations the same address — so `&Lease{} == &Lease{}` was true and a
// map[*Lease]*Job held one entry for two jobs. That is a defect which would
// have vanished silently when B2 adds the first real field, working by
// accident until then. TestLease_HasDistinctIdentity pins the size directly.
type LeaseID uint64

// LeaseUnset is the invalid zero, in the same spirit as StateUnset: a lease
// nobody issued must not be indistinguishable from one that was. Job.Grant
// refuses it.
const LeaseUnset LeaseID = 0

// Lease is the admission token for the correctness loop.
//
// Holding one is what makes a job RUNNABLE in Fetching, Assessing and
// Repairing; it is surrendered at the crossing, and Extracting and Finalizing
// hold only a compute slot. That is why running-ness is a question about what
// a job HOLDS rather than about pool occupancy — see the design's §3.4.
//
// A lease is admission to the correctness loop, and its id is the whole of it.
//
// This type reserved `manifest *Manifest` and `barrier *StorageBarrier` fields
// for "Half B2", on prior spec §6's argument that the three share a lifetime.
// Neither arrived, and neither should:
//
//   - The BARRIER is process-level, not per-lease. §10.1's banner in
//     2026-08-25-job-lifecycle-design.md records the refutation: one Barrier is
//     built in app.New with a cross-job overlapKey map, and reconciling
//     per-lease would destroy durable records for jobs in post-processing.
//
//   - The MANIFEST is keyed on holding what a position requires, not on
//     holding a lease. grantFor runs under Queue.mu and Hydrate does disk I/O,
//     so there is no manifest to install at grant time
//     (internal/dispatch/tick.go, reconcileResidency). And post-processing
//     reads the manifest at Extracting/Finalizing, where needsLease is false
//     (internal/sched/requirements.go) — four sites, `grep -n 'Manifest()'
//     internal/postproc/*.go | grep -v _test` — so a lease-gated manifest
//     would be unreadable exactly where the stage that verifies CRCs needs it.
//
// Residency lives on job.Job — see content.go's AttachContent/Evict pair — and
// is driven by dispatch.Residency against RenderView.Holds. Do not reintroduce
// either field here.
type Lease struct {
	id LeaseID
}

// NewLease mints a lease with the given id. The caller — the Queue's pool — is
// the only thing that knows what ids it has issued, so issuance lives there
// and this is a plain constructor. Passing LeaseUnset produces a lease Grant
// will refuse; that is deliberate, so there is one enforcement point rather
// than two.
func NewLease(id LeaseID) *Lease { return &Lease{id: id} }

// ID reports which issuance this lease is. A nil lease reports LeaseUnset
// rather than panicking.
//
// Nil is not a defensive hypothetical here: Surrender, Cross and Finish all
// return (*Lease, error) and legitimately yield nil when the job held nothing
// — surrenderLocked returns j.lease unconditionally, and the design has
// reclaim no-op on nil "so that no call site has to test for it". A logging or
// auditing caller writing l.ID() on that return would panic, and LeaseUnset is
// exactly the value that already means "no issuance".
func (l *Lease) ID() LeaseID {
	if l == nil {
		return LeaseUnset
	}
	return l.id
}

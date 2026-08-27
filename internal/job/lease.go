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

// Lease is the admission token for the correctness loop. Prior spec §6:
// pool-A capacity, the resident Manifest and the StorageBarrier have exactly
// one lifetime between them — from a job beginning to fetch until it crosses
// the irreversible boundary — and "three things with one lifetime are one
// object".
//
// Holding one is what makes a job RUNNABLE in Fetching, Assessing and
// Repairing; it is surrendered at the crossing, and Extracting and Finalizing
// hold only a compute slot. That is why running-ness is a question about what
// a job HOLDS rather than about pool occupancy — see the design's §3.4.
//
// The manifest and barrier fields arrive in Half B2. The pool that ISSUES
// these arrives earlier, in B1, which is why the id below lands now and the
// payload later. They are named here rather than left out because §6's
// argument is that the three share a lifetime and are therefore one object;
// an opaque placeholder now would be a second representation of the same
// thing, which is the ownership violation §6 exists to retire.
type Lease struct {
	id LeaseID
	// manifest *Manifest        // Half B2
	// barrier  *StorageBarrier  // Half B2
}

// NewLease mints a lease with the given id. The caller — the Queue's pool — is
// the only thing that knows what ids it has issued, so issuance lives there
// and this is a plain constructor. Passing LeaseUnset produces a lease Grant
// will refuse; that is deliberate, so there is one enforcement point rather
// than two.
func NewLease(id LeaseID) *Lease { return &Lease{id: id} }

// ID reports which issuance this lease is.
func (l *Lease) ID() LeaseID { return l.id }

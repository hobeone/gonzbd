package job

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
// The manifest and barrier fields arrive in Half B, together with the Queue
// that issues these. They are named here rather than left out because §6's
// argument is that the three share a lifetime and are therefore one object;
// an opaque placeholder now would be a second representation of the same
// thing, which is the ownership violation §6 exists to retire.
type Lease struct {
	// manifest *Manifest        // Half B
	// barrier  *StorageBarrier  // Half B
}

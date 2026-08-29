package dispatch

import "context"

// tick is one pass over the registry. It is called only from run, so it never
// overlaps itself and needs no locking against another tick.
//
// It copies the registry under d.mu and releases it before the first Advance:
// D-B9 forbids holding d.mu across such a call, because Workers.Abort runs
// inside Queue.mu and an Abort implementation that took d.mu would deadlock
// ABBA against a concurrent Cancel.
//
// Residency reconciliation, worker launch and eviction are not implemented
// yet — they land in Tasks 4-6. This tick only advances.
func (d *Dispatcher) tick(_ context.Context) {
	for _, j := range d.snapshotOrder() {
		if err := d.q.Advance(j); err != nil {
			d.logAdvanceError(j.ID(), err)
		}
	}
}

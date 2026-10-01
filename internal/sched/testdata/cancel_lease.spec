pkg ./internal/sched/
run ^TestCancel_AnInstanceHoldingNoLeaseIsNotAborted$

# Cancel interrupts only an instance that holds a lease itself. The downloader's
# tracker reap relies on it: the instance Abort names is the one registered
# under its ID, since a removed instance has been parked. Without the lease
# check, holds() answers for a parked instance at Fetching from nothing, and at
# Assessing or Repairing from the compute slot a later instance holds under the
# same ID.

[holds() ignores the instance's own lease]
file internal/sched/queue.go
--- anchor
	if needsLease(pos) && !s.HoldsLease {
--- replace
	if false && needsLease(pos) && !s.HoldsLease {
--- end

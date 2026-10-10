package dispatch

import (
	"context"
	"errors"

	"github.com/hobeone/gonzbd/internal/job"
)

// Residency is how the dispatcher makes a job's manifest available and takes
// it away again. It names no manifest type on purpose. That used to be an
// import consequence — dispatch could not name queue.Manifest because it may
// not import internal/queue — and since the content tier moved into
// internal/job (which dispatch DOES import) it is a discipline instead,
// enforced by TestDispatchNamesNoManifestType in this package.
//
// Hydrate may block on disk I/O. The dispatcher calls it with no lock held.
//
// Hydrate's errors fall into three classes. A context error says nothing about
// the job. An error wrapping ErrResidencyFault says the job's content could not
// be read for a reason that is about the device, not the job — an unreadable
// file during verification — and the implementation parks the job itself. Any
// other error means the job can never run, and the dispatcher settles it
// Failed.
type Residency interface {
	Hydrate(ctx context.Context, id string) error
	Evict(id string)
}

// ErrResidencyFault marks a Hydrate error the dispatcher must not settle: the
// job's content could not be read for a reason that is about the device, and
// the job is parked rather than failed. See Residency.
var ErrResidencyFault = errors.New("dispatch: residency fault")

// Store is the persistence the dispatcher needs, and no more (D-B11): read the
// whole queue once at Start, and write a job's four axes when they move.
// internal/dispatch defines it; internal/dispatch/store implements it against
// SQLite. The implementation lives in its own package so this one stays free of
// a SQL driver, the same shape Residency and Runner have.
type Store interface {
	Load(ctx context.Context) ([]Persisted, error)
	Save(ctx context.Context, p Persisted) error
	Delete(ctx context.Context, id string) error
}

// Persisted is one job's durable state: identity, header, and the four axes.
// `crossed` is deliberately absent — it is derived from State via
// Attempt.crossed, and storing it would create a second source of truth that
// could disagree with State after a restore.
type Persisted struct {
	ID string
	// SortKey is queue order: the insertion sequence register assigns, carried
	// here so a restart can rebuild the order rather than inherit whatever
	// order the store happened to return rows in. Queue order is the priority
	// policy sched consults, so this is not cosmetic.
	//
	// Restore reads order from this field ALONE. That is what makes reordering
	// B2.4's problem rather than a free extension: a /api?mode=switch move that
	// did not rewrite keys would survive in memory and vanish at the next
	// restart, so B2.4 needs an atomic whole-queue resequence in the store.
	// See entry.seq (registry.go).
	SortKey int64
	Header  Header
	// Policy is what the job is permitted to do, stored resolved rather than
	// as the upstream PP integer it was derived from: PP is external
	// vocabulary that "does not exist past App" (internal/job/policy.go), so
	// persisting it would carry it back inside the internal layer. job.Job
	// already owns this field — New takes it and Policy() returns it — so
	// storing it is persisting the owner's own value, not a second
	// derivation of it.
	Policy job.Policy
	State  job.StateView
	Intent job.Intent

	DownloadStarted   int64
	DownloadFinished  int64
	Par2ReleaseReason string
	RecoveryBytes     int64
	Par2Recovered     bool
}

// Runner starts the work for one job at one state. It must return promptly —
// the dispatcher calls it from the tick goroutine, and a Runner that blocks
// stalls every other job's advance.
//
// The runner reports terminal completion by calling Dispatcher.Finished, or
// Dispatcher.FinishedJob with the instance it resolved, finished work that
// continues to another state by calling Dispatcher.AdvanceFrom, and any other
// exit by calling Dispatcher.Yielded, Dispatcher.YieldedJob or
// Dispatcher.YieldedFrom. Not calling one of them strands the job's resources:
// the Queue cannot tell "holding and working" from "holding and yielded", so
// nothing else can return them.
//
// AdvanceFrom takes the *job.Job, which Dispatcher.Job returns, because it
// must not act on a later instance registered under the same ID. Finished and
// Yielded take the job ID Run is handed and act on whichever instance is
// registered under it; FinishedJob and YieldedJob take the instance a runner
// resolved with Dispatcher.Job and act only while it is still the one
// registered.
type Runner interface {
	Run(ctx context.Context, id string, state job.State)
}

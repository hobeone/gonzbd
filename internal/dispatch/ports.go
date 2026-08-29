package dispatch

import (
	"context"

	"github.com/hobeone/gonzbd/internal/job"
)

// Residency is how the dispatcher makes a job's manifest available and takes it
// away again. It names no manifest type on purpose: Manifest lives in
// internal/queue until B2.4, and the dispatcher never reads its contents. It
// decides WHEN a job should be resident (D-B8) and delegates WHAT to load.
//
// Hydrate may block on disk I/O. The dispatcher calls it with no lock held.
type Residency interface {
	Hydrate(ctx context.Context, id string) error
	Evict(id string)
}

// Store is the persistence the dispatcher needs, and no more (D-B11): read the
// whole queue once at Start, and write a job's four axes when they move.
// internal/dispatch defines it; B2.2 implements it against SQLite.
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
	ID     string
	Header Header
	State  job.StateView
	Intent job.Intent
}

// Runner starts the work for one job at one state. It must return promptly —
// the dispatcher calls it from the tick goroutine, and a Runner that blocks
// stalls every other job's advance.
//
// The runner reports completion by calling Dispatcher.Finished, and any other
// exit by calling Dispatcher.Yielded. Not calling either strands the job's
// resources: the Queue cannot tell "holding and working" from "holding and
// yielded", so nothing else can return them.
type Runner interface {
	Run(ctx context.Context, id string, state job.State)
}

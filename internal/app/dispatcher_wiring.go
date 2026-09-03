package app

import (
	"context"
	"database/sql"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// Dispatcher returns the application's job dispatcher.
func (app *Application) Dispatcher() *dispatch.Dispatcher {
	return app.dispatcher
}

func (app *Application) lookupJob(id string) (*job.Job, bool) {
	if app.dispatcher != nil {
		return app.dispatcher.Job(id)
	}
	return nil, false
}

// appWorkers satisfies sched.Workers.
type appWorkers struct {
	app *Application
}

func (w *appWorkers) Abort(jobID string) {
	// sched.Workers abort callback. Promptly signals cancellation to workers.
}

type nopDispatchStore struct{}

func (nopDispatchStore) Load(context.Context) ([]dispatch.Persisted, error) { return nil, nil }
func (nopDispatchStore) Save(context.Context, dispatch.Persisted) error     { return nil }
func (nopDispatchStore) Delete(context.Context, string) error               { return nil }

type appCheckpointStore struct {
	db *sql.DB
}

func (s *appCheckpointStore) SaveBatch(ctx context.Context, cps []job.Checkpoint) error {
	return nil
}

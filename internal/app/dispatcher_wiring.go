package app

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// Dispatcher returns the application's job dispatcher.
func (app *Application) Dispatcher() *dispatch.Dispatcher {
	return app.dispatcher
}

// Config returns the application's configuration.
func (app *Application) Config() *config.Config {
	return app.config
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
	if s.db == nil || len(cps) == 0 {
		return nil
	}
	for _, cp := range cps {
		_, err := s.db.ExecContext(ctx,
			`UPDATE dispatch_jobs SET state = ?, next = ?, activity = ?, outcome = ?, assessed = ?, intent = ? WHERE id = ?`,
			cp.State.State, cp.State.Next, cp.State.Activity, cp.State.Outcome, cp.State.Assessed, cp.Intent, cp.ID,
		)
		if err != nil {
			return err
		}
		if cp.Progress != nil {
			const qF = `UPDATE job_files SET complete = ?, fetch_policy = ?, filename = ?, assembled_crc32 = ?, failed_bytes = ?, bytes_downloaded = ? WHERE job_id = ? AND file_index = ?`
			for i := range cp.Progress.NumFiles() {
				complete := 0
				if cp.Progress.FileComplete(i) {
					complete = 1
				}
				fetch := int(cp.Progress.FileFetchPolicy(i))
				if _, err := s.db.ExecContext(ctx, qF,
					complete, fetch, cp.Progress.FileFilename(i), cp.Progress.FileAssembledCRC32(i),
					cp.Progress.FileFailedBytes(i), cp.Progress.FileBytesDownloaded(i),
					cp.ID, i,
				); err != nil {
					return fmt.Errorf("checkpoint: update job_file %s index %d: %w", cp.ID, i, err)
				}
			}
		}
	}
	return nil
}

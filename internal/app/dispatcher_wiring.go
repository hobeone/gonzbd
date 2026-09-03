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

func (w *appWorkers) Abort(jobID string) { //nocover: no-op interface stub
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("checkpoint: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmtJobs, err := tx.PrepareContext(ctx,
		`UPDATE dispatch_jobs SET state = ?, next = ?, activity = ?, outcome = ?, assessed = ?, intent = ?, download_started = ?, download_finished = ?, par2_release_reason = ? WHERE id = ?`,
	)
	if err != nil {
		return fmt.Errorf("checkpoint: prepare dispatch_jobs: %w", err)
	}
	defer func() { _ = stmtJobs.Close() }()

	stmtFiles, err := tx.PrepareContext(ctx,
		`UPDATE job_files SET complete = ?, fetch_policy = ?, filename = ?, assembled_crc32 = ?, failed_bytes = ?, bytes_downloaded = ? WHERE job_id = ? AND file_index = ?`,
	)
	if err != nil {
		return fmt.Errorf("checkpoint: prepare job_files: %w", err)
	}
	defer func() { _ = stmtFiles.Close() }()

	stmtFailed, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO failed_articles (job_id, art_idx) VALUES (?, ?)`,
	)
	if err != nil {
		return fmt.Errorf("checkpoint: prepare failed_articles: %w", err)
	}
	defer func() { _ = stmtFailed.Close() }()

	for _, cp := range cps {
		var downloadStarted, downloadFinished int64
		var par2Reason string
		if cp.Progress != nil {
			if ds := cp.Progress.DownloadStarted(); !ds.IsZero() {
				downloadStarted = ds.Unix()
			}
			if df := cp.Progress.DownloadFinished(); !df.IsZero() {
				downloadFinished = df.Unix()
			}
			par2Reason = cp.Progress.Par2ReleaseReason()
		}

		if _, err := stmtJobs.ExecContext(ctx,
			cp.State.State, cp.State.Next, cp.State.Activity, cp.State.Outcome,
			cp.State.Assessed, cp.Intent, downloadStarted, downloadFinished, par2Reason,
			cp.ID,
		); err != nil {
			return fmt.Errorf("checkpoint: update dispatch_job %s: %w", cp.ID, err)
		}

		if cp.Progress != nil {
			for i := range cp.Progress.NumFiles() {
				complete := 0
				if cp.Progress.FileComplete(i) {
					complete = 1
				}
				fetch := int(cp.Progress.FileFetchPolicy(i))
				if _, err := stmtFiles.ExecContext(ctx,
					complete, fetch, cp.Progress.FileFilename(i), cp.Progress.FileAssembledCRC32(i),
					cp.Progress.FileFailedBytes(i), cp.Progress.FileBytesDownloaded(i),
					cp.ID, i,
				); err != nil {
					return fmt.Errorf("checkpoint: update job_file %s index %d: %w", cp.ID, i, err)
				}
			}

			if cp.Progress.ArticlesFailed() > 0 {
				for artIdx := range cp.Progress.TotalArticles() {
					if cp.Progress.ArticleFailed(artIdx) {
						if _, err := stmtFailed.ExecContext(ctx, cp.ID, artIdx); err != nil {
							return fmt.Errorf("checkpoint: insert failed_article %s index %d: %w", cp.ID, artIdx, err)
						}
					}
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("checkpoint: commit: %w", err)
	}
	return nil
}

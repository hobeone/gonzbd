package app

import (
	"context"
	"errors"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/durability"
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

// Abort calls CancelJob on the downloader pool. CancelJob clears downloader
// tracking records (tryList and inFlight) for the given jobID before yielding
// the job on the dispatcher. Note that CancelJob clears tracking records; it
// does not cancel or drain active NNTP worker goroutines.
func (w *appWorkers) Abort(j *job.Job) {
	if w.app == nil || j == nil {
		return
	}
	jobID := j.ID()
	w.app.mu.Lock()
	dl := w.app.downloader
	disp := w.app.dispatcher
	log := w.app.log
	w.app.mu.Unlock()

	if dl != nil {
		if c, ok := dl.(interface{ CancelJob(string) }); ok {
			c.CancelJob(jobID)
		}
		if wk, ok := dl.(interface{ Wake() }); ok {
			wk.Wake()
		}
	}
	if disp != nil {
		// Yield asynchronously: Abort is invoked inside sched.Queue.mu's lock span
		// during Cancel. Calling disp.YieldedJob directly would deadlock on Queue.mu.
		// If the job was already removed via Dispatcher.Remove or replaced by a new
		// attempt under the same ID, disp.YieldedJob returns an ErrNotFound which is
		// benign and logged at debug level.
		go func() {
			if err := disp.YieldedJob(j); err != nil && log != nil {
				if errors.Is(err, dispatch.ErrNotFound) {
					log.Debug("abort: worker yield completed with notice", "job", jobID, "err", err)
				} else {
					log.Warn("abort: worker yield failed", "job", jobID, "err", err)
				}
			}
		}()
	}
}

type nopDispatchStore struct{}

func (nopDispatchStore) Load(context.Context) ([]dispatch.Persisted, error) { return nil, nil }
func (nopDispatchStore) Save(context.Context, dispatch.Persisted) error     { return nil }
func (nopDispatchStore) Delete(context.Context, string) error               { return nil }

// appCheckpointStore is checkpoint.Store over durability.Store. It only
// translates: durability cannot name job.Checkpoint (internal/job imports it),
// so each checkpoint is mapped here to the store's plain types, and the store
// does the writing. A nil store makes it a no-op, which is the no-history-
// database mode.
type appCheckpointStore struct {
	store *durability.Store
}

func (s *appCheckpointStore) SaveBatch(ctx context.Context, cps []job.Checkpoint) error {
	if s.store == nil || len(cps) == 0 {
		return nil
	}
	batch := make([]durability.JobProgress, 0, len(cps))
	for _, cp := range cps {
		jp := durability.JobProgress{JobID: cp.ID}
		if p := cp.Progress; p != nil {
			jp.Files = make([]durability.FileRow, p.NumFiles())
			for i := range jp.Files {
				jp.Files[i] = durability.FileRow{
					FileIndex:      i,
					Complete:       p.FileComplete(i),
					FetchPolicy:    uint8(p.FileFetchPolicy(i)),
					Filename:       p.FileFilename(i),
					AssembledCRC32: p.FileAssembledCRC32(i),
				}
			}
			if p.ArticlesFailed() > 0 {
				for artIdx := range p.TotalArticles() {
					if p.ArticleFailed(artIdx) {
						jp.FailedArticles = append(jp.FailedArticles, artIdx)
					}
				}
			}
		}
		batch = append(batch, jp)
	}
	return s.store.SaveProgress(ctx, batch)
}

package app

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/hobeone/gonzbd/internal/directunpack"
)

// directUnpackOrchestrator owns the set of in-flight DirectUnpackers and the
// concurrency counter, extracted from Application (#109 Step 3).
//
// It owns its own mutex. Every former access to the unpackers map / active
// counter was a dedicated app.mu critical section touching only this state —
// never co-atomic with app.mu's other guarded fields (downloader,
// downloaderStats, notifyDispatcher). Moving it to a private lock therefore
// preserves each operation's atomicity while decoupling it from the
// downloader-swap lock, so a DirectUnpack status read no longer serialises
// against a downloader reload.
//
// It holds *Application only for read-only, construction-immutable dependencies
// (queue, pipeline, config, log, ctx, emit); those need no locking.
type directUnpackOrchestrator struct {
	app *Application

	mu        sync.Mutex
	unpackers map[string]*directunpack.DirectUnpacker
	active    int
}

func newDirectUnpackOrchestrator(app *Application) *directUnpackOrchestrator {
	return &directUnpackOrchestrator{
		app:       app,
		unpackers: make(map[string]*directunpack.DirectUnpacker),
	}
}

// maybeStart starts (or feeds) a DirectUnpacker for a completed file when the
// job is eligible for extraction-while-downloading.
func (o *directUnpackOrchestrator) maybeStart(fc FileComplete) {
	app := o.app
	snap := app.queue.SnapshotJob(fc.JobID)
	if snap == nil || snap.PostProc {
		return
	}
	// Skip DU for jobs that don't want unpacking (PP < 2) or have a password
	// (DU would fail on the password and fall back anyway).
	if snap.PP < 2 {
		return
	}
	if snap.Password != "" {
		return
	}
	m, mErr := snap.Manifest()
	if mErr != nil || fc.FileIdx < 0 || fc.FileIdx >= m.NumFiles() {
		return
	}
	filename := m.FileSubject(fc.FileIdx)
	setname, vol := directunpack.AnalyzeRarFilename(filename)
	if vol == 0 {
		return // not a RAR volume
	}

	// Resolve the on-disk path from the pipeline's file info cache.
	info, err := app.pipeline.resolveFileInfo(fc.JobID, fc.FileIdx)
	if err != nil {
		app.log.Debug("directunpack: cannot resolve file path",
			"job", fc.JobID, "fileidx", fc.FileIdx, "err", err)
		return
	}

	o.mu.Lock()
	du, exists := o.unpackers[fc.JobID]
	if !exists {
		cfgSnap := app.config.Snapshot()
		downloadDirBase := cfgSnap.General.DownloadDir
		pp := &cfgSnap.PostProc
		limit := pp.DirectUnpackThreads
		flatUnpack := pp.FlatUnpack
		overwriteFiles := pp.OverwriteFiles
		ignoreUnrarDates := pp.IgnoreUnrarDates
		if limit > 0 && o.active >= limit {
			o.mu.Unlock()
			app.log.Debug("directunpack: skipping, concurrency limit reached",
				"job", fc.JobID, "active", o.active, "limit", limit)
			return
		}
		downloadDir := filepath.Join(downloadDirBase, snap.Name)
		du = directunpack.New(
			app.log.With("component", "directunpack", "job", fc.JobID),
			fc.JobID, downloadDir, downloadDir,
			o.buildOpts(flatUnpack, overwriteFiles, ignoreUnrarDates),
		)
		// Provide all filenames so the DU can compute total volume counts.
		allNames := make([]string, m.NumFiles())
		for i := range m.NumFiles() {
			allNames[i] = m.FileSubject(i)
		}
		du.SetAllFilenames(allNames)
		o.unpackers[fc.JobID] = du
		o.active++
	}
	o.mu.Unlock()

	// A file can reach "complete" with some of its articles permanently Failed
	// (the assembler still fires OnFileComplete once every article is resolved
	// — Done or Failed — so the job isn't stuck waiting on data that will never
	// arrive; see internal/assembler's handleFatalArticle). The on-disk RAR
	// volume in that case is the right size but has gaps where the failed
	// articles' bytes should be. DirectUnpack must not report success on such a
	// volume — mark the set corrupt before Add() so extraction aborts (or
	// success is suppressed) instead of silently trusting incomplete data. par2
	// repair will fix it from the recovery blocks; the normal unpack stage
	// re-extracts afterward.
	if hasFailedArticle(m, snap.Progress(), fc.FileIdx) {
		reason := fmt.Sprintf("volume %s had failed/missing download articles", filename)
		du.MarkCorrupt(setname, reason)
		app.log.Warn("directunpack: marking set corrupt, volume incomplete",
			"job", fc.JobID, "set", setname, "file", filename)
	}

	du.Add(app.ctx, filename, info.Path)
}

// buildOpts constructs DirectUnpack options from the given config-derived
// values. The caller reads them (alongside the concurrency limit and download
// dir) from value getters, so the orchestrator lock isn't held across
// two separate config reads.
func (o *directUnpackOrchestrator) buildOpts(flatUnpack, overwriteFiles, ignoreUnrarDates bool) directunpack.Options {
	return directunpack.Options{
		Password:         "", // per-job passwords are pre-checked; DU skips password jobs
		OneFolder:        flatUnpack,
		OverwriteFiles:   overwriteFiles,
		IgnoreUnrarDates: ignoreUnrarDates,
		OnStatusChange: func() {
			o.app.emit(Event{Type: "queue_updated"})
		},
	}
}

// status returns the DirectUnpack status for one job.
func (o *directUnpackOrchestrator) status(jobID string) (directunpack.Status, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	du, ok := o.unpackers[jobID]
	if !ok {
		return directunpack.Status{}, false
	}
	return du.Status(), true
}

// statuses returns a snapshot of every active DirectUnpacker's status, keyed by
// job ID. Takes the orchestrator lock once regardless of job count — used by
// queueList to avoid re-locking per job in the listing hot path (OPT-12).
func (o *directUnpackOrchestrator) statuses() map[string]directunpack.Status {
	o.mu.Lock()
	defer o.mu.Unlock()
	statuses := make(map[string]directunpack.Status, len(o.unpackers))
	for jobID, du := range o.unpackers {
		statuses[jobID] = du.Status()
	}
	return statuses
}

// abortJob aborts and removes the DirectUnpacker for one job (if any). Used
// when a job is removed from the queue.
func (o *directUnpackOrchestrator) abortJob(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if du, ok := o.unpackers[id]; ok {
		du.Abort()
		delete(o.unpackers, id)
		o.active--
	}
}

// abortAll aborts and removes every active DirectUnpacker (shutdown path).
func (o *directUnpackOrchestrator) abortAll() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, du := range o.unpackers {
		du.Abort()
		delete(o.unpackers, id)
		o.active--
	}
}

// collect removes and returns the DirectUnpacker for a job entering
// post-processing (nil if none), decrementing the active count.
func (o *directUnpackOrchestrator) collect(jobID string) *directunpack.DirectUnpacker {
	o.mu.Lock()
	defer o.mu.Unlock()
	du := o.unpackers[jobID]
	if du != nil {
		o.active--
	}
	delete(o.unpackers, jobID)
	return du
}

// inject adds a DirectUnpacker directly. Test-support only (see export_test.go).
func (o *directUnpackOrchestrator) inject(jobID string, du *directunpack.DirectUnpacker) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.unpackers[jobID] = du
}

// setActive overrides the active counter. Test-support only (see export_test.go).
func (o *directUnpackOrchestrator) setActive(n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.active = n
}

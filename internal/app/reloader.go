package app

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"time"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/downloader"
)

// ReloadPostProcOptions applies all hot-applicable postproc settings from pp
// (and the script directory from General) to the running post-processor.
//
// The two heavy stages (unpack, repair) each receive one atomic Apply built by
// the shared config→stage translators (unpackConfigFromPP / repairConfigFromPP),
// which are the single source of truth for that mapping — used identically here
// and at construction (buildStages) so the two paths cannot drift. The remaining
// stages each have a single hot-reloadable setting, applied directly.
//
// pp and scriptDir must be value snapshots taken without holding
// config.Config's lock (this is called from the API's reloadSection after it
// snapshots cfg; sync.RWMutex is not reentrant).
//
// Config-only fields (DirectUnpack, DirectUnpackThreads, EnableUnrar, Enable7zip)
// are not applied here: they are read from config at dispatch time, and the API
// has already written them to config before this runs, so no propagation is
// needed.
func (app *Application) ReloadPostProcOptions(pp config.PostProcConfig, scriptDir string) {
	cmdCfg := cmdutil.CmdConfig{Nice: pp.Nice, Ionice: pp.Ionice}

	// Extra params are validated at config load/mutation (config.Validate), so
	// a parse error here is not expected; warn and fall back to no extra args,
	// mirroring the former SetExtraParams warn-and-continue behaviour. A failed
	// unrar validation is a warning only — the args are still applied.
	extraPar2Args, err := cmdutil.ParseExtraParams(pp.ExtraPar2Params)
	if err != nil {
		app.log.Warn("Failed to parse extra par2 params", "err", err)
		extraPar2Args = nil
	}
	extraUnrarArgs, err := cmdutil.ParseExtraParams(pp.ExtraUnrarParams)
	if err != nil {
		app.log.Warn("Failed to parse extra unrar params", "err", err)
		extraUnrarArgs = nil
	} else if verr := cmdutil.ValidateUnrarParams(extraUnrarArgs); verr != nil {
		app.log.Warn("extra_unrar_params contains non-standard flags", "err", verr)
	}

	// Heavy stages: one atomic Apply each, via the shared translator.
	if app.stages.Unpack != nil {
		app.stages.Unpack.Apply(unpackConfigFromPP(pp, app.probe, cmdCfg, extraUnrarArgs))
		app.stages.Unpack.SetEnabled(pp.EnableUnrar || pp.Enable7zip || pp.EnableFileJoin || pp.EnableTar)
	}
	if app.stages.Repair != nil {
		app.stages.Repair.Apply(repairConfigFromPP(pp, app.probe, cmdCfg, extraPar2Args))
	}

	// One-liner stages: apply their single hot-reloadable setting directly.
	if app.stages.QuickCheck != nil {
		app.stages.QuickCheck.SetEnabled(pp.EnableQuickCheck)
	}
	if app.stages.Par2Cleanup != nil {
		app.stages.Par2Cleanup.SetCleanup(pp.EnableParCleanup)
	}
	if app.stages.Finalize != nil {
		app.stages.Finalize.SetFolderRename(pp.FolderRename)
	}
	if app.stages.Script != nil {
		app.stages.Script.SetScriptCanFail(pp.ScriptCanFail)
		app.stages.Script.SetScriptDir(scriptDir)
	}
	if app.stages.Deobfuscate != nil {
		app.stages.Deobfuscate.SetEnabled(pp.DeobfuscateFilenames)
	}
	if app.stages.SampleCleanup != nil {
		app.stages.SampleCleanup.SetEnabled(pp.IgnoreSamples)
	}
	if app.stages.ExtensionCleanup != nil {
		app.stages.ExtensionCleanup.SetExtensions(pp.CleanupExtensions)
	}
}

// ReloadDownloadOptions applies all hot-applicable download settings from d
// to the running pipeline. Same locking note as ReloadPostProcOptions: d must
// be a value snapshot taken without holding config.Config's lock.
//
// SanitizeOptions are not applied here: they are read from config at
// job-enqueue time, and the API has already written them to config before this
// runs.
func (app *Application) ReloadDownloadOptions(d config.DownloadConfig) {
	app.SetMinFreeSpace(int64(d.MinFreeSpace))
	app.SetMaxArtTries(d.MaxArtTries)
	app.SetMaxArtOpt(d.MaxArtOpt)
	app.SetTopOnly(d.TopOnly)
	app.SetPropagationDelay(d.PropagationDelay)
	if app.queue != nil {
		app.queue.SetSanitizeOptions(d.SanitizeOptions())
		app.queue.SetMaxActiveJobs(d.MaxActiveJobs)
	}
}

// ReloadGeneralOptions applies all hot-applicable general settings from g
// to the running logging handlers. Same locking note as ReloadPostProcOptions:
// g must be a value snapshot taken without holding config.Config's lock.
func (app *Application) ReloadGeneralOptions(g config.GeneralConfig) {
	globalLevel, err := g.ParseLogLevel()
	if err != nil {
		app.log.Error("failed to parse global log level on reload, keeping current", "err", err)
		globalLevel = globalLevelVar.Level()
	}

	compLevels, err := g.ParseLogLevels()
	if err != nil {
		app.log.Error("failed to parse component log levels on reload, keeping current", "err", err)
		componentLevelsMu.RLock()
		compLevels = make(map[string]slog.Level, len(componentLevels))
		maps.Copy(compLevels, componentLevels)
		componentLevelsMu.RUnlock()
	}

	SetLogLevels(globalLevel, compLevels)
}

// ReloadDownloader stops the current downloader and starts a new one with
// the given server configurations. Used when server settings change at runtime.
//
// reloadMu serializes the whole body end-to-end: nothing here calls this
// concurrently with itself internally, but the API layer (modeSetConfig)
// invokes it once per HTTP request with no serialization of its own, so two
// concurrent server-config updates could otherwise interleave their
// Stop/setCompletions/checkpoint/ClearAllEmitted/Start sequences and leave app.downloader
// and app.pipeline's completions source wired to two different downloader
// instances (leaking the loser's goroutines and stalling dispatch on the
// orphaned one). app.mu is only held within that section to snapshot the old
// downloader and, at the end, to swap in the new one — stopping the old
// downloader and building/starting the new one happen without app.mu held,
// so app.mu-guarded reads (Speed, ServerStatus, PauseDownloads, etc.) are
// never blocked on downloader shutdown or startup work. See AGENTS.md
// "never hold a mutex during disk I/O or network calls" and issue #118.
func (app *Application) ReloadDownloader(scs []config.ServerConfig) error {
	app.reloadMu.Lock()
	defer app.reloadMu.Unlock()

	app.mu.Lock()
	if !app.started.Load() || app.stopped.Load() {
		app.mu.Unlock()
		return errors.New("app: not running")
	}
	oldDownloader := app.downloader
	app.mu.Unlock()
	// --- No lock held below this line ---

	_ = oldDownloader.Stop()

	// Quiesce the old downloader's output: setCompletions(nil) returns only
	// once every buffered result has been drained AND written by a pipeline
	// worker.
	//
	// The write half is why this is a quiescence point rather than a
	// hand-off, and it did not always hold. Draining moves results onto a
	// buffered work channel; the pwrite happens later, on a worker. The
	// checkpoint below acks what is on disk, so anything still queued to be
	// written would be cleared as outstanding by ClearAllEmitted and then
	// written immediately afterwards — #390 again, with the checkpoint in
	// place. pipeline.setCompletions now waits for the writes, so this line
	// is what makes the ordering below sound.
	app.pipeline.setCompletions(nil)

	// Ack what the assembler has already written, BEFORE the clear below
	// re-offers it.
	//
	// An article is Emitted from dispatch until a barrier acks it durable, so
	// everything written since the last checkpoint sits in that window —
	// bytes on disk that the queue still calls outstanding. Clearing Emitted
	// without this offers them again, and #390 is what that costs: the
	// re-fetch goes out on the wire, and if it fails terminally against the
	// NEW server set — likeliest precisely when the reload removed the server
	// that had the article — it is acked permanently failed while its bytes
	// are already on disk. markDone then early-returns on the next barrier's
	// durable ack, because done is already set, so the two disagree
	// permanently: a durable_runs row covers the article while a
	// failed_articles row calls it permanently failed. The
	// inflated failedBytes can reach RepairNoCapacity or
	// RepairBeyondCapacity, both Hopeless(), and the Early Health Gate aborts
	// a job whose file was never damaged.
	//
	// context.Background() rather than app.ctx, for the reason Shutdown's
	// checkpoint uses it: a cancellation racing this must not skip the ack
	// and leave the clear below to run anyway, which is the bug rather than a
	// milder version of it.
	//
	// reloadCheckpointTimeout is the BUDGET, not a wall-clock bound on this
	// call — see its declaration. checkpointJob takes the per-job barrier
	// lock before it consults any context, and sync.Mutex is not
	// context-aware, so a job whose barrier is already running elsewhere is
	// waited for however long that takes. reloadMu is held across all of it
	// and stopWorkers acquires reloadMu with no timeout of its own, so a
	// wedged barrier here delays Shutdown by the same amount. Making that
	// bound real needs a cancellable acquisition in checkpointJob, which is
	// its own change — issue #417, which also covers the fact that this
	// checkpoint is best-effort: it reports no status, so the clear below
	// runs whether it acked every job or none.
	//
	// No app.barrier nil check: checkpointAllWithBudget and checkpointJob
	// each already return early on nil, and a third copy here would gate
	// nothing that they do not.
	//
	// The narrower alternative — teach resetForReload to skip an article the
	// writer still holds — is not expressible at the queue layer, which
	// cannot see what the writer is holding. Ordering is the fix.
	cpCtx, cancel := context.WithTimeout(context.Background(), reloadCheckpointTimeout)
	app.checkpointAllShare(cpCtx, reloadCheckpointTimeout)
	cancel()

	// Now it's safe to clear emitted: no more article resolutions arrive from
	// old results, so notifyCh won't be consumed between clear and the new
	// downloader's first dispatch pass.
	app.queue.ClearAllEmitted()

	servers := make([]*downloader.Server, len(scs))
	for i, sc := range scs {
		servers[i] = downloader.NewServer(sc)
	}
	newDownloader := downloader.New(app.queue, servers, app.meter, app.buildDownloaderOptions(), app.log)
	if err := newDownloader.Start(app.ctx); err != nil {
		return err
	}

	app.mu.Lock()
	app.downloader = newDownloader
	app.downloaderStats = newDownloader
	app.mu.Unlock()
	app.pipeline.setCompletions(newDownloader.Completions())
	return nil
}

// SetMinFreeSpace updates the low-disk-space threshold. Thread-safe.
func (app *Application) SetMinFreeSpace(bytes int64) {
	app.config.With(func(c *config.Config) {
		c.Downloads.MinFreeSpace = config.ByteSize(bytes)
	})
	if app.assembler != nil {
		app.assembler.SetMinFreeBytes(bytes)
	}
}

// SetMaxArtTries updates per-article retry limit and related dispatch options.
// Thread-safe; takes effect on the next dispatch pass.
func (app *Application) SetMaxArtTries(v int) {
	app.config.With(func(c *config.Config) {
		c.Downloads.MaxArtTries = v
	})
	app.pushDispatchOptions()
}

// SetMaxArtOpt updates the backup-server retry limit.
func (app *Application) SetMaxArtOpt(v int) {
	app.config.With(func(c *config.Config) {
		c.Downloads.MaxArtOpt = v
	})
	app.pushDispatchOptions()
}

// SetTopOnly controls whether dispatch is restricted to the top-priority server.
func (app *Application) SetTopOnly(v bool) {
	app.config.With(func(c *config.Config) {
		c.Downloads.TopOnly = v
	})
	app.pushDispatchOptions()
}

// SetPropagationDelay updates the delay before new jobs start downloading.
func (app *Application) SetPropagationDelay(minutes int) {
	app.config.With(func(c *config.Config) {
		c.Downloads.PropagationDelay = minutes
	})
	app.pushDispatchOptions()
}

// pushDispatchOptions reads the current mutable dispatch fields under app.mu
// and forwards them to the running downloader. Must not be called while
// holding app.mu.
func (app *Application) pushDispatchOptions() {
	dlCfg := app.config.GetDownloads()
	maxArtTries := dlCfg.MaxArtTries
	maxArtOpt := dlCfg.MaxArtOpt
	topOnly := dlCfg.TopOnly
	propDelay := dlCfg.PropagationDelay
	app.mu.Lock()
	dl := app.downloader
	app.mu.Unlock()
	// --- No lock held below this line ---
	if dl != nil {
		dl.SetDispatchOptions(maxArtTries, maxArtOpt, topOnly, time.Duration(propDelay)*time.Minute)
	}
}

package app

import (
	"errors"
	"log/slog"
	"maps"
	"time"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
)

// ReloadPostProcOptions applies all hot-applicable postproc settings from pp
// (and the script directory from General) to the running post-processor.
// Same idempotency note as other Reload*Options.
//
// pp and scriptDir must be value snapshots taken without holding
// config.Config's lock: several of the Set* calls below acquire that lock
// themselves (e.g. SetEnableFileJoin), so calling this from inside
// cfg.WithRead/With would deadlock (sync.RWMutex is not reentrant).
func (app *Application) ReloadPostProcOptions(pp config.PostProcConfig, scriptDir string) {
	app.SetQuickCheckEnabled(pp.EnableQuickCheck)
	app.SetParCleanup(pp.EnableParCleanup)
	app.SetRarCleanup(pp.EnableRarCleanup)
	app.SetOverwriteFiles(pp.OverwriteFiles)
	app.SetFlatUnpack(pp.FlatUnpack)
	app.SetPermissions(pp.Permissions)
	app.SetFolderRename(pp.FolderRename)
	app.SetScriptCanFail(pp.ScriptCanFail)

	// --- NEW HOT-RELOADS ---
	app.SetUseGoRAR(pp.UseGoRAR)
	app.SetUseGo7z(pp.UseGo7z)
	app.SetUseGoPar2(pp.UseGoPar2)
	app.SetGoRarFallback(pp.GoRarFallback)
	app.SetGo7zFallback(pp.Go7zFallback)
	app.SetGoPar2Fallback(pp.GoPar2Fallback)
	app.SetNiceAndIonice(pp.Nice, pp.Ionice)
	app.SetExternalCommands(pp.Par2Command, pp.UnrarCommand, pp.SevenzCommand)
	app.SetExtraParams(pp.ExtraUnrarParams, pp.ExtraPar2Params)
	app.SetCleanupExtensions(pp.CleanupExtensions)
	app.SetDeobfuscate(pp.DeobfuscateFilenames)
	app.SetIgnoreSamples(pp.IgnoreSamples)
	app.SetScriptDir(scriptDir)
	app.SetUnpackEnabled(pp.EnableUnrar || pp.Enable7zip || pp.EnableFileJoin || pp.EnableTar)
	app.SetPasswordFile(pp.PasswordFile)
	app.SetEnableFileJoin(pp.EnableFileJoin)
	app.SetEnableTar(pp.EnableTar)
	app.SetEnableRecursive(pp.EnableRecursive)
	app.SetDirectUnpack(pp.DirectUnpack)
	app.SetDirectUnpackThreads(pp.DirectUnpackThreads)
	app.SetEnableUnrar(pp.EnableUnrar)
	app.SetEnable7zip(pp.Enable7zip)
	app.SetPar2Turbo(pp.Par2Turbo)
	app.SetIgnoreUnrarDates(pp.IgnoreUnrarDates)
	app.SetStrictSandbox(pp.StrictSandbox)
}

// ReloadDownloadOptions applies all hot-applicable download settings from d
// to the running pipeline. Same locking note as ReloadPostProcOptions: d must
// be a value snapshot taken without holding config.Config's lock.
func (app *Application) ReloadDownloadOptions(d config.DownloadConfig) {
	app.SetSanitizeOptions(d.SanitizeOptions())
	app.SetMinFreeSpace(int64(d.MinFreeSpace))
	app.SetMaxArtTries(d.MaxArtTries)
	app.SetMaxArtOpt(d.MaxArtOpt)
	app.SetTopOnly(d.TopOnly)
	app.SetPropagationDelay(d.PropagationDelay)
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
// Stop/setCompletions/ClearAllEmitted/Start sequences and leave app.downloader
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

	// Wait for the pipeline to drain all buffered results from the old
	// downloader's (now-closed) completions channel. setCompletions
	// blocks until the pipeline's run() loop receives the update, which
	// only happens after the pipeline has finished processing all
	// buffered results and detected the channel close.
	app.pipeline.setCompletions(nil)

	// Now it's safe to clear emitted: no more MarkArticleDone calls
	// from old results, so notifyCh won't be consumed between clear
	// and the new downloader's first dispatch pass.
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

// SetQuickCheckEnabled enables or disables the CRC pre-verify pass at runtime
// without restarting. Takes effect for the next job that enters post-processing.
func (app *Application) SetQuickCheckEnabled(enabled bool) {
	if app.stages.QuickCheck != nil {
		app.stages.QuickCheck.SetEnabled(enabled)
	}
}

// SetParCleanup enables or disables par2 file deletion for future jobs.
// Thread-safe; takes effect immediately without restart.
func (app *Application) SetParCleanup(enabled bool) {
	if app.stages.Par2Cleanup != nil {
		app.stages.Par2Cleanup.SetCleanup(enabled)
	}
}

// SetRarCleanup enables or disables archive file deletion for future jobs.
// Thread-safe; takes effect immediately without restart.
// No-op when no unpack stage is configured (unrar/7z disabled at startup).
func (app *Application) SetRarCleanup(enabled bool) {
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetCleanup(enabled)
	}
}

// SetOverwriteFiles enables or disables overwriting existing files on extraction.
func (app *Application) SetOverwriteFiles(enabled bool) {
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetOverwriteFiles(enabled)
	}
}

// SetFlatUnpack enables or disables flat (directory-ignoring) extraction.
func (app *Application) SetFlatUnpack(enabled bool) {
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetFlatUnpack(enabled)
	}
}

// SetPermissions updates the octal permission string applied after extraction.
func (app *Application) SetPermissions(v string) {
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetPermissions(v)
	}
}

// SetFolderRename enables or disables the _UNPACK_/_FAILED_ prefix behavior.
func (app *Application) SetFolderRename(enabled bool) {
	if app.stages.Finalize != nil {
		app.stages.Finalize.SetFolderRename(enabled)
	}
}

// SetScriptCanFail controls whether non-zero script exit codes fail the job.
func (app *Application) SetScriptCanFail(enabled bool) {
	if app.stages.Script != nil {
		app.stages.Script.SetScriptCanFail(enabled)
	}
}

// SetUseGoRAR enables or disables pure-Go RAR extraction at runtime.
func (app *Application) SetUseGoRAR(v bool) {
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetUseGoRAR(v)
	}
}

// SetUseGo7z enables or disables pure-Go 7-Zip extraction at runtime.
func (app *Application) SetUseGo7z(v bool) {
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetUseGo7z(v)
	}
}

// SetUseGoPar2 enables or disables pure-Go par2 verification/repair at runtime.
func (app *Application) SetUseGoPar2(v bool) {
	if app.stages.Repair != nil {
		app.stages.Repair.SetUseGoPar2(v)
	}
}

// SetGoRarFallback enables or disables fallback to unrar binary.
func (app *Application) SetGoRarFallback(v bool) {
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetGoRarFallback(v)
	}
}

// SetGo7zFallback enables or disables fallback to 7z binary.
func (app *Application) SetGo7zFallback(v bool) {
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetGo7zFallback(v)
	}
}

// SetGoPar2Fallback enables or disables fallback to par2 binary.
func (app *Application) SetGoPar2Fallback(v bool) {
	if app.stages.Repair != nil {
		app.stages.Repair.SetGoPar2Fallback(v)
	}
}

// SetNiceAndIonice updates nice and ionice command prefixes for external tools.
func (app *Application) SetNiceAndIonice(nice, ionice string) {
	if app.stages.Repair != nil {
		app.stages.Repair.SetNiceAndIonice(nice, ionice)
	}
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetNiceAndIonice(nice, ionice)
	}
}

// SetExternalCommands updates the paths to par2, unrar, and 7z binaries.
func (app *Application) SetExternalCommands(par2Cmd, unrarCmd, sevenzCmd string) {
	if app.stages.Repair != nil {
		app.stages.Repair.SetPar2Command(par2Cmd)
	}
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetUnrarCommand(unrarCmd)
		app.stages.Unpack.SetSevenZipCommand(sevenzCmd)
	}
}

// SetExtraParams parses and applies extra command parameters to unrar and par2.
func (app *Application) SetExtraParams(unrarParams, par2Params string) {
	if app.stages.Repair != nil {
		extraPar2Args, err := cmdutil.ParseExtraParams(par2Params)
		if err == nil {
			app.stages.Repair.SetExtraPar2Params(extraPar2Args)
		} else {
			app.log.Warn("Failed to parse extra par2 params", "err", err)
		}
	}
	if app.stages.Unpack != nil {
		extraUnrarArgs, err := cmdutil.ParseExtraParams(unrarParams)
		if err == nil {
			if err := cmdutil.ValidateUnrarParams(extraUnrarArgs); err != nil {
				app.log.Warn("extra_unrar_params contains non-standard flags", "err", err)
			}
			app.stages.Unpack.SetExtraUnrarParams(extraUnrarArgs)
		} else {
			app.log.Warn("Failed to parse extra unrar params", "err", err)
		}
	}
}

// SetCleanupExtensions updates the file extension cleanup list.
func (app *Application) SetCleanupExtensions(exts []string) {
	if app.stages.ExtensionCleanup != nil {
		app.stages.ExtensionCleanup.SetExtensions(exts)
	}
}

// SetDeobfuscate enables or disables filename deobfuscation.
func (app *Application) SetDeobfuscate(enabled bool) {
	if app.stages.Deobfuscate != nil {
		app.stages.Deobfuscate.SetEnabled(enabled)
	}
}

// SetIgnoreSamples enables or disables automatic sample cleanup.
func (app *Application) SetIgnoreSamples(enabled bool) {
	if app.stages.SampleCleanup != nil {
		app.stages.SampleCleanup.SetEnabled(enabled)
	}
}

// SetScriptDir updates the user scripts directory.
func (app *Application) SetScriptDir(dir string) {
	if app.stages.Script != nil {
		app.stages.Script.SetScriptDir(dir)
	}
}

// SetUnpackEnabled enables or disables the unpack stage at runtime.
func (app *Application) SetUnpackEnabled(enabled bool) {
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetEnabled(enabled)
	}
}

// SetPasswordFile updates the unpack password file at runtime.
func (app *Application) SetPasswordFile(v string) {
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetPasswordFile(v)
	}
}

// SetEnableFileJoin enables or disables split file joining at runtime.
func (app *Application) SetEnableFileJoin(v bool) {
	app.config.With(func(c *config.Config) {
		c.PostProc.EnableFileJoin = v
	})
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetEnableFileJoin(v)
	}
}

// SetEnableTar enables or disables plain .tar extraction at runtime.
func (app *Application) SetEnableTar(v bool) {
	app.config.With(func(c *config.Config) {
		c.PostProc.EnableTar = v
	})
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetEnableTar(v)
	}
}

// SetEnableRecursive enables or disables recursive unpacking at runtime.
func (app *Application) SetEnableRecursive(v bool) {
	app.config.With(func(c *config.Config) {
		c.PostProc.EnableRecursive = v
	})
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetEnableRecursive(v)
	}
}

// SetDirectUnpack enables or disables extraction of RAR archives while still downloading.
func (app *Application) SetDirectUnpack(v bool) {
	app.config.With(func(c *config.Config) {
		c.PostProc.DirectUnpack = v
	})
}

// SetDirectUnpackThreads limits the number of concurrent DirectUnpack workers.
func (app *Application) SetDirectUnpackThreads(v int) {
	app.config.With(func(c *config.Config) {
		c.PostProc.DirectUnpackThreads = v
	})
}

// SetEnableUnrar enables or disables standard RAR unpacking at runtime.
func (app *Application) SetEnableUnrar(v bool) {
	app.config.With(func(c *config.Config) {
		c.PostProc.EnableUnrar = v
	})
}

// SetEnable7zip enables or disables standard 7-Zip unpacking at runtime.
func (app *Application) SetEnable7zip(v bool) {
	app.config.With(func(c *config.Config) {
		c.PostProc.Enable7zip = v
	})
}

// SetPar2Turbo updates par2cmdline-turbo settings at runtime. Thread-safe.
func (app *Application) SetPar2Turbo(v bool) {
	app.config.With(func(c *config.Config) {
		c.PostProc.Par2Turbo = v
	})
	if app.stages.Repair != nil {
		app.stages.Repair.SetPar2Turbo(v)
	}
}

// SetIgnoreUnrarDates updates unrar modify timestamps options at runtime. Thread-safe.
func (app *Application) SetIgnoreUnrarDates(v bool) {
	app.config.With(func(c *config.Config) {
		c.PostProc.IgnoreUnrarDates = v
	})
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetIgnoreUnrarDates(v)
	}
}

// SetStrictSandbox updates strict sandboxing option at runtime. Thread-safe.
// v is trusted to already satisfy the Linux-only platform constraint:
// config.PostProcConfig.validate() rejects strict_sandbox=true on any
// unsupported platform before a value can reach here (at config load or via
// Config.Set(), both of which run Validate()), so no platform check is
// repeated at this layer.
func (app *Application) SetStrictSandbox(v bool) {
	app.config.With(func(c *config.Config) {
		c.PostProc.StrictSandbox = v
	})
	if app.stages.Unpack != nil {
		app.stages.Unpack.SetStrictSandbox(v)
	}
}

// SetSanitizeOptions updates the filename sanitization options used for new
// jobs. Thread-safe; takes effect for the next enqueued job.
func (app *Application) SetSanitizeOptions(opts fsutil.SanitizeOptions) {
	app.config.With(func(c *config.Config) {
		c.Downloads.ReplaceIllegalWith = opts.ReplaceIllegalWith
		c.Downloads.ReplaceSpacesWith = opts.ReplaceSpacesWith
		c.Downloads.StripDiacritics = opts.StripDiacritics
		c.Downloads.CleanupList = opts.CleanupList
	})
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
	var maxArtTries, maxArtOpt int
	var topOnly bool
	var propDelay int
	app.config.WithRead(func(c *config.Config) {
		maxArtTries = c.Downloads.MaxArtTries
		maxArtOpt = c.Downloads.MaxArtOpt
		topOnly = c.Downloads.TopOnly
		propDelay = c.Downloads.PropagationDelay
	})
	app.mu.Lock()
	dl := app.downloader
	app.mu.Unlock()
	// --- No lock held below this line ---
	if dl != nil {
		dl.SetDispatchOptions(maxArtTries, maxArtOpt, topOnly, time.Duration(propDelay)*time.Minute)
	}
}

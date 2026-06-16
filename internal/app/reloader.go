package app

import (
	"errors"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/downloader"
)

// ReloadPostProcOptions applies all hot-applicable postproc settings from cfg
// to the running post-processor. Same idempotency note as other Reload*Options.
func (app *Application) ReloadPostProcOptions(cfg *config.Config) {
	app.SetQuickCheckEnabled(cfg.PostProc.EnableQuickCheck)
	app.SetParCleanup(cfg.PostProc.EnableParCleanup)
	app.SetRarCleanup(cfg.PostProc.EnableRarCleanup)
	app.SetOverwriteFiles(cfg.PostProc.OverwriteFiles)
	app.SetFlatUnpack(cfg.PostProc.FlatUnpack)
	app.SetPermissions(cfg.PostProc.Permissions)
	app.SetFolderRename(cfg.PostProc.FolderRename)
	app.SetScriptCanFail(cfg.PostProc.ScriptCanFail)

	// --- NEW HOT-RELOADS ---
	app.SetUseGoRAR(cfg.PostProc.UseGoRAR)
	app.SetUseGo7z(cfg.PostProc.UseGo7z)
	app.SetUseGoPar2(cfg.PostProc.UseGoPar2)
	app.SetGoRarFallback(cfg.PostProc.GoRarFallback)
	app.SetGo7zFallback(cfg.PostProc.Go7zFallback)
	app.SetGoPar2Fallback(cfg.PostProc.GoPar2Fallback)
	app.SetNiceAndIonice(cfg.PostProc.Nice, cfg.PostProc.Ionice)
	app.SetExternalCommands(cfg.PostProc.Par2Command, cfg.PostProc.UnrarCommand, cfg.PostProc.SevenzCommand)
	app.SetExtraParams(cfg.PostProc.ExtraUnrarParams, cfg.PostProc.ExtraPar2Params)
	app.SetCleanupExtensions(cfg.PostProc.CleanupExtensions)
	app.SetDeobfuscate(cfg.PostProc.DeobfuscateFilenames)
	app.SetIgnoreSamples(cfg.PostProc.IgnoreSamples)
	app.SetScriptDir(cfg.General.ScriptDir)
	app.SetUnpackEnabled(cfg.PostProc.EnableUnrar || cfg.PostProc.Enable7zip || cfg.PostProc.EnableFileJoin)
	app.SetPasswordFile(cfg.PostProc.PasswordFile)
	app.SetEnableFileJoin(cfg.PostProc.EnableFileJoin)
	app.SetEnableRecursive(cfg.PostProc.EnableRecursive)
	app.SetDirectUnpack(cfg.PostProc.DirectUnpack)
	app.SetDirectUnpackThreads(cfg.PostProc.DirectUnpackThreads)
	app.SetEnableUnrar(cfg.PostProc.EnableUnrar)
	app.SetEnable7zip(cfg.PostProc.Enable7zip)
	app.SetPar2Turbo(cfg.PostProc.Par2Turbo)
	app.SetIgnoreUnrarDates(cfg.PostProc.IgnoreUnrarDates)
}

// ReloadDownloadOptions applies all hot-applicable download settings from cfg
// to the running pipeline. Same idempotency note as ReloadPostProcOptions.
func (app *Application) ReloadDownloadOptions(cfg *config.Config) {
	app.SetSanitizeOptions(cfg.Downloads.SanitizeOptions())
	app.SetMinFreeSpace(int64(cfg.Downloads.MinFreeSpace))
	app.SetMaxArtTries(cfg.Downloads.MaxArtTries)
	app.SetMaxArtOpt(cfg.Downloads.MaxArtOpt)
	app.SetTopOnly(cfg.Downloads.TopOnly)
	app.SetPropagationDelay(cfg.Downloads.PropagationDelay)
}

// ReloadGeneralOptions applies all hot-applicable general settings from cfg
// to the running logging handlers.
func (app *Application) ReloadGeneralOptions(cfg *config.Config) {
	globalLevel, err := cfg.General.ParseLogLevel()
	if err != nil {
		app.log.Error("failed to parse global log level on reload", "err", err)
		return
	}
	compLevels, err := cfg.General.ParseLogLevels()
	if err != nil {
		app.log.Error("failed to parse component log levels on reload", "err", err)
		return
	}
	SetLogLevels(globalLevel, compLevels)
}

// ReloadDownloader stops the current downloader and starts a new one with
// the given server configurations. Used when server settings change at runtime.
func (app *Application) ReloadDownloader(scs []config.ServerConfig) error {
	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.started.Load() || app.stopped.Load() {
		return errors.New("app: not running")
	}
	_ = app.downloader.Stop()

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
	app.downloader = newDownloader
	app.pipeline.setCompletions(newDownloader.Completions())
	return nil
}

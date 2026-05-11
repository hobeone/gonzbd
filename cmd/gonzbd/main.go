// Command gonzbd runs the GoNZBD daemon. Two run modes:
//
//	--serve          Start the HTTP server (API + web UI) and block
//	                 until SIGINT/SIGTERM. The long-running daemon mode.
//	--nzb <path>     One-shot mode: download a single NZB file and exit.
//	                 Step 4.1 proof-of-life; still useful for smoke tests.
//
// Exactly one of --serve or --nzb must be supplied.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hobeone/gonzbd/internal/api"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/bpsmeter"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dirscanner"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/rss"
	"github.com/hobeone/gonzbd/internal/scheduler"
	"github.com/hobeone/gonzbd/internal/urlgrabber"
	"github.com/hobeone/gonzbd/internal/web"

	// Side-effect imports: register pprof handlers and expvar counters
	// on http.DefaultServeMux (routed via /debug/ in composeRouter).
	_ "net/http/pprof" //nolint:gosec // G108: intentionally exposed on /debug/ behind auth

	_ "github.com/hobeone/gonzbd/internal/telemetry"
)

// Build metadata. Overridden at build time via:
//
//	go build -ldflags="-X main.Version=v0.1.0 -X main.Commit=abc1234 -X main.Date=2026-05-06T14:00:00Z"
var (
	Version = "dev"     // semver tag (e.g. "v0.3.0"), or "dev" for local builds
	Commit  = "unknown" // short git SHA
	Date    = "unknown" // RFC-3339 build timestamp
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "", "path to YAML config file")
	configPathF := flag.String("f", "", "alias for --config")
	nzbPath := flag.String("nzb", "", "one-shot: path to NZB file to download (mutually exclusive with --serve)")
	serve := flag.Bool("serve", false, "run the daemon: HTTP server (API + web UI) blocking until signal")
	listenAddr := flag.String("listen", "", "override the config's host:port listener (serve mode only)")
	downloadDir := flag.String("download-dir", "", "override download-dir (incomplete) from config")
	logLevelsFlag := flag.String("log-levels", "", "comma-separated component=level overrides (e.g. api=warn,nntp=error)")
	pidPath := flag.String("pid", "", "write daemon PID to this path while running (serve mode only)")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	if *configPath == "" {
		*configPath = *configPathF
	}

	if *showVersion {
		fmt.Printf("gonzbd %s (commit %s, built %s, %s)\n", Version, Commit, Date, runtime.Version())
		return
	}

	if *configPath == "" {
		usage()
		os.Exit(2)
	}

	switch {
	case *serve && *nzbPath != "":
		fmt.Fprintln(os.Stderr, "--serve and --nzb are mutually exclusive")
		os.Exit(2)
	case *serve:
		if err := serveMode(*configPath, *listenAddr, *downloadDir, *logLevelsFlag, *pidPath, *verbose); err != nil {
			slog.Error("serve failed", "err", err)
			os.Exit(1)
		}
	case *nzbPath != "":
		if err := run(*configPath, *nzbPath, *downloadDir, *logLevelsFlag, *verbose); err != nil {
			slog.Error("download failed", "err", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  gonzbd --config <path> --serve [--listen host:port] [--download-dir <path>] [--log-levels api=warn,nntp=error] [--pid <path>] [-v]")
	fmt.Fprintln(os.Stderr, "  gonzbd --config <path> --nzb <path> [--download-dir <path>] [--log-levels api=warn] [-v]")
	fmt.Fprintln(os.Stderr, "  gonzbd --version")
	fmt.Fprintln(os.Stderr, "  -f is an alias for --config")
}

// serveMode runs the long-lived daemon: boots the download pipeline, opens
// the history DB, constructs the API server and web handler, composes them
// on a single listener, and blocks until SIGINT/SIGTERM.
func serveMode(configPath, listenOverride, downloadDirOverride, logLevelsOverride, pidPath string, verbose bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Info("config file not found; creating default", "path", configPath)
			cfg, err = config.Default()
			if err != nil {
				return fmt.Errorf("create default config: %w", err)
			}
			if err := cfg.Save(configPath); err != nil {
				return fmt.Errorf("save default config: %w", err)
			}
		} else {
			return fmt.Errorf("load config: %w", err)
		}
	}

	dlDir, adminDir, err := resolveDirs(cfg, downloadDirOverride)
	if err != nil {
		return err
	}

	// Ensure download, complete, and admin directories exist. Creating them
	// early gives the user immediate feedback (at startup) if a path is
	// invalid (e.g., typo, unmounted filesystem) instead of failing silently
	// when the first download tries to write hours later.
	//
	// Log the absolute paths so users can verify where data will land,
	// which is especially useful in Docker where relative paths resolve
	// relative to the working directory inside the container.
	for _, d := range []struct{ name, path string }{
		{"download", dlDir},
		{"complete", cfg.General.CompleteDir},
		{"admin", adminDir},
	} {
		absPath, _ := filepath.Abs(d.path)
		if err := os.MkdirAll(d.path, 0o750); err != nil {
			return fmt.Errorf("create %s dir %s: %w", d.name, absPath, err)
		}
		slog.Info("directory ready", "role", d.name, "path", absPath) // pre-logger setup; slog.Default not yet configured
	}

	// Set up structured logging. The -v CLI flag overrides the config level.
	logLevel, err := cfg.General.ParseLogLevel()
	if err != nil {
		return fmt.Errorf("parse log level: %w", err)
	}
	if verbose {
		logLevel = slog.LevelDebug
	}

	// Per-component log level overrides. CLI flag takes precedence over config.
	compLevels, err := resolveLogLevels(cfg, logLevelsOverride)
	if err != nil {
		return fmt.Errorf("parse log levels: %w", err)
	}

	logFile := ""
	if cfg.General.LogDir != "" {
		logFile = filepath.Join(cfg.General.LogDir, "gonzbd.log")
	}
	logger, logCloser, err := app.Setup(app.LoggingOptions{
		Level:           logLevel,
		LogFile:         logFile,
		ComponentLevels: compLevels,
	})
	if err != nil {
		return fmt.Errorf("setup logging: %w", err)
	}
	defer func() {
		if logCloser != nil {
			_ = logCloser.Close() //nolint:errcheck // close error not actionable at shutdown
		}
	}()
	_ = logger // installed as slog.Default by Setup
	log := slog.Default().With("component", "main")

	log.Info("gonzbd starting",
		"version", Version,
		"commit", Commit,
		"built", Date,
		"go", runtime.Version(),
	)

	// Single-instance lock prevents two daemons from corrupting the same
	// admin dir. Released on every exit path via defer.
	lock, err := app.AcquireLockfile(filepath.Join(adminDir, "gonzbd.lock"))
	if err != nil {
		if errors.Is(err, app.ErrLocked) {
			return fmt.Errorf("another gonzbd instance is running (admin dir %s); aborting", adminDir)
		}
		return fmt.Errorf("acquire lockfile: %w", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			log.Warn("release lockfile", "err", err)
		}
	}()

	if pidPath != "" {
		if err := writePIDFile(pidPath); err != nil {
			return fmt.Errorf("write pid file: %w", err)
		}
		defer func() {
			if err := os.Remove(pidPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Warn("remove pid file", "err", err)
			}
		}()
	}

	histDB, err := history.Open(filepath.Join(adminDir, "history.db"))
	if err != nil {
		return fmt.Errorf("open history db: %w", err)
	}
	defer func() { _ = histDB.Close() }() //nolint:errcheck // daemon shutdown; close error not actionable
	histRepo := history.NewRepository(histDB)

	application, err := app.New(app.Config{
		DownloadDir:     dlDir,
		CompleteDir:     cfg.General.CompleteDir,
		AdminDir:        adminDir,
		WriteCacheBytes: int64(cfg.Downloads.WriteCacheSize),
		Servers:         enabledServers(cfg.Servers),
		Categories:      cfg.Categories,
		Sanitize: fsutil.SanitizeOptions{
			ReplaceIllegalWith: cfg.Downloads.ReplaceIllegalWith,
			ReplaceSpacesWith:  cfg.Downloads.ReplaceSpacesWith,
			StripDiacritics:    cfg.Downloads.StripDiacritics,
			CleanupList:        cfg.Downloads.CleanupList,
		},

		// Download tuning.
		BandwidthMax:     int64(cfg.Downloads.BandwidthMax),
		BandwidthPerc:    int(cfg.Downloads.BandwidthPerc),
		MinFreeSpace:     int64(cfg.Downloads.MinFreeSpace),
		MaxArtTries:      cfg.Downloads.MaxArtTries,
		MaxArtOpt:        cfg.Downloads.MaxArtOpt,
		TopOnly:          cfg.Downloads.TopOnly,
		NoPenalties:      cfg.Downloads.NoPenalties,
		PreCheck:         cfg.Downloads.PreCheck,
		PropagationDelay: cfg.Downloads.PropagationDelay,

		// PostProc pipeline.
		Sorters:              cfg.Sorters,
		ScriptDir:            cfg.General.ScriptDir,
		DeobfuscateFilenames: cfg.PostProc.DeobfuscateFilenames,
		IgnoreSamples:        cfg.PostProc.IgnoreSamples,
		EnableUnrar:          cfg.PostProc.EnableUnrar,
		Enable7zip:           cfg.PostProc.Enable7zip,
		EnableParCleanup:     cfg.PostProc.EnableParCleanup,
		EnableRarCleanup:     cfg.PostProc.EnableRarCleanup,
		Par2Command:          cfg.PostProc.Par2Command,
		Par2Turbo:            cfg.PostProc.Par2Turbo,
		UnrarCommand:         cfg.PostProc.UnrarCommand,
		SevenzCommand:        cfg.PostProc.SevenzCommand,
		IgnoreUnrarDates:     cfg.PostProc.IgnoreUnrarDates,
		OverwriteFiles:       cfg.PostProc.OverwriteFiles,
		FlatUnpack:           cfg.PostProc.FlatUnpack,
		Prefer7zip:           cfg.PostProc.Prefer7zip,

		Version:    Version,
		APIKey:     cfg.General.APIKey,
		ListenAddr: net.JoinHostPort(cfg.General.Host, strconv.Itoa(cfg.General.Port)),
	}, histRepo)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := application.Start(ctx); err != nil {
		return fmt.Errorf("start application: %w", err)
	}
	// Safety net: ensures Shutdown runs even if a startup step after
	// Start() fails (e.g., startScheduler). Shutdown is idempotent;
	// the normal shutdown path also calls it explicitly for ordering.
	defer func() {
		if err := application.Shutdown(); err != nil {
			log.Warn("application shutdown (deferred)", "err", err)
		}
	}()

	// Bandwidth meter. State persists across restarts so lifetime totals
	// aren't reset by a daemon restart.
	meter := bpsmeter.NewMeter(10*time.Second, time.Now)
	meterStatePath := filepath.Join(adminDir, "bpsmeter.json")
	if state, err := bpsmeter.LoadState(meterStatePath); err == nil {
		bpsmeter.Restore(meter, state)
	}

	// Notifier dispatcher. Build from config; sinks are registered
	// based on the [notifications] config section.
	notify := app.BuildNotifier(cfg.Notifications)
	application.SetNotifier(notify)

	// Ingest adapter shared by the dir scanner and URL grabber. Both
	// receive raw NZB bytes and push jobs onto the same queue.
	ingest := &ingestHandler{app: application, logger: slog.Default().With("component", "ingest")}

	// URL grabber. Used both by the RSS scanner's handler and by the API
	// (mode=addurl). One instance is enough; Grabber is safe for
	// concurrent Fetch callers because each call has its own http.Request.
	grabber := urlgrabber.New(urlgrabber.Config{Logger: slog.Default().With("component", "urlgrabber")}, ingest)

	// Directory scanner. Enabled only when DirscanDir is set.
	if err := startDirScanner(ctx, cfg, adminDir, ingest, log); err != nil {
		return err
	}

	// Scheduler. Parsed schedules drive periodic pause/resume/etc.
	if err := startScheduler(ctx, cfg, application.Queue(), application, cancel, log); err != nil {
		return err
	}

	// RSS scanner. Each accepted item is handed to the URL grabber.
	if err := startRSSScanner(ctx, cfg, adminDir, grabber, log); err != nil {
		return err
	}

	apiSrv := api.New(api.Options{
		Version:    Version,
		Commit:     Commit,
		Date:       Date,
		Queue:      application.Queue(),
		History:    histRepo,
		Config:     cfg,
		ConfigPath: configPath,
		Grabber:    grabber,
		App:        application,
	})

	// Inject the WebSocket broadcaster from the API server into the
	// application so it can fire real-time events.
	application.SetEmitter(wsAdapter{apiSrv.EventBroadcaster()})

	// Check for missing dependencies and surface them via logs and UI warnings.
	for _, warning := range app.CheckDependencies() {
		log.Warn(warning)
		apiSrv.AddWarning(warning)
	}

	// Warn when no NNTP servers are configured. The app runs but cannot
	// download until a server is added via the settings UI.
	if len(enabledServers(cfg.Servers)) == 0 {
		const msg = "No news servers configured — add one in Config → Servers to start downloading"
		log.Warn(msg)
		apiSrv.AddWarning(msg)
	}

	listen := listenOverride
	if listen == "" {
		listen = net.JoinHostPort(cfg.General.Host, strconv.Itoa(cfg.General.Port))
	}

	// Build the web UI auth check. If the config has username/password set,
	// require HTTP Basic Auth before serving the SPA or issuing the apikey
	// cookie. Localhost bypass and existing valid apikey cookie are also
	// accepted without a password prompt.
	var authCheck web.AuthCheck
	if cfg.General.Username != "" && cfg.General.Password != "" {
		cfgRef := cfg // capture for closure
		authCheck = func(w http.ResponseWriter, r *http.Request) bool {
			// Accept existing valid apikey cookie (already authenticated).
			if c, err := r.Cookie("gonzbd_apikey"); err == nil && c.Value != "" {
				var apiKey string
				cfgRef.WithRead(func(c *config.Config) { apiKey = c.General.APIKey })
				if c.Value == apiKey {
					return true
				}
			}
			// Check HTTP Basic Auth.
			user, pass, ok := r.BasicAuth()
			if !ok {
				w.Header().Set("WWW-Authenticate", `Basic realm="GoNZBD"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return false
			}
			var wantUser, wantPass string
			cfgRef.WithRead(func(c *config.Config) {
				wantUser = c.General.Username
				wantPass = c.General.Password
			})
			if user != wantUser || pass != wantPass {
				w.Header().Set("WWW-Authenticate", `Basic realm="GoNZBD"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return false
			}
			return true
		}
	}

	webHandler, err := web.Handler(func() string {
		var key string
		cfg.WithRead(func(c *config.Config) { key = c.General.APIKey })
		return key
	}, authCheck)
	if err != nil {
		return fmt.Errorf("web handler: %w", err)
	}
	handler := composeRouter(apiSrv, webHandler)

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	// errCh sized to 2 so both HTTP and HTTPS goroutines can report
	// without blocking each other if both fail simultaneously.
	errCh := make(chan error, 2)
	go func() {
		log.Info("http listener starting", "addr", listen, "api_key_prefix", keyPrefix(cfg.General.APIKey))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// HTTPS listener — started when https_port > 0.
	var httpsSrv *http.Server
	if cfg.General.HTTPSPort > 0 {
		httpsListen := net.JoinHostPort(cfg.General.Host, strconv.Itoa(cfg.General.HTTPSPort))
		certFile := cfg.General.HTTPSCert
		keyFile := cfg.General.HTTPSKey

		// Auto-generate self-signed certificate if the files don't exist.
		if !fileExists(certFile) || !fileExists(keyFile) {
			log.Info("https: cert/key not found, generating self-signed certificate",
				"cert", certFile, "key", keyFile)
			if err := app.WriteSelfSigned(certFile, keyFile); err != nil {
				return fmt.Errorf("generate self-signed cert: %w", err)
			}
			log.Info("https: self-signed certificate written",
				"cert", certFile, "key", keyFile)
		}

		httpsSrv = &http.Server{
			Addr:              httpsListen,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Info("https listener starting", "addr", httpsListen)
			if err := httpsSrv.ListenAndServeTLS(certFile, keyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		log.Error("listener failed", "err", err)
	}

	// Best-effort graceful shutdown. 5s is enough for in-flight API calls
	// without keeping signal handlers trapped if the pipeline is wedged.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	if httpsSrv != nil {
		if err := httpsSrv.Shutdown(shutdownCtx); err != nil {
			log.Warn("https shutdown", "err", err)
		}
	}
	if err := application.Shutdown(); err != nil {
		log.Warn("application shutdown", "err", err)
	}
	if err := bpsmeter.SaveState(meterStatePath, bpsmeter.Capture(meter)); err != nil {
		log.Warn("save bpsmeter state", "err", err)
	}
	return nil
}

// writePIDFile writes the current process PID to path, atomically. The
// caller is expected to remove the file on shutdown.
func writePIDFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir pid parent: %w", err)
	}
	tmp := path + ".tmp"
	data := []byte(strconv.Itoa(os.Getpid()) + "\n")
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // pidfile is world-readable by convention
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best-effort cleanup; rename error takes precedence
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// fileExists returns true if path names a regular file (or symlink to one).
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// startDirScanner wires the watched-directory scanner when cfg.General.DirscanDir
// is set. It's a goroutine that lives for the duration of ctx.
func startDirScanner(ctx context.Context, cfg *config.Config, adminDir string, h *ingestHandler, log *slog.Logger) error {
	if cfg.General.DirscanDir == "" {
		return nil
	}
	store, err := dirscanner.OpenStore(filepath.Join(adminDir, "dirscan.json"))
	if err != nil {
		return fmt.Errorf("open dirscanner store: %w", err)
	}
	interval := time.Duration(cfg.General.DirscanSpeed) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	catFn := func() []string {
		var names []string
		cfg.WithRead(func(c *config.Config) {
			names = make([]string, len(c.Categories))
			for i, cat := range c.Categories {
				names[i] = cat.Name
			}
		})
		return names
	}
	sc := dirscanner.New(cfg.General.DirscanDir, store, h, catFn, slog.Default().With("component", "dirscanner"))
	go func() {
		if err := sc.Run(ctx, interval); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("dirscanner", "err", err)
		}
	}()
	log.Info("dirscanner started", "dir", cfg.General.DirscanDir, "interval", interval)
	return nil
}

// startScheduler parses cfg.Schedules, registers the known actions, and
// launches the scheduler loop. cancel is used by the "shutdown" action
// to trigger the same shutdown path as SIGINT.
func startScheduler(ctx context.Context, cfg *config.Config, q *queue.Queue, application *app.Application, cancel context.CancelFunc, log *slog.Logger) error {
	specs, err := schedulesFromConfig(cfg.Schedules)
	if err != nil {
		return fmt.Errorf("parse schedules: %w", err)
	}
	reg := scheduler.NewRegistry()
	reg.Register("pause", func(_ context.Context, _ string) error { q.PauseAll(); return nil })
	reg.Register("resume", func(_ context.Context, _ string) error { q.ResumeAll(); return nil })
	reg.Register("speedlimit", func(_ context.Context, arg string) error {
		bps, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			log.Warn("scheduler: invalid speedlimit arg", "arg", arg, "err", err)
			return nil // don't fail the scheduler for a bad arg
		}
		application.SetSpeedLimit(bps * 1024) // arg is KB/s
		log.Info("scheduler: speedlimit set", "kbps", bps)
		return nil
	})
	reg.Register("shutdown", func(_ context.Context, _ string) error {
		log.Info("scheduler: shutdown action fired")
		cancel()
		return nil
	})
	sch := scheduler.New(specs, reg, slog.Default().With("component", "scheduler"))
	go func() {
		if err := sch.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("scheduler", "err", err)
		}
	}()
	log.Info("scheduler started", "schedules", len(specs))
	return nil
}

// startRSSScanner builds feeds from config, opens the dedup store, and
// runs a periodic scanner that hands each accepted item to the grabber.
func startRSSScanner(ctx context.Context, cfg *config.Config, adminDir string, g *urlgrabber.Grabber, log *slog.Logger) error {
	feeds, err := feedsFromConfig(cfg.RSS)
	if err != nil {
		return fmt.Errorf("parse rss feeds: %w", err)
	}
	if len(feeds) == 0 {
		return nil
	}
	store, err := rss.OpenStore(filepath.Join(adminDir, "rss-dedup.json"))
	if err != nil {
		return fmt.Errorf("open rss store: %w", err)
	}
	handler := &rssToURLHandler{grabber: g, logger: slog.Default().With("component", "rss")}
	sc := rss.NewScanner(feeds, store, handler, nil, slog.Default().With("component", "rss"))
	go func() {
		if err := sc.Run(ctx, 15*time.Minute); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("rss scanner", "err", err)
		}
	}()
	log.Info("rss scanner started", "feeds", len(feeds))
	return nil
}

// composeRouter produces the outer HTTP handler that routes /api requests
// to the API server, /debug/ to profiling/telemetry handlers, and
// everything else to the web UI handler.
func composeRouter(apiSrv *api.Server, webHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api", apiSrv.Handler())
	mux.Handle("/api/", apiSrv.Handler())

	// Profiling and telemetry — net/http/pprof registers its handlers
	// on http.DefaultServeMux, so route /debug/pprof/ there. expvar
	// registers /debug/vars on DefaultServeMux too.
	mux.Handle("/debug/", http.DefaultServeMux)

	mux.Handle("/", webHandler)
	return mux
}

// keyPrefix returns the first few chars of an API key for debug logs,
// avoiding leaking the full secret to the operator's terminal.
func keyPrefix(key string) string {
	if len(key) < 4 {
		return "<unset>"
	}
	return key[:4] + "..."
}

// resolveDirs computes the effective download and admin directories from
// the config and optional overrides. Separated from serveMode for reuse.
func resolveDirs(cfg *config.Config, downloadDirOverride string) (dlDir, adminDir string, err error) {
	dlDir = cfg.General.DownloadDir
	if downloadDirOverride != "" {
		dlDir = downloadDirOverride
	}
	if dlDir == "" {
		return "", "", fmt.Errorf("download directory is empty (set general.download_dir in config or pass --download-dir)")
	}

	adminDir = cfg.General.AdminDir
	if adminDir == "" {
		adminDir = filepath.Join(dlDir, "admin")
	}
	return dlDir, adminDir, nil
}

// resolveLogLevels merges per-component log levels from the config file
// with any CLI overrides. CLI entries take precedence. The override string
// is comma-separated key=value pairs, e.g. "api=warn,nntp=error".
func resolveLogLevels(cfg *config.Config, cliOverride string) (map[string]slog.Level, error) {
	// Start from config.
	levels, err := cfg.General.ParseLogLevels()
	if err != nil {
		return nil, err
	}

	if cliOverride == "" {
		return levels, nil
	}

	// Parse CLI override and merge.
	if levels == nil {
		levels = make(map[string]slog.Level)
	}
	for entry := range strings.SplitSeq(cliOverride, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --log-levels entry %q (expected component=level)", entry)
		}
		lvl, parseErr := config.ParseLevel(strings.TrimSpace(v))
		if parseErr != nil {
			return nil, fmt.Errorf("invalid --log-levels level for %q: %w", k, parseErr)
		}
		levels[strings.TrimSpace(k)] = lvl
	}
	return levels, nil
}

func run(configPath, nzbPath, downloadDirOverride, logLevelsOverride string, verbose bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Info("config file not found; using defaults", "path", configPath)
			cfg, err = config.Default()
			if err != nil {
				return fmt.Errorf("create default config: %w", err)
			}
		} else {
			return fmt.Errorf("load config: %w", err)
		}
	}

	// One-shot mode: stderr-only logging.
	logLevel := slog.LevelInfo
	if verbose {
		logLevel = slog.LevelDebug
	}

	// Per-component log level overrides.
	compLevels, err := resolveLogLevels(cfg, logLevelsOverride)
	if err != nil {
		return fmt.Errorf("parse log levels: %w", err)
	}

	logger, _, err := app.Setup(app.LoggingOptions{
		Level:           logLevel,
		LogFile:         "", // no file logging for one-shot mode
		ComponentLevels: compLevels,
	})
	if err != nil {
		return fmt.Errorf("setup logging: %w", err)
	}
	_ = logger // installed as slog.Default by Setup
	log := slog.Default().With("component", "main")

	dlDir, adminDir, err := resolveDirs(cfg, downloadDirOverride)
	if err != nil {
		return err
	}

	// Create directories eagerly — fail fast on bad paths.
	for _, d := range []struct{ name, path string }{
		{"download", dlDir},
		{"complete", cfg.General.CompleteDir},
		{"admin", adminDir},
	} {
		absPath, _ := filepath.Abs(d.path)
		if err := os.MkdirAll(d.path, 0o750); err != nil {
			return fmt.Errorf("create %s dir %s: %w", d.name, absPath, err)
		}
		log.Info("directory ready", "role", d.name, "path", absPath)
	}

	// Open history repo (needed for summary at the end)
	db, err := history.Open(filepath.Join(adminDir, "history.db"))
	if err != nil {
		return fmt.Errorf("open history db: %w", err)
	}
	defer db.Close() //nolint:errcheck // best-effort cleanup at exit
	repo := history.NewRepository(db)

	if len(enabledServers(cfg.Servers)) == 0 {
		return fmt.Errorf("no news servers configured — add at least one server to your config file")
	}

	application, err := app.New(app.Config{
		DownloadDir:     dlDir,
		CompleteDir:     cfg.General.CompleteDir,
		AdminDir:        adminDir,
		WriteCacheBytes: int64(cfg.Downloads.WriteCacheSize),
		Servers:         enabledServers(cfg.Servers),
		Categories:      cfg.Categories,
	}, repo)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := application.Start(ctx); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer func() {
		if err := application.Shutdown(); err != nil {
			log.Warn("shutdown", "err", err)
		}
	}()

	job, rawNZB, err := loadJob(nzbPath)
	if err != nil {
		return fmt.Errorf("load NZB: %w", err)
	}
	totalFiles := len(job.Files)
	if totalFiles == 0 {
		return fmt.Errorf("NZB %s contains no usable files", nzbPath)
	}

	if err := application.AddJob(ctx, job, rawNZB, true); err != nil {
		return fmt.Errorf("enqueue job: %w", err)
	}

	start := time.Now()
	log.Info("download started",
		"job", job.Name, "files", totalFiles, "bytes", job.TotalBytes)

	// Wait for the job to reach History (indicates post-processing is complete).
	log.Info("waiting for job to complete", "job", job.Name, "id", job.ID)

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("interrupted: %w", ctx.Err())
		case ppc := <-application.PostProcComplete():
			if ppc.JobID == job.ID {
				goto done
			}
		case <-tick.C:
			// Secondary check: has it already reached history?
			// This covers the case where PostProcComplete fired before we started selecting.
			if h, err := application.GetHistory(ctx, job.ID); err == nil {
				log.Info("job found in history", "job", job.Name, "status", h.Status)
				goto done
			}
		case <-time.After(60 * time.Minute):
			return fmt.Errorf("no completion in 60 minutes; aborting")
		}
	}

done:
	duration := time.Since(start)
	hist, err := application.GetHistory(ctx, job.ID)
	if err != nil {
		return fmt.Errorf("retrieve history for summary: %w", err)
	}

	fmt.Printf("\n--- Download Summary ---\n")
	fmt.Printf("Job:        %s\n", job.Name)
	fmt.Printf("Status:     %s\n", hist.Status)
	if hist.FailMessage != "" {
		fmt.Printf("Error:      %s\n", hist.FailMessage)
	}
	fmt.Printf("Location:   %s\n", hist.Path)
	fmt.Printf("Total Size: %s\n", formatBytes(job.TotalBytes))
	fmt.Printf("Duration:   %v\n", duration.Round(time.Second))

	// Network throughput (average)
	netMBps := float64(job.TotalBytes) / (1024 * 1024) / duration.Seconds()
	fmt.Printf("Avg Network: %.2f MB/s\n", netMBps)

	// Disk performance (estimated by total job time including assembly and post-proc)
	diskMBps := float64(job.TotalBytes) / (1024 * 1024) / duration.Seconds()
	fmt.Printf("Avg Disk:    %.2f MB/s\n", diskMBps)
	fmt.Printf("------------------------\n\n")

	return nil
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func loadJob(path string) (*queue.Job, []byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: user-supplied NZB path is the whole point
	if err != nil {
		return nil, nil, err
	}

	parsed, err := nzb.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: filepath.Base(path)}, fsutil.SanitizeOptions{})
	return job, data, err
}

// enabledServers filters the config's server list to Enable=true entries.
func enabledServers(all []config.ServerConfig) []config.ServerConfig {
	out := make([]config.ServerConfig, 0, len(all))
	for _, s := range all {
		if s.Enable {
			out = append(out, s)
		}
	}
	return out
}

type wsAdapter struct {
	b *api.Broadcaster
}

func (w wsAdapter) Broadcast(e app.Event) {
	w.b.Broadcast(api.Event{
		Type:          e.Type,
		Speed:         e.Speed,
		Remaining:     e.Remaining,
		SpeedLimit:    e.SpeedLimit,
		BandwidthMax:  e.BandwidthMax,
		BandwidthPerc: e.BandwidthPerc,
		NzoID:         e.NzoID,
	})
}

// Command sabnzbd runs the SABnzbd-Go daemon. Two run modes:
//
//      --serve          Start the HTTP server (API + web UI) and block
//                       until SIGINT/SIGTERM. The long-running daemon mode.
//      --nzb <path>     One-shot mode: download a single NZB file and exit.
//                       Step 4.1 proof-of-life; still useful for smoke tests.
//
// Exactly one of --serve or --nzb must be supplied.
package main

import (
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
	"strconv"
	"syscall"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/api"
	"github.com/hobeone/sabnzbd-go/internal/app"
	"github.com/hobeone/sabnzbd-go/internal/bpsmeter"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/dirscanner"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/internal/notifier"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
	"github.com/hobeone/sabnzbd-go/internal/rss"
	"github.com/hobeone/sabnzbd-go/internal/scheduler"
	"github.com/hobeone/sabnzbd-go/internal/urlgrabber"
	"github.com/hobeone/sabnzbd-go/internal/web"
)

// Version is the build version of the sabnzbd binary. Overridden at build
// time via -ldflags="-X main.Version=<value>".
var Version = "0.0.0-dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "", "path to YAML config file")
	configPathF := flag.String("f", "", "alias for --config")
	nzbPath := flag.String("nzb", "", "one-shot: path to NZB file to download (mutually exclusive with --serve)")
	serve := flag.Bool("serve", false, "run the daemon: HTTP server (API + web UI) blocking until signal")
	listenAddr := flag.String("listen", "", "override the config's host:port listener (serve mode only)")
	downloadDir := flag.String("download-dir", "", "override complete-dir from config")
	pidPath := flag.String("pid", "", "write daemon PID to this path while running (serve mode only)")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	if *configPath == "" {
		*configPath = *configPathF
	}

	if *showVersion {
		fmt.Println(Version)
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
		if err := serveMode(*configPath, *listenAddr, *downloadDir, *pidPath, *verbose); err != nil {
			slog.Error("serve failed", "err", err)
			os.Exit(1)
		}
	case *nzbPath != "":
		if err := run(*configPath, *nzbPath, *downloadDir, *verbose); err != nil {
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
	fmt.Fprintln(os.Stderr, "  sabnzbd --config <path> --serve [--listen host:port] [--download-dir <path>] [--pid <path>] [-v]")
	fmt.Fprintln(os.Stderr, "  sabnzbd --config <path> --nzb <path> [--download-dir <path>] [-v]")
	fmt.Fprintln(os.Stderr, "  sabnzbd --version")
	fmt.Fprintln(os.Stderr, "  -f is an alias for --config")
}

// serveMode runs the long-lived daemon: boots the download pipeline, opens
// the history DB, constructs the API server and web handler, composes them
// on a single listener, and blocks until SIGINT/SIGTERM.
func serveMode(configPath, listenOverride, downloadDirOverride, pidPath string, verbose bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	dlDir, adminDir, err := resolveDirs(cfg, downloadDirOverride)
	if err != nil {
		return err
	}

	// Admin dir must exist before we can acquire the lockfile inside it.
	if err := os.MkdirAll(adminDir, 0o750); err != nil {
		return fmt.Errorf("create admin dir %s: %w", adminDir, err)
	}

	// Set up structured logging. The -v CLI flag overrides the config level.
	logLevel, err := cfg.General.ParseLogLevel()
	if err != nil {
		return fmt.Errorf("parse log level: %w", err)
	}
	if verbose {
		logLevel = slog.LevelDebug
	}
	logFile := ""
	if cfg.General.LogDir != "" {
		logFile = filepath.Join(cfg.General.LogDir, "sabnzbd.log")
	}
	logger, logCloser, err := app.Setup(app.LoggingOptions{
		Level:   logLevel,
		LogFile: logFile,
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

	// Single-instance lock prevents two daemons from corrupting the same
	// admin dir. Released on every exit path via defer.
	lock, err := app.AcquireLockfile(filepath.Join(adminDir, "sabnzbd.lock"))
	if err != nil {
		if errors.Is(err, app.ErrLocked) {
			return fmt.Errorf("another sabnzbd instance is running (admin dir %s); aborting", adminDir)
		}
		return fmt.Errorf("acquire lockfile: %w", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			slog.Warn("release lockfile", "err", err)
		}
	}()

	if pidPath != "" {
		if err := writePIDFile(pidPath); err != nil {
			return fmt.Errorf("write pid file: %w", err)
		}
		defer func() {
			if err := os.Remove(pidPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("remove pid file", "err", err)
			}
		}()
	}

	application, err := app.New(app.Config{
		DownloadDir: dlDir,
		AdminDir:    adminDir,
		CacheLimit:  int64(cfg.Downloads.ArticleCacheSize),
		Servers:     enabledServers(cfg.Servers),
	})
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}

	queueStateDir := filepath.Join(adminDir, "queue")

	histDB, err := history.Open(filepath.Join(adminDir, "history.db"))
	if err != nil {
		return fmt.Errorf("open history db: %w", err)
	}
	defer func() { _ = histDB.Close() }() //nolint:errcheck // daemon shutdown; close error not actionable
	histRepo := history.NewRepository(histDB)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := application.Start(ctx); err != nil {
		return fmt.Errorf("start application: %w", err)
	}

	// Bandwidth meter. State persists across restarts so lifetime totals
	// aren't reset by a daemon restart. Quota is not yet wired (no config
	// surface); pass nil into Restore/Capture.
	meter := bpsmeter.NewMeter(10*time.Second, time.Now)
	meterStatePath := filepath.Join(adminDir, "bpsmeter.json")
	if state, err := bpsmeter.LoadState(meterStatePath); err == nil {
		bpsmeter.Restore(meter, nil, state)
	}

	// Notifier dispatcher. Sinks (email/apprise/script) are not yet
	// config-driven; dispatcher stays empty until that config lands.
	_ = notifier.NewDispatcher(slog.Default()) //nolint:errcheck // placeholder wiring for upcoming sinks

	// Ingest adapter shared by the dir scanner and URL grabber. Both
	// receive raw NZB bytes and push jobs onto the same queue.
	ingest := &ingestHandler{queue: application.Queue(), logger: slog.Default()}

	// URL grabber. Used both by the RSS scanner's handler and by the API
	// (mode=addurl). One instance is enough; Grabber is safe for
	// concurrent Fetch callers because each call has its own http.Request.
	grabber := urlgrabber.New(urlgrabber.Config{Logger: slog.Default()}, ingest)

	// Directory scanner. Enabled only when DirscanDir is set.
	if err := startDirScanner(ctx, cfg, adminDir, ingest); err != nil {
		return err
	}

	// Scheduler. Parsed schedules drive periodic pause/resume/etc.
	if err := startScheduler(ctx, cfg, application.Queue(), cancel); err != nil {
		return err
	}

	// RSS scanner. Each accepted item is handed to the URL grabber.
	if err := startRSSScanner(ctx, cfg, adminDir, grabber); err != nil {
		return err
	}

	apiSrv := api.New(api.Options{
		Auth: api.AuthConfig{
			APIKey:          cfg.General.APIKey,
			NZBKey:          cfg.General.NZBKey,
			LocalhostBypass: true,
		},
		Version:    Version,
		Queue:      application.Queue(),
		History:    histRepo,
		Config:     cfg,
		ConfigPath: configPath,
		Grabber:    grabber,
		App:        application,
	})

	listen := listenOverride
	if listen == "" {
		listen = net.JoinHostPort(cfg.General.Host, strconv.Itoa(cfg.General.Port))
	}

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           composeRouter(apiSrv, web.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("http listener starting", "addr", listen, "api_key_prefix", keyPrefix(cfg.General.APIKey))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-errCh:
		slog.Error("http listener failed", "err", err)
	}

	// Best-effort graceful shutdown. 5s is enough for in-flight API calls
	// without keeping signal handlers trapped if the pipeline is wedged.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http shutdown", "err", err)
	}
	if err := application.Shutdown(); err != nil {
		slog.Warn("application shutdown", "err", err)
	}
	if err := application.Queue().Save(queueStateDir); err != nil {
		slog.Warn("save queue state", "err", err)
	}
	if err := bpsmeter.SaveState(meterStatePath, bpsmeter.Capture(meter, nil)); err != nil {
		slog.Warn("save bpsmeter state", "err", err)
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

// startDirScanner wires the watched-directory scanner when cfg.General.DirscanDir
// is set. It's a goroutine that lives for the duration of ctx.
func startDirScanner(ctx context.Context, cfg *config.Config, adminDir string, h *ingestHandler) error {
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
	sc := dirscanner.New(cfg.General.DirscanDir, store, h, slog.Default())
	go func() {
		if err := sc.Run(ctx, interval); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("dirscanner", "err", err)
		}
	}()
	slog.Info("dirscanner started", "dir", cfg.General.DirscanDir, "interval", interval)
	return nil
}

// startScheduler parses cfg.Schedules, registers the known actions, and
// launches the scheduler loop. cancel is used by the "shutdown" action
// to trigger the same shutdown path as SIGINT.
func startScheduler(ctx context.Context, cfg *config.Config, q *queue.Queue, cancel context.CancelFunc) error {
	specs, err := schedulesFromConfig(cfg.Schedules)
	if err != nil {
		return fmt.Errorf("parse schedules: %w", err)
	}
	reg := scheduler.NewRegistry()
	reg.Register("pause", func(_ context.Context, _ string) error { q.PauseAll(); return nil })
	reg.Register("resume", func(_ context.Context, _ string) error { q.ResumeAll(); return nil })
	// speedlimit is logged for now; hooking into a real Limiter requires
	// the Downloader to take a *bpsmeter.Limiter, which is a later wiring step.
	reg.Register("speedlimit", func(_ context.Context, arg string) error {
		slog.Info("scheduler: speedlimit (noop until downloader is wired)", "arg", arg)
		return nil
	})
	reg.Register("shutdown", func(_ context.Context, _ string) error {
		slog.Info("scheduler: shutdown action fired")
		cancel()
		return nil
	})
	sch := scheduler.New(specs, reg, slog.Default())
	go func() {
		if err := sch.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("scheduler", "err", err)
		}
	}()
	slog.Info("scheduler started", "schedules", len(specs))
	return nil
}

// startRSSScanner builds feeds from config, opens the dedup store, and
// runs a periodic scanner that hands each accepted item to the grabber.
func startRSSScanner(ctx context.Context, cfg *config.Config, adminDir string, g *urlgrabber.Grabber) error {
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
	handler := &rssToURLHandler{grabber: g, logger: slog.Default()}
	sc := rss.NewScanner(feeds, store, handler, nil, slog.Default())
	go func() {
		if err := sc.Run(ctx, 15*time.Minute); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("rss scanner", "err", err)
		}
	}()
	slog.Info("rss scanner started", "feeds", len(feeds))
	return nil
}

// composeRouter produces the outer HTTP handler that routes /api requests
// to the API server and everything else to the web UI handler.
func composeRouter(apiSrv *api.Server, webHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api", apiSrv.Handler())
	mux.Handle("/api/", apiSrv.Handler())
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
	dlDir = cfg.General.CompleteDir
	if downloadDirOverride != "" {
		dlDir = downloadDirOverride
	}
	if dlDir == "" {
		return "", "", fmt.Errorf("complete directory is empty (set general.complete_dir in config or pass --download-dir)")
	}

	adminDir = cfg.General.AdminDir
	if adminDir == "" {
		adminDir = filepath.Join(dlDir, "admin")
	}
	return dlDir, adminDir, nil
}

func run(configPath, nzbPath, downloadDirOverride string, verbose bool) error {
	// One-shot mode: stderr-only logging.
	logLevel := slog.LevelInfo
	if verbose {
		logLevel = slog.LevelDebug
	}
	logger, _, err := app.Setup(app.LoggingOptions{
		Level:   logLevel,
		LogFile: "", // no file logging for one-shot mode
	})
	if err != nil {
		return fmt.Errorf("setup logging: %w", err)
	}
	_ = logger // installed as slog.Default by Setup

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	dlDir, adminDir, err := resolveDirs(cfg, downloadDirOverride)
	if err != nil {
		return err
	}

	application, err := app.New(app.Config{
		DownloadDir: dlDir,
		AdminDir:    adminDir,
		CacheLimit:  int64(cfg.Downloads.ArticleCacheSize),
		Servers:     enabledServers(cfg.Servers),
	})
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
			slog.Warn("shutdown", "err", err)
		}
	}()

	job, err := loadJob(nzbPath)
	if err != nil {
		return fmt.Errorf("load NZB: %w", err)
	}
	totalFiles := len(job.Files)
	if totalFiles == 0 {
		return fmt.Errorf("NZB %s contains no usable files", nzbPath)
	}

	if err := application.Queue().Add(job); err != nil {
		return fmt.Errorf("enqueue job: %w", err)
	}

	slog.Info("download started",
		"job", job.Name, "files", totalFiles, "bytes", job.TotalBytes)

	completed := 0
	for completed < totalFiles {
		select {
		case <-ctx.Done():
			return fmt.Errorf("interrupted: %w", ctx.Err())
		case fc := <-application.FileComplete():
			if fc.JobID != job.ID {
				continue
			}
			completed++
			slog.Info("file complete", "fileidx", fc.FileIdx, "progress", fmt.Sprintf("%d/%d", completed, totalFiles))
		case <-time.After(30 * time.Minute):
			return fmt.Errorf("no progress in 30 minutes; aborting")
		}
	}

	slog.Info("download complete", "job", job.Name, "dir", filepath.Join(dlDir, job.Name))
	return nil
}

func loadJob(path string) (*queue.Job, error) {
	f, err := os.Open(path) //nolint:gosec // G304: user-supplied NZB path is the whole point
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // read-only; close error not actionable

	parsed, err := nzb.Parse(f)
	if err != nil {
		return nil, err
	}
	return queue.NewJob(parsed, queue.AddOptions{Filename: filepath.Base(path)})
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

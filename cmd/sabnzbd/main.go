// Command sabnzbd runs the SABnzbd-Go daemon. Two run modes:
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/api"
	"github.com/hobeone/sabnzbd-go/internal/app"
	"github.com/hobeone/sabnzbd-go/internal/bpsmeter"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/dirscanner"
	"github.com/hobeone/sabnzbd-go/internal/fsutil"
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
	downloadDir := flag.String("download-dir", "", "override download-dir (incomplete) from config")
	logAllow := flag.String("log-allow", "", "comma-separated list of components to log (overrides config)")
	logDeny := flag.String("log-deny", "", "comma-separated list of components to suppress (overrides config)")
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
		if err := serveMode(*configPath, *listenAddr, *downloadDir, *logAllow, *logDeny, *pidPath, *verbose); err != nil {
			slog.Error("serve failed", "err", err)
			os.Exit(1)
		}
	case *nzbPath != "":
		if err := run(*configPath, *nzbPath, *downloadDir, *logAllow, *logDeny, *verbose); err != nil {
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
	fmt.Fprintln(os.Stderr, "  sabnzbd --config <path> --serve [--listen host:port] [--download-dir <path>] [--log-allow <list>] [--log-deny <list>] [--pid <path>] [-v]")
	fmt.Fprintln(os.Stderr, "  sabnzbd --config <path> --nzb <path> [--download-dir <path>] [--log-allow <list>] [--log-deny <list>] [-v]")
	fmt.Fprintln(os.Stderr, "  sabnzbd --version")
	fmt.Fprintln(os.Stderr, "  -f is an alias for --config")
}

// serveMode runs the long-lived daemon: boots the download pipeline, opens
// the history DB, constructs the API server and web handler, composes them
// on a single listener, and blocks until SIGINT/SIGTERM.
func serveMode(configPath, listenOverride, downloadDirOverride, logAllowOverride, logDenyOverride, pidPath string, verbose bool) error {
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

	// Log filtering overrides
	allow := cfg.General.LogAllow
	if logAllowOverride != "" {
		allow = strings.Split(logAllowOverride, ",")
	}
	deny := cfg.General.LogDeny
	if logDenyOverride != "" {
		deny = strings.Split(logDenyOverride, ",")
	}

	logFile := ""
	if cfg.General.LogDir != "" {
		logFile = filepath.Join(cfg.General.LogDir, "sabnzbd.log")
	}
	logger, logCloser, err := app.Setup(app.LoggingOptions{
		Level:   logLevel,
		LogFile: logFile,
		Allow:   allow,
		Deny:    deny,
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

	histDB, err := history.Open(filepath.Join(adminDir, "history.db"))
	if err != nil {
		return fmt.Errorf("open history db: %w", err)
	}
	defer func() { _ = histDB.Close() }() //nolint:errcheck // daemon shutdown; close error not actionable
	histRepo := history.NewRepository(histDB)

	application, err := app.New(app.Config{
		DownloadDir: dlDir,
		CompleteDir: cfg.General.CompleteDir,
		AdminDir:    adminDir,
		CacheLimit:  int64(cfg.Downloads.ArticleCacheSize),
		Servers:     enabledServers(cfg.Servers),
		Categories:  cfg.Categories,
		Sanitize: fsutil.SanitizeOptions{
			ReplaceIllegalWith: cfg.Downloads.ReplaceIllegalWith,
			ReplaceSpacesWith:  cfg.Downloads.ReplaceSpacesWith,
			StripDiacritics:    cfg.Downloads.StripDiacritics,
		},
	}, histRepo)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}

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
	_ = notifier.NewDispatcher(slog.Default().With("component", "notifier")) //nolint:errcheck // placeholder wiring for upcoming sinks

	// Ingest adapter shared by the dir scanner and URL grabber. Both
	// receive raw NZB bytes and push jobs onto the same queue.
	ingest := &ingestHandler{app: application, logger: slog.Default().With("component", "ingest")}

	// URL grabber. Used both by the RSS scanner's handler and by the API
	// (mode=addurl). One instance is enough; Grabber is safe for
	// concurrent Fetch callers because each call has its own http.Request.
	grabber := urlgrabber.New(urlgrabber.Config{Logger: slog.Default().With("component", "urlgrabber")}, ingest)

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
		Version:    Version,
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
		slog.Warn(warning)
		apiSrv.AddWarning(warning)
	}

	listen := listenOverride
	if listen == "" {
		listen = net.JoinHostPort(cfg.General.Host, strconv.Itoa(cfg.General.Port))
	}

	webHandler, err := web.Handler(cfg.General.APIKey)
	if err != nil {
		return fmt.Errorf("web handler: %w", err)
	}
	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           composeRouter(apiSrv, webHandler),
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
	sch := scheduler.New(specs, reg, slog.Default().With("component", "scheduler"))
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
	handler := &rssToURLHandler{grabber: g, logger: slog.Default().With("component", "rss")}
	sc := rss.NewScanner(feeds, store, handler, nil, slog.Default().With("component", "rss"))
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

func run(configPath, nzbPath, downloadDirOverride, logAllowOverride, logDenyOverride string, verbose bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// One-shot mode: stderr-only logging.
	logLevel := slog.LevelInfo
	if verbose {
		logLevel = slog.LevelDebug
	}

	// Log filtering overrides
	allow := cfg.General.LogAllow
	if logAllowOverride != "" {
		allow = strings.Split(logAllowOverride, ",")
	}
	deny := cfg.General.LogDeny
	if logDenyOverride != "" {
		deny = strings.Split(logDenyOverride, ",")
	}

	logger, _, err := app.Setup(app.LoggingOptions{
		Level:   logLevel,
		LogFile: "", // no file logging for one-shot mode
		Allow:   allow,
		Deny:    deny,
	})
	if err != nil {
		return fmt.Errorf("setup logging: %w", err)
	}
	_ = logger // installed as slog.Default by Setup

	dlDir, adminDir, err := resolveDirs(cfg, downloadDirOverride)
	if err != nil {
		return err
	}

	// Open history repo (needed for summary at the end)
	db, err := history.Open(filepath.Join(adminDir, "history.db"))
	if err != nil {
		return fmt.Errorf("open history db: %w", err)
	}
	defer db.Close()
	repo := history.NewRepository(db)

	application, err := app.New(app.Config{
		DownloadDir: dlDir,
		CompleteDir: cfg.General.CompleteDir,
		AdminDir:    adminDir,
		CacheLimit:  int64(cfg.Downloads.ArticleCacheSize),
		Servers:     enabledServers(cfg.Servers),
		Categories:  cfg.Categories,
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
			slog.Warn("shutdown", "err", err)
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
	slog.Info("download started",
		"job", job.Name, "files", totalFiles, "bytes", job.TotalBytes)

	// Wait for the job to reach History (indicates post-processing is complete).
	slog.Info("waiting for job to complete", "job", job.Name, "id", job.ID)

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
				slog.Info("job found in history", "job", job.Name, "status", h.Status)
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
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: filepath.Base(path)})
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
		Type:      e.Type,
		Speed:     e.Speed,
		Remaining: e.Remaining,
	})
}

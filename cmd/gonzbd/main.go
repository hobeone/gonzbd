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
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hobeone/gonzbd/internal/api"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/bpsmeter"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dirscanner"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/humanfmt"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/urlgrabber"
	"github.com/hobeone/gonzbd/internal/web"

	// Side-effect import: register expvar counters on http.DefaultServeMux
	// (routed via /debug/ in composeRouter). pprof is intentionally not
	// imported — it has no auth gate and must not be reachable in this
	// binary.
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

// loadOrCreateConfig loads the YAML config at configPath, creating a default
// config in memory if the file doesn't exist yet. persist controls whether
// that generated default is written back to configPath: serve mode persists
// it so the file exists on disk for the operator to edit; one-shot (--nzb)
// mode does not bother, since it isn't expected to be re-run against the
// same path the way the daemon is.
func loadOrCreateConfig(configPath string, persist bool) (*config.Config, error) {
	cfg, err := config.Load(configPath)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if persist {
		slog.Info("config file not found; creating default", "path", configPath)
	} else {
		slog.Info("config file not found; using defaults", "path", configPath)
	}
	cfg, err = config.Default()
	if err != nil {
		return nil, fmt.Errorf("create default config: %w", err)
	}
	if persist {
		if err := cfg.Save(configPath); err != nil {
			return nil, fmt.Errorf("save default config: %w", err)
		}
	}
	return cfg, nil
}

// serveMode runs the long-lived daemon: boots the download pipeline, opens
// the history DB, constructs the API server and web handler, composes them
// on a single listener, and blocks until SIGINT/SIGTERM.
func serveMode(configPath, listenOverride, downloadDirOverride, logLevelsOverride, pidPath string, verbose bool) error {
	cfg, err := loadOrCreateConfig(configPath, true)
	if err != nil {
		return err
	}

	dlDir, adminDir, err := resolveDirs(cfg, downloadDirOverride)
	if err != nil {
		return err
	}

	// Ensure download, complete, and admin directories exist. Creating them
	// early gives the user immediate feedback (at startup) if a path is
	// invalid (e.g., typo, unmounted filesystem) instead of failing silently
	// when the first download tries to write hours later. Logged via
	// slog.Default() directly: this runs before Setup() installs the
	// configured logger, so it's the pre-logger-setup default handler.
	if err := ensureRuntimeDirs(cfg, dlDir, adminDir, slog.Default()); err != nil {
		return err
	}

	// Set up structured logging. The -v CLI flag overrides the config level.
	logFile := ""
	if cfg.General.LogDir != "" {
		logFile = filepath.Join(cfg.General.LogDir, "gonzbd.log")
	}
	logCloser, err := configureLogging(cfg, logLevelsOverride, verbose, true, logFile)
	if err != nil {
		return err
	}
	defer func() {
		if logCloser != nil {
			_ = logCloser.Close() //nolint:errcheck // close error not actionable at shutdown
		}
	}()
	log := slog.Default().With("component", "main")

	log.Info("gonzbd starting",
		"version", Version,
		"commit", Commit,
		"built", Date,
		"go", runtime.Version(),
	)
	logBuildDeps(log)

	// Single-instance lock prevents two daemons from corrupting the same
	// admin dir, plus an optional PID file. Both released on every exit
	// path via the returned cleanup func.
	releaseLockAndPID, err := acquireLockAndPID(adminDir, pidPath, log)
	if err != nil {
		return err
	}
	defer releaseLockAndPID()

	histDB, err := history.Open(context.Background(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		return fmt.Errorf("open history db: %w", err)
	}
	defer func() { _ = histDB.Close() }() //nolint:errcheck // daemon shutdown; close error not actionable
	histRepo := history.NewRepository(histDB)

	events := api.NewBroadcaster(log.With("component", "api"))
	application, err := app.New(cfg, histRepo, app.WithVersion(Version), app.WithEventEmitter(wsAdapter{events}))
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
		meter.Restore(state)
	}

	// Notifier dispatcher. Build from config; sinks are registered
	// based on the [notifications] config section.
	notify := app.BuildNotifier(cfg.Notifications)
	application.SetNotifier(notify)

	// Ingest adapter shared by the dir scanner and URL grabber. Both
	// receive raw NZB bytes and push jobs onto the same queue.
	ingest := &ingestHandler{app: application, config: cfg, logger: slog.Default().With("component", "ingest")}

	// URL grabber. Used by the API (mode=addurl). One instance is enough;
	// Grabber is safe for concurrent Fetch callers because each call has its own http.Request.
	grabber := urlgrabber.New(urlgrabber.Config{Logger: slog.Default().With("component", "urlgrabber")}, ingest)

	// Directory scanner. Enabled only when DirscanDir is set.
	if err := startDirScanner(ctx, cfg, adminDir, ingest, log); err != nil {
		return err
	}

	apiSrv := buildAPIServer(cfg, configPath, application, histRepo, grabber, cancel, log, events)

	// The web SPA cookie carries the API server's ephemeral session key,
	// not the permanent General.APIKey — see AuthConfig.SessionKey.
	//
	// trustedFn reads the live config on each request so the admin session
	// cookie is only issued to trusted sources (loopback or a configured
	// local_range). This is the SEC-1 issuance-side gate; the API auth
	// middleware enforces the matching acceptance-side gate.
	trustedFn := func(r *http.Request) bool {
		gen := cfg.GetGeneral()
		ranges, _ := config.ParseLocalRanges(gen.LocalRanges)
		fh := config.ForwardedHeaders{
			XForwardedFor: r.Header.Get("X-Forwarded-For"),
			Forwarded:     r.Header.Get("Forwarded"),
			XRealIP:       r.Header.Get("X-Real-IP"),
		}
		trusted, reason := config.IsTrustedRemote(r.RemoteAddr, fh, ranges, gen.VerifyXFFHeader, gen.TrustedForwardHeader)
		if !trusted {
			log.Warn("refused session cookie / /debug/ access: source not trusted", "remote_addr", r.RemoteAddr, "reason", reason)
		}
		return trusted
	}
	webHandler, err := web.Handler(apiSrv.SessionKey, trustedFn)
	if err != nil {
		return fmt.Errorf("web handler: %w", err)
	}
	scriptHashes, err := web.IndexScriptHashes()
	if err != nil {
		return fmt.Errorf("web script hashes: %w", err)
	}
	httpHandler := composeRouter(apiSrv, webHandler, false, scriptHashes, trustedFn, log)
	httpsHandler := composeRouter(apiSrv, webHandler, true, scriptHashes, trustedFn, log)

	httpSrv, httpsSrv := newServers(cfg, httpHandler, httpsHandler, log)
	if listenOverride != "" {
		httpSrv.Addr = listenOverride
	}

	// Warn when an *effective* listener address is non-loopback but no
	// local_ranges are configured: the web UI will refuse to hand out a
	// session cookie to remote/proxied clients, so it will appear "logged
	// out". Checked against httpSrv.Addr (which reflects listenOverride,
	// not just the config file's general.host) and, when enabled,
	// httpsSrv.Addr — --listen only overrides the HTTP listener, so a
	// config-file-only check would miss a non-loopback --listen override.
	// API-key auth still works from anywhere regardless.
	for _, warn := range nonLoopbackWarnings(httpSrv.Addr, httpsSrv.Addr, cfg.General.HTTPSPort > 0, cfg.General.LocalRanges) {
		log.Warn(warn)
		apiSrv.AddWarning(warn)
	}
	errCh, httpsSrv, err := startListeners(httpSrv, httpsSrv, cfg, log)
	if err != nil {
		return err
	}

	waitErr := awaitShutdownSignal(ctx, errCh, log)
	shutdownServeMode(httpSrv, httpsSrv, application, meterStatePath, meter, log)
	return waitErr
}

// awaitShutdownSignal blocks until either ctx is done (SIGINT/SIGTERM, via
// signal.NotifyContext) or a listener goroutine reports a failure on errCh,
// logging which of the two occurred. Returns nil for a normal shutdown
// signal, or the listener's error so the caller can complete shutdown and
// still exit non-zero instead of silently swallowing a bind/runtime
// listener failure.
func awaitShutdownSignal(ctx context.Context, errCh <-chan error, log *slog.Logger) error {
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
		return nil
	case err := <-errCh:
		log.Error("listener failed", "err", err)
		return err
	}
}

// shutdownServeMode performs serveMode's best-effort graceful shutdown:
// stop both HTTP(S) listeners concurrently, each with its own 5s timeout
// budget (enough for in-flight API calls without keeping signal handlers
// trapped if the pipeline is wedged), stop the application, and persist the
// bandwidth meter's lifetime totals. Each step is independent and
// best-effort; a failure in one does not skip the rest.
//
// The two listeners intentionally get independent contexts rather than one
// shared context passed to both Shutdown calls: a shared context lets a
// slow-draining HTTP listener consume most of the deadline before HTTPS's
// Shutdown call even starts, leaving HTTPS too little of its own budget.
// This isn't covered by a timing-based regression test — http.Server.Shutdown
// is a passive wait keyed off absolute deadlines and each listener's own
// (already in-flight, request-driven) completion time, not off when Shutdown
// is invoked, so a black-box test comparing sequential-shared vs
// concurrent-independent invocation can't observe a difference in outcome or
// wall-clock time between the two (verified empirically before deciding not
// to chase this further). The two independent context.WithTimeout calls
// below are the property to check by reading the code.
func shutdownServeMode(httpSrv, httpsSrv *http.Server, application *app.Application, meterStatePath string, meter *bpsmeter.Meter, log *slog.Logger) {
	const shutdownTimeout = 5 * time.Second
	var wg sync.WaitGroup
	if httpSrv != nil {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			if err := httpSrv.Shutdown(ctx); err != nil {
				log.Warn("http shutdown", "err", err)
			}
		})
	}
	if httpsSrv != nil {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			if err := httpsSrv.Shutdown(ctx); err != nil {
				log.Warn("https shutdown", "err", err)
			}
		})
	}
	wg.Wait()

	if application != nil {
		if err := application.Shutdown(); err != nil {
			log.Warn("application shutdown", "err", err)
		}
	}
	if meter != nil && meterStatePath != "" {
		if err := bpsmeter.SaveState(meterStatePath, meter.Capture()); err != nil {
			log.Warn("save bpsmeter state", "err", err)
		}
	}
}

// acquireLockAndPID takes the single-instance admin-dir lockfile (preventing
// two daemons from corrupting the same admin dir) and, if pidPath is set,
// writes a PID file. On success it returns a cleanup func that releases both
// resources in reverse order (PID file first, then lock — matching the
// original defer ordering) — the caller should `defer` it. On failure to
// write the PID file, the already-acquired lock is released before
// returning the error, since no cleanup func is handed back in that case.
func acquireLockAndPID(adminDir, pidPath string, log *slog.Logger) (cleanup func(), err error) {
	lock, err := app.AcquireLockfile(filepath.Join(adminDir, "gonzbd.lock"))
	if err != nil {
		if errors.Is(err, app.ErrLocked) {
			return nil, fmt.Errorf("another gonzbd instance is running (admin dir %s); aborting", adminDir)
		}
		return nil, fmt.Errorf("acquire lockfile: %w", err)
	}
	releaseLock := func() {
		if err := lock.Release(); err != nil {
			log.Warn("release lockfile", "err", err)
		}
	}

	if pidPath == "" {
		return releaseLock, nil
	}

	if err := writePIDFile(pidPath); err != nil {
		releaseLock()
		return nil, fmt.Errorf("write pid file: %w", err)
	}
	return func() {
		if err := os.Remove(pidPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("remove pid file", "err", err)
		}
		releaseLock()
	}, nil
}

// writePIDFile writes the current process PID to path, atomically. The
// caller is expected to remove the file on shutdown.
func writePIDFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir pid parent: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".pid-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	data := []byte(strconv.Itoa(os.Getpid()) + "\n")
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// fileExists returns true if path names a regular file (or symlink to one).
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ensureSelfSignedCert generates a self-signed certificate/key pair at
// certFile/keyFile if either is missing. Called before starting the HTTPS
// listener so operators get a working (if browser-untrusted) HTTPS endpoint
// without manually provisioning a certificate.
func ensureSelfSignedCert(certFile, keyFile string, log *slog.Logger) error {
	if fileExists(certFile) && fileExists(keyFile) {
		return nil
	}
	log.Info("https: cert/key not found, generating self-signed certificate",
		"cert", certFile, "key", keyFile)
	if err := app.WriteSelfSigned(certFile, keyFile); err != nil {
		return fmt.Errorf("generate self-signed cert: %w", err)
	}
	log.Info("https: self-signed certificate written",
		"cert", certFile, "key", keyFile)
	return nil
}

// buildAPIServer constructs the API server, wires its WebSocket broadcaster
// into the application's event emitter, and surfaces startup warnings
// (missing external dependencies, no NNTP servers configured) through both
// the log and the API server's in-UI warning list.
func buildAPIServer(cfg *config.Config, configPath string, application *app.Application, histRepo *history.Repository, grabber *urlgrabber.Grabber, cancel context.CancelFunc, log *slog.Logger, events *api.Broadcaster) *api.Server {
	apiSrv := api.New(api.Options{
		Version:      Version,
		Commit:       Commit,
		Date:         Date,
		Queue:        application.Queue(),
		History:      histRepo,
		Config:       cfg,
		ConfigPath:   configPath,
		Grabber:      grabber,
		App:          application,
		ShutdownFunc: cancel,
		Logger:       log,
		Broadcaster:  events,
	})

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

	return apiSrv
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
		cats := cfg.GetCategories()
		names := make([]string, len(cats))
		for i, cat := range cats {
			names[i] = cat.Name
		}
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

// startListeners — when cfg.General.HTTPSPort > 0 — first auto-provisions a
// self-signed cert (via ensureSelfSignedCert), failing fast before either
// listener starts if that fails. It then starts the HTTP listener
// goroutine, and, when HTTPS is enabled, the HTTPS listener goroutine too.
// Both goroutines report a listener failure (anything but the expected
// http.ErrServerClosed on graceful Shutdown) onto the returned errCh, sized
// to 2 so neither goroutine blocks if both fail simultaneously. When HTTPS
// is disabled, the returned *http.Server is nil — the original httpsSrv
// (from newServers) must not be used past this point in that case, since it
// was never started; the caller should reassign its httpsSrv variable to
// this return value.
func startListeners(httpSrv, httpsSrv *http.Server, cfg *config.Config, log *slog.Logger) (chan error, *http.Server, error) {
	var certFile, keyFile string
	if cfg.General.HTTPSPort > 0 {
		certFile = cfg.General.HTTPSCert
		keyFile = cfg.General.HTTPSKey
		if err := ensureSelfSignedCert(certFile, keyFile, log); err != nil {
			return nil, nil, err
		}
	} else {
		httpsSrv = nil
	}

	errCh := make(chan error, 2)
	go func() {
		log.Info("http listener starting", "addr", httpSrv.Addr, "api_key_prefix", keyPrefix(cfg.General.APIKey))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	if httpsSrv == nil {
		return errCh, nil, nil
	}

	go func() {
		log.Info("https listener starting", "addr", httpsSrv.Addr)
		if err := httpsSrv.ListenAndServeTLS(certFile, keyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	return errCh, httpsSrv, nil
}

// newServers constructs the HTTP and HTTPS servers with the configured addresses
// and handlers.
func newServers(cfg *config.Config, httpHandler, httpsHandler http.Handler, log *slog.Logger) (httpSrv, httpsSrv *http.Server) {
	errLog := slog.NewLogLogger(log.Handler(), slog.LevelError)
	httpAddr := net.JoinHostPort(cfg.General.Host, strconv.Itoa(cfg.General.Port))
	httpSrv = &http.Server{
		Addr:              httpAddr,
		Handler:           httpHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          errLog,
	}

	httpsAddr := net.JoinHostPort(cfg.General.Host, strconv.Itoa(cfg.General.HTTPSPort))
	httpsSrv = &http.Server{
		Addr:              httpsAddr,
		Handler:           httpsHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          errLog,
	}

	return httpSrv, httpsSrv
}

// composeRouter produces the outer HTTP handler that routes /api requests
// to the API server, /debug/ to profiling/telemetry handlers (gated to
// trusted callers only — SEC-4), and everything else to the web UI handler.
//
// apiSrv.Handler() logs its own requests internally (internal/api's
// loggingMiddleware), so it is mounted as-is here. The web/static-asset
// handler and /debug/ have no logging of their own, so each is wrapped
// individually in requestLoggingMiddleware — wrapping the whole mux after
// composition would double-log every /api request.
func composeRouter(apiSrv *api.Server, webHandler http.Handler, isHTTPS bool, scriptHashes []string, trustedFn func(*http.Request) bool, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api", apiSrv.Handler())
	mux.Handle("/api/", apiSrv.Handler())

	// Telemetry — expvar registers /debug/vars on http.DefaultServeMux, so
	// route /debug/ there, but only for callers the SEC-1 trust check
	// already treats as loopback/local-range (same predicate that gates
	// the admin session cookie). expvar has no per-request auth of its
	// own and leaks os.Args (cmdline) + memstats, so an unauthenticated
	// remote caller must never reach it. pprof is not imported by this
	// binary, so no profiling endpoints are reachable regardless.
	debugHandler := trustGate(http.DefaultServeMux, trustedFn)
	mux.Handle("/debug/", requestLoggingMiddleware(log.With("component", "debug"), "debug", debugHandler))

	mux.Handle("/", requestLoggingMiddleware(log.With("component", "web"), "web", webHandler))
	return securityHeadersHandler(mux, isHTTPS, contentSecurityPolicy(scriptHashes))
}

// routerStatusWriter wraps ResponseWriter to capture the status code for
// requestLoggingMiddleware. Mirrors internal/api's statusWriter; kept
// separate since it's a different package with no shared logging
// abstraction, and this one doesn't need the Hijacker passthrough that
// internal/api's WebSocket route requires.
type routerStatusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *routerStatusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *routerStatusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// requestLoggingMiddleware logs exactly one line per request handled by
// next, at a level derived from the final status code (5xx Error, 4xx
// Warn, else Info) — the same shape as internal/api's per-request log
// line, for handlers (static assets, /debug/) that have no logging of
// their own.
func requestLoggingMiddleware(log *slog.Logger, message string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &routerStatusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)

		level := slog.LevelInfo
		switch {
		case sw.status >= http.StatusInternalServerError:
			level = slog.LevelError
		case sw.status >= http.StatusBadRequest:
			level = slog.LevelWarn
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		log.Log(r.Context(), level, message,
			"remote_ip", host,
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start),
		)
	})
}

// trustGate wraps h so it only serves requests from callers trustedFn
// approves (loopback, or a configured general.local_ranges entry); all
// other callers get 404 (not 403, to avoid confirming the path exists).
func trustGate(h http.Handler, trustedFn func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if trustedFn == nil || !trustedFn(r) {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// contentSecurityPolicy builds the CSP header value. scriptHashes are the
// 'sha256-...' source expressions for the SPA's inline <script> tags (see
// web.IndexScriptHashes) — computed at startup from the actual embedded
// build output rather than hardcoded, so a SvelteKit upgrade that changes
// the bootstrap script's content can't silently desync the policy from
// what's served. Google Fonts domains are allowlisted because the SPA
// loads Roboto and Material Symbols directly from fonts.googleapis.com /
// fonts.gstatic.com rather than self-hosting them.
func contentSecurityPolicy(scriptHashes []string) string {
	var scriptSrc strings.Builder
	scriptSrc.WriteString("script-src 'self'")
	for _, h := range scriptHashes {
		scriptSrc.WriteString(" " + h)
	}
	return scriptSrc.String() +
		"; default-src 'self'" +
		"; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com" +
		"; font-src 'self' https://fonts.gstatic.com" +
		"; img-src 'self' data:" +
		"; connect-src 'self' ws: wss:" +
		"; frame-ancestors 'none';"
}

func securityHeadersHandler(next http.Handler, isHTTPS bool, csp string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", csp)

		if isHTTPS {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		}
		next.ServeHTTP(w, r)
	})
}

// keyPrefix returns the first few chars of an API key for debug logs,
// avoiding leaking the full secret to the operator's terminal.
func keyPrefix(key string) string {
	if len(key) < 4 {
		return "<unset>"
	}
	return key[:4] + "..."
}

// nonLoopbackWithoutLocalRanges returns a warning message when the daemon
// binds to a non-loopback address but no local_ranges are configured. In
// that state the SEC-1 trust gate refuses to issue the web session cookie to
// remote/proxied clients (loopback-only default), so the UI appears logged
// out for them. Returns "" when the configuration is safe (loopback bind or
// local_ranges set). API-key/NZB-key auth is unaffected in all cases.
func nonLoopbackWithoutLocalRanges(host string, localRanges []string) string {
	if len(localRanges) > 0 {
		return ""
	}
	host = strings.TrimSpace(host)
	if host == "" || strings.EqualFold(host, "localhost") {
		return ""
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsLoopback() {
		return ""
	}
	return fmt.Sprintf("general.host is %q (non-loopback) but general.local_ranges is empty: "+
		"the web UI will only issue its session cookie to loopback clients, so it will appear logged out "+
		"for remote/proxied clients. Set general.local_ranges to your reverse-proxy/LAN/Docker CIDR "+
		"(API-key auth is unaffected).", host)
}

// listenerHost extracts the host portion of a "host:port" listener address
// (e.g. an *http.Server.Addr, possibly overridden by --listen). Falls back
// to the whole string if it doesn't parse as host:port, which
// nonLoopbackWithoutLocalRanges then treats conservatively as non-loopback.
func listenerHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" {
		// A blank host (e.g. ":4289", as produced by "--listen :4289" or
		// any host:port string with no host component) means "listen on
		// all interfaces" — net.Listen resolves it to the wildcard address
		// ([::] / 0.0.0.0), not loopback. Normalize to an explicit
		// non-loopback marker: nonLoopbackWithoutLocalRanges's empty-string
		// case means "no host configured" (matching config.GeneralConfig's
		// validated, always-non-empty general.host), which is the wrong
		// reading for a wildcard bind address.
		return "0.0.0.0"
	}
	return host
}

// nonLoopbackWarnings computes the SEC-1 non-loopback warning for each
// distinct effective listener host. httpAddr is the HTTP listener's
// *http.Server.Addr (reflecting any --listen override); httpsAddr is only
// evaluated when httpsEnabled (https_port > 0) — HTTPS always binds
// General.Host and is unaffected by --listen, but is included here so a
// non-loopback general.host still warns even when --listen narrows the HTTP
// listener to loopback. Deduplicates identical hosts so binding both
// listeners to the same address doesn't produce two warnings.
func nonLoopbackWarnings(httpAddr, httpsAddr string, httpsEnabled bool, localRanges []string) []string {
	addrs := []string{httpAddr}
	if httpsEnabled {
		addrs = append(addrs, httpsAddr)
	}
	seen := make(map[string]bool, len(addrs))
	var warnings []string
	for _, addr := range addrs {
		host := listenerHost(addr)
		if seen[host] {
			continue
		}
		seen[host] = true
		if warn := nonLoopbackWithoutLocalRanges(host, localRanges); warn != "" {
			warnings = append(warnings, warn)
		}
	}
	return warnings
}

// ensureRuntimeDirs creates the download, complete, admin, and (if
// configured) watch directories, logging each absolute path so users can
// verify where data will land — especially useful in Docker where relative
// paths resolve relative to the working directory inside the container.
// Shared by serveMode (called with slog.Default(), before the configured
// logger is installed) and run (called with the already-configured logger).
func ensureRuntimeDirs(cfg *config.Config, dlDir, adminDir string, log *slog.Logger) error {
	dirs := []struct{ name, path string }{
		{"download", dlDir},
		{"complete", cfg.General.CompleteDir},
		{"admin", adminDir},
	}
	if cfg.General.DirscanDir != "" {
		dirs = append(dirs, struct{ name, path string }{"watch", cfg.General.DirscanDir})
	}
	for _, d := range dirs {
		absPath, _ := filepath.Abs(d.path)
		if err := os.MkdirAll(d.path, 0o750); err != nil {
			return fmt.Errorf("create %s dir %s: %w", d.name, absPath, err)
		}
		log.Info("directory ready", "role", d.name, "path", absPath)
	}
	return nil
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

// configureLogging computes the effective base log level and installs the
// process-wide slog handler via app.Setup. useConfigLevel selects the base
// level source: serveMode honors cfg.General's configured level (daemon
// mode, tunable in the config file); run (one-shot --nzb mode) always starts
// from slog.LevelInfo, since it's a short-lived CLI invocation rather than a
// long-running daemon. In both cases -v (verbose) overrides to Debug, and
// logLevelsOverride supplies CLI per-component overrides on top of the
// config's. logFile is the target log file path, or "" for stderr-only
// (one-shot mode never file-logs). The returned io.Closer is non-nil only
// when logFile is set — callers should close it, when non-nil, at shutdown.
func configureLogging(cfg *config.Config, logLevelsOverride string, verbose, useConfigLevel bool, logFile string) (io.Closer, error) {
	logLevel := slog.LevelInfo
	if useConfigLevel {
		lvl, err := cfg.General.ParseLogLevel()
		if err != nil {
			return nil, fmt.Errorf("parse log level: %w", err)
		}
		logLevel = lvl
	}
	if verbose {
		logLevel = slog.LevelDebug
	}

	compLevels, err := resolveLogLevels(cfg, logLevelsOverride)
	if err != nil {
		return nil, fmt.Errorf("parse log levels: %w", err)
	}

	_, logCloser, err := app.Setup(app.LoggingOptions{
		Level:           logLevel,
		LogFile:         logFile,
		ComponentLevels: compLevels,
	})
	if err != nil {
		return nil, fmt.Errorf("setup logging: %w", err)
	}
	return logCloser, nil
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

// waitForCompletion blocks until job reaches history (post-processing
// complete), the context is cancelled, or a 60-minute deadline elapses.
// It polls GetHistory every 2s as a secondary check, covering the case
// where PostProcComplete fired before the select loop started listening.
func waitForCompletion(ctx context.Context, application *app.Application, job *queue.Job, log *slog.Logger) error {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	deadline := time.NewTimer(60 * time.Minute)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("interrupted: %w", ctx.Err())
		case ppc := <-application.PostProcComplete():
			if ppc.JobID == job.ID {
				return nil
			}
		case <-tick.C:
			// Secondary check: has it already reached history?
			// This covers the case where PostProcComplete fired before we started selecting.
			if h, err := application.GetHistory(ctx, job.ID); err == nil {
				log.Info("job found in history", "job", job.Name, "status", h.Status)
				return nil
			}
		case <-deadline.C:
			return fmt.Errorf("no completion in 60 minutes; aborting")
		}
	}
}

func run(configPath, nzbPath, downloadDirOverride, logLevelsOverride string, verbose bool) error {
	cfg, err := loadOrCreateConfig(configPath, false)
	if err != nil {
		return err
	}

	// One-shot mode: stderr-only logging, base level always Info (not
	// config-driven) unless -v is passed.
	if _, err := configureLogging(cfg, logLevelsOverride, verbose, false, ""); err != nil {
		return err
	}
	log := slog.Default().With("component", "main")

	dlDir, adminDir, err := resolveDirs(cfg, downloadDirOverride)
	if err != nil {
		return err
	}

	// Create directories eagerly — fail fast on bad paths.
	if err := ensureRuntimeDirs(cfg, dlDir, adminDir, log); err != nil {
		return err
	}

	// Open history repo (needed for summary at the end)
	db, err := history.Open(context.Background(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		return fmt.Errorf("open history db: %w", err)
	}
	defer db.Close() //nolint:errcheck // best-effort cleanup at exit
	repo := history.NewRepository(db)

	if len(enabledServers(cfg.Servers)) == 0 {
		return fmt.Errorf("no news servers configured — add at least one server to your config file")
	}

	application, err := app.New(cfg, repo)
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

	job, rawNZB, err := loadJob(cfg, nzbPath, log)
	if err != nil {
		return fmt.Errorf("load NZB: %w", err)
	}
	totalFiles := job.NumFiles()
	if totalFiles == 0 {
		return fmt.Errorf("NZB %s contains no usable files", nzbPath)
	}

	if err := application.AddJob(ctx, job, rawNZB, true); err != nil {
		return fmt.Errorf("enqueue job: %w", err)
	}

	start := time.Now()
	log.Info("download started",
		"job", job.Name, "files", totalFiles, "bytes", job.TotalBytes())

	// Wait for the job to reach History (indicates post-processing is complete).
	log.Info("waiting for job to complete", "job", job.Name, "id", job.ID)
	if err := waitForCompletion(ctx, application, job, log); err != nil {
		return err
	}

	duration := time.Since(start)
	hist, err := application.GetHistory(ctx, job.ID)
	if err != nil {
		return fmt.Errorf("retrieve history for summary: %w", err)
	}
	printSummary(job, hist, duration)
	return nil
}

// printSummary prints the one-shot mode download summary to stdout.
func printSummary(job *queue.Job, hist *history.Entry, duration time.Duration) {
	// The promoted scalar, which needs no resident manifest. The history
	// fallback stays for a job that never carried one at all.
	totalBytes := job.TotalBytes()
	if totalBytes == 0 && hist != nil {
		totalBytes = hist.Bytes
	}

	fmt.Printf("\n--- Download Summary ---\n")
	fmt.Printf("Job:        %s\n", job.Name)
	fmt.Printf("Status:     %s\n", hist.Status)
	if hist.FailMessage != "" {
		fmt.Printf("Error:      %s\n", hist.FailMessage)
	}
	fmt.Printf("Location:   %s\n", hist.Path)
	fmt.Printf("Total Size: %s\n", humanfmt.Bytes(totalBytes))
	fmt.Printf("Duration:   %v\n", duration.Round(time.Second))

	// Network throughput (average)
	netMBps := float64(totalBytes) / (1024 * 1024) / duration.Seconds()
	fmt.Printf("Avg Speed:   %.2f MB/s\n", netMBps)
	fmt.Printf("------------------------\n\n")
}

func loadJob(cfg *config.Config, path string, log *slog.Logger) (*queue.Job, []byte, error) { //nocover: thin delegator to unit-tested app.BuildIngestJob, covered end-to-end by test/integration/oneshot_test.go
	data, err := os.ReadFile(path) //nolint:gosec // G304: user-supplied NZB path is the whole point
	if err != nil {
		return nil, nil, err
	}

	parsed, err := nzb.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	job, err := app.BuildIngestJob(cfg, parsed, filepath.Base(path), types.FetchOptions{
		PP:       types.PPInherit,
		Priority: constants.DefaultPriority,
	}, log)
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

// logBuildDeps logs each Go module dependency linked into the binary at Debug
// level. Replace directives are resolved so the effective (fork) version is shown.
func logBuildDeps(log *slog.Logger) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, dep := range bi.Deps {
		ver := dep.Version
		if dep.Replace != nil {
			ver = dep.Replace.Version
		}
		log.Debug("dependency", "module", dep.Path, "version", ver)
	}
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
		Tool:          e.Tool,
		Line:          e.Line,
		Stage:         e.Stage,
		Servers:       e.Servers,
	})
}

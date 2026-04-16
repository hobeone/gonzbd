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
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
	"github.com/hobeone/sabnzbd-go/internal/web"
)

// Version is the build version of the sabnzbd binary. Overridden at build
// time via -ldflags="-X main.Version=<value>".
var Version = "0.0.0-dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "", "path to YAML config file")
	nzbPath := flag.String("nzb", "", "one-shot: path to NZB file to download (mutually exclusive with --serve)")
	serve := flag.Bool("serve", false, "run the daemon: HTTP server (API + web UI) blocking until signal")
	listenAddr := flag.String("listen", "", "override the config's host:port listener (serve mode only)")
	downloadDir := flag.String("download-dir", "", "override complete-dir from config")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		return
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if *configPath == "" {
		usage()
		os.Exit(2)
	}

	switch {
	case *serve && *nzbPath != "":
		fmt.Fprintln(os.Stderr, "--serve and --nzb are mutually exclusive")
		os.Exit(2)
	case *serve:
		if err := serveMode(*configPath, *listenAddr, *downloadDir); err != nil {
			slog.Error("serve failed", "err", err)
			os.Exit(1)
		}
	case *nzbPath != "":
		if err := run(*configPath, *nzbPath, *downloadDir); err != nil {
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
	fmt.Fprintln(os.Stderr, "  sabnzbd --config <path> --serve [--listen host:port] [--download-dir <path>] [-v]")
	fmt.Fprintln(os.Stderr, "  sabnzbd --config <path> --nzb <path> [--download-dir <path>] [-v]")
	fmt.Fprintln(os.Stderr, "  sabnzbd --version")
}

// serveMode runs the long-lived daemon: boots the download pipeline, opens
// the history DB, constructs the API server and web handler, composes them
// on a single listener, and blocks until SIGINT/SIGTERM.
func serveMode(configPath, listenOverride, downloadDirOverride string) error {
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

	// History DB lives under the admin dir so it survives restarts.
	if err := os.MkdirAll(adminDir, 0o750); err != nil {
		return fmt.Errorf("create admin dir %s: %w", adminDir, err)
	}
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

	apiSrv := api.New(api.Options{
		Auth: api.AuthConfig{
			APIKey:          cfg.General.APIKey,
			NZBKey:          cfg.General.NZBKey,
			LocalhostBypass: true,
		},
		Version: Version,
		Queue:   application.Queue(),
		History: histRepo,
		Config:  cfg,
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

func run(configPath, nzbPath, downloadDirOverride string) error {
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

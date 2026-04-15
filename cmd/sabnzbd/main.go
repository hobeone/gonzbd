// Command sabnzbd downloads a single NZB file and exits. It is the Step 4.1
// proof-of-life; later phases add watched directories, the web UI, and the
// API server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/app"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// Version is the build version of the sabnzbd binary. Overridden at build
// time via -ldflags="-X main.Version=<value>".
var Version = "0.0.0-dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "", "path to YAML config file")
	nzbPath := flag.String("nzb", "", "path to NZB file to download")
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

	if *configPath == "" || *nzbPath == "" {
		fmt.Fprintln(os.Stderr, "usage: sabnzbd --config <path> --nzb <path> [--download-dir <path>] [-v]")
		os.Exit(2)
	}

	if err := run(*configPath, *nzbPath, *downloadDir); err != nil {
		slog.Error("download failed", "err", err)
		os.Exit(1)
	}
}

func run(configPath, nzbPath, downloadDirOverride string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	dlDir := cfg.General.CompleteDir
	if downloadDirOverride != "" {
		dlDir = downloadDirOverride
	}
	if dlDir == "" {
		return fmt.Errorf("complete directory is empty (set general.complete_dir in config or pass --download-dir)")
	}

	adminDir := cfg.General.AdminDir
	if adminDir == "" {
		adminDir = filepath.Join(dlDir, "admin")
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

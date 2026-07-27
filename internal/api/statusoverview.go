package api

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// downloadDirFreeBytesTimeout bounds the free-space stat call so a stuck
// network mount can't hang this handler indefinitely.
const downloadDirFreeBytesTimeout = 3 * time.Second

// modeStatusOverview returns the aggregate General Info + System Info
// snapshot for the status page. Deliberately excludes the GitHub update
// check (see modeStatusCheckUpdate) so a slow/unreachable network call
// never delays this handler, and excludes per-server data (already
// available, live, via mode=server_stats).
func (s *Server) modeStatusOverview(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()

	var (
		downloadDir, completeDir, adminDir, scriptDir, logDir string
		par2Cmd, unrarCmd, sevenzCmd                          string
		minFreeSpace                                          int64
	)
	if s.config != nil {
		gen := s.config.GetGeneral()
		pp := s.config.GetPostProc()
		dl := s.config.GetDownloads()
		downloadDir = gen.DownloadDir
		completeDir = gen.CompleteDir
		adminDir = gen.AdminDir
		scriptDir = gen.ScriptDir
		logDir = gen.LogDir
		par2Cmd = pp.Par2Command
		unrarCmd = pp.UnrarCommand
		sevenzCmd = pp.SevenzCommand
		minFreeSpace = int64(dl.MinFreeSpace)
	}

	var articleCacheBytes int64
	var downloadDirFreeBytes int64
	var bv app.BinaryVersions
	if s.status != nil {
		articleCacheBytes = s.status.ArticleCacheBytes()
		ctx, cancel := context.WithTimeout(r.Context(), downloadDirFreeBytesTimeout)
		free, err := s.status.DownloadDirFreeBytes(ctx)
		cancel()
		if err == nil {
			downloadDirFreeBytes = free
		} else {
			s.log.Warn("status_overview: download dir free bytes", "error", err)
		}
		bv = s.status.BinaryVersionsInfo()
	}

	general := map[string]any{
		"version":        s.version,
		"commit":         s.commit,
		"build_date":     s.date,
		"go_version":     runtime.Version(),
		"uptime_seconds": int64(time.Since(s.startTime).Seconds()),
		"hostname":       hostname,
		"local_ip":       localIPv4(),
		"config_path":    s.configPath,
		"download_dir":   downloadDir,
		"complete_dir":   completeDir,
		"admin_dir":      adminDir,
		"script_dir":     scriptDir,
		"log_dir":        logDir,
		"par2":           map[string]any{"path": resolveBinary(par2Cmd, "par2"), "version": bv.Par2Version},
		"unrar":          map[string]any{"path": resolveBinary(unrarCmd, "unrar"), "version": bv.UnrarVersion},
		"sevenzip":       map[string]any{"path": resolveBinary(sevenzCmd, unpack.SevenZipBinaries...), "version": bv.SevenzVersion},
	}

	system := map[string]any{
		"os":                      runtime.GOOS,
		"arch":                    runtime.GOARCH,
		"article_cache_bytes":     articleCacheBytes,
		"download_dir_free_bytes": downloadDirFreeBytes,
		"min_free_space_bytes":    minFreeSpace,
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"general": general,
		"system":  system,
	})
}

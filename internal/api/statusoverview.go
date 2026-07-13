package api

import (
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// modeStatusOverview returns the aggregate General Info + System Info
// snapshot for the status page. Deliberately excludes the GitHub update
// check (see modeStatusCheckUpdate) so a slow/unreachable network call
// never delays this handler, and excludes per-server data (already
// available, live, via mode=server_stats).
func (s *Server) modeStatusOverview(w http.ResponseWriter, _ *http.Request) {
	hostname, _ := os.Hostname()

	var (
		downloadDir, completeDir, adminDir, scriptDir, logDir string
		par2Cmd, unrarCmd, sevenzCmd                          string
		minFreeSpace                                          int64
	)
	if s.config != nil {
		s.config.WithRead(func(cfg *config.Config) {
			downloadDir = cfg.General.DownloadDir
			completeDir = cfg.General.CompleteDir
			adminDir = cfg.General.AdminDir
			scriptDir = cfg.General.ScriptDir
			logDir = cfg.General.LogDir
			par2Cmd = cfg.PostProc.Par2Command
			unrarCmd = cfg.PostProc.UnrarCommand
			sevenzCmd = cfg.PostProc.SevenzCommand
			minFreeSpace = int64(cfg.Downloads.MinFreeSpace)
		})
	}

	var articleCacheBytes int64
	var downloadDirFreeBytes int64
	var binVersions struct{ Par2, Unrar, Sevenz string }
	if s.app != nil {
		articleCacheBytes = s.app.ArticleCacheBytes()
		if free, err := s.app.DownloadDirFreeBytes(); err == nil {
			downloadDirFreeBytes = free
		} else {
			s.log.Warn("status_overview: download dir free bytes", "error", err)
		}
		bv := s.app.BinaryVersionsInfo()
		binVersions.Par2, binVersions.Unrar, binVersions.Sevenz = bv.Par2Version, bv.UnrarVersion, bv.SevenzVersion
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
		"par2":           map[string]any{"path": resolveBinary(par2Cmd, "par2"), "version": binVersions.Par2},
		"unrar":          map[string]any{"path": resolveBinary(unrarCmd, "unrar"), "version": binVersions.Unrar},
		"sevenzip":       map[string]any{"path": resolveBinary(sevenzCmd, unpack.SevenZipBinaries...), "version": binVersions.Sevenz},
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

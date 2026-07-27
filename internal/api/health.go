package api

import (
	"context"
	"net/http"
	"time"
)

// healthTimeout bounds DB and disk stat checks so health probes do not hang.
const healthTimeout = 3 * time.Second

// modeHealth performs a readiness/liveness health check on the daemon,
// verifying DB connectivity, disk space headroom, and pipeline liveness.
func (s *Server) modeHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()

	dbOK := true
	dbMsg := "ok"
	if s.history != nil {
		if err := s.history.Ping(ctx); err != nil {
			dbOK = false
			dbMsg = "database unreachable"
		}
	} else if s.status != nil {
		if err := s.status.PingDB(ctx); err != nil {
			dbOK = false
			dbMsg = "database unreachable"
		}
	}

	diskOK := true
	diskMsg := "ok"
	if s.status != nil {
		free, err := s.status.DownloadDirFreeBytes(ctx)
		if err != nil {
			diskOK = false
			diskMsg = "download dir stat failed"
		} else {
			var minFreeBytes int64
			if s.config != nil {
				minFreeBytes = int64(s.config.GetDownloads().MinFreeSpace)
			}
			if minFreeBytes > 0 && free < minFreeBytes {
				diskOK = false
				diskMsg = "low disk space"
			}
		}
	}

	pipelineOK := true
	pipelineMsg := "ok"
	if s.status != nil {
		if !s.status.IsPipelineHealthy(ctx) {
			pipelineOK = false
			pipelineMsg = "pipeline stalled"
		}
	}

	healthy := dbOK && diskOK && pipelineOK
	statusCode := http.StatusOK
	if !healthy {
		statusCode = http.StatusServiceUnavailable
	}

	level := callerLevel(r, s.getAuth())
	if level < LevelProtected {
		// Unauthenticated LevelOpen response: bare status only, zero internal details leaked.
		if healthy {
			respondJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		} else {
			respondJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error"})
		}
		return
	}

	// Authenticated response: detailed diagnostic envelope.
	respondJSON(w, statusCode, map[string]any{
		"status": healthy,
		"health": map[string]any{
			"db":       dbMsg,
			"disk":     diskMsg,
			"pipeline": pipelineMsg,
		},
	})
}

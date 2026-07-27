package api

import (
	"context"
	"net/http"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

// testConnectionTimeout bounds the on-demand connection test — independent
// of the server's configured Timeout, since a broken/maxed-out server
// shouldn't make the test hang for the user's full configured timeout
// (which can be 60s+).
const testConnectionTimeout = 10 * time.Second

// testDiskSpeedTimeout bounds the on-demand disk-speed test so a
// genuinely broken disk can't hang the request indefinitely.
const testDiskSpeedTimeout = 10 * time.Second

// statusTestConnection dials an existing, already-configured server by
// name (value= parameter) and reports success/failure. A 502/503
// response is flagged as likely_connection_limit: true, since that's
// the response most providers return when an account's connection
// limit is already in use by active downloads — a common, often
// benign cause distinct from a real configuration problem.
func (s *Server) statusTestConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}

	name, ok := s.requireParam(w, r, "value", "server name")
	if !ok {
		return
	}

	var target *config.ServerConfig
	if s.config != nil {
		for _, srv := range s.config.GetServers() {
			if srv.Name == name {
				sc := srv
				target = &sc
				break
			}
		}
	}
	if target == nil {
		respondOK(w, "result", map[string]any{"ok": false, "error": "server not found: " + name})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), testConnectionTimeout)
	defer cancel()

	res, err := s.status.TestNNTPServer(ctx, *target)
	if err != nil {
		s.log.Warn("test_connection failed", "server", name, "error", err)
		respondOK(w, "result", map[string]any{
			"ok":                      false,
			"error":                   err.Error(),
			"likely_connection_limit": res.ConnectionLimitExceeded,
		})
		return
	}

	s.log.Info("test_connection passed", "server", name, "latency_ms", res.Latency.Milliseconds())
	respondOK(w, "result", map[string]any{
		"ok":         true,
		"latency_ms": res.Latency.Milliseconds(),
	})
}

// statusTestDiskSpeed runs a bounded write-speed test against the
// configured download directory.
func (s *Server) statusTestDiskSpeed(w http.ResponseWriter, r *http.Request) {
	if !s.requireApp(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), testDiskSpeedTimeout)
	defer cancel()

	mbPerSec, err := s.status.TestDownloadDirWriteSpeedMBPerSec(ctx)
	if err != nil {
		s.log.Warn("test_disk_speed failed", "error", err)
		respondOK(w, "result", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	respondOK(w, "result", map[string]any{"ok": true, "mb_per_sec": mbPerSec})
}

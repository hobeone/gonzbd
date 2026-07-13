package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/nntp"
)

// testConnectionTimeout bounds the on-demand connection test — independent
// of the server's configured Timeout, since a broken/maxed-out server
// shouldn't make the test hang for the user's full configured timeout
// (which can be 60s+).
const testConnectionTimeout = 10 * time.Second

// statusTestConnection dials an existing, already-configured server by
// name (value= parameter) and reports success/failure. A 502/503
// response is flagged as likely_connection_limit: true, since that's
// the response most providers return when an account's connection
// limit is already in use by active downloads — a common, often
// benign cause distinct from a real configuration problem.
func (s *Server) statusTestConnection(w http.ResponseWriter, r *http.Request) {
	name := formString(r, "value")
	if name == "" {
		s.respondError(w, http.StatusBadRequest, "missing value parameter (server name)")
		return
	}

	var target *config.ServerConfig
	if s.config != nil {
		s.config.WithRead(func(cfg *config.Config) {
			for i := range cfg.Servers {
				if cfg.Servers[i].Name == name {
					sc := cfg.Servers[i]
					target = &sc
					return
				}
			}
		})
	}
	if target == nil {
		respondOK(w, "result", map[string]any{"ok": false, "error": "server not found: " + name})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), testConnectionTimeout)
	defer cancel()

	start := time.Now()
	conn, err := nntp.Dial(ctx, *target, nntp.WithLogger(s.log))
	if err != nil {
		s.log.Warn("test_connection failed", "server", name, "error", err)
		respondOK(w, "result", map[string]any{
			"ok":                      false,
			"error":                   err.Error(),
			"likely_connection_limit": errors.Is(err, nntp.ErrServerUnavailable),
		})
		return
	}
	latency := time.Since(start)
	_ = conn.Close() //nolint:errcheck // test connection; close error is irrelevant

	s.log.Info("test_connection passed", "server", name, "latency_ms", latency.Milliseconds())
	respondOK(w, "result", map[string]any{
		"ok":         true,
		"latency_ms": latency.Milliseconds(),
	})
}

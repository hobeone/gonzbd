package api

import "net/http"

// statusConfig returns the full runtime configuration with credentials and
// other secrets redacted, for display on the status page's Config panel.
// Unlike modeGetConfig (mode=get_config), this is not a SABnzbd-compatible
// endpoint and does not remap section names -- it exists purely for
// gonzbd's own UI.
func (s *Server) statusConfig(w http.ResponseWriter, _ *http.Request) {
	if s.config == nil {
		s.respondError(w, http.StatusInternalServerError, "config not wired")
		return
	}
	respondOK(w, "config", s.config.Redacted())
}

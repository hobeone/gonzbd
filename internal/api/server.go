package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/urlgrabber"
)

// Options configures the API server at construction time.
type Options struct {
	// Version is the application version string returned by mode=version.
	Version string
	// Commit is the short git SHA of the build.
	Commit string
	// Date is the RFC-3339 build timestamp.
	Date string

	// Logger is the structured logger. Defaults to slog.Default() when nil.
	Logger *slog.Logger

	// Dispatcher is the job dispatcher singleton.
	Dispatcher *dispatch.Dispatcher

	// History is the history repository. May be nil; handlers that need it
	// will respond with 500 if it is absent.
	History *history.Repository

	// Config is the application configuration. May be nil; handlers that need it
	// will respond with 500 if it is absent.
	Config *config.Config

	// ConfigPath is the filesystem path where the configuration is stored.
	// Required for mode=set_config to persist changes.
	ConfigPath string

	// Grabber fetches remote NZBs. When nil, mode=addurl returns 501.
	Grabber *urlgrabber.Grabber

	// App is the top-level application instance. Required for hot-reloading
	// core components like the downloader. It is fanned out into the narrow
	// role fields on Server (see setAppServices); handlers depend on those
	// roles, not on the aggregate.
	App AppServices

	// ShutdownFunc is called by mode=shutdown and mode=restart to initiate
	// a graceful application exit. When nil, those modes return 501.
	ShutdownFunc func()

	// Broadcaster is the WebSocket event broadcaster. When nil, New defaults
	// to constructing a fresh Broadcaster.
	Broadcaster *Broadcaster
}

// Server is the HTTP API server. It owns the mode dispatch table and
// exposes its handler via Handler; the caller (cmd/gonzbd) is responsible
// for binding and serving it on a real net/http.Server.
type Server struct {
	version   string
	commit    string
	date      string
	startTime time.Time
	log       *slog.Logger

	dispatcher *dispatch.Dispatcher
	history    *history.Repository
	config     *config.Config
	configPath string
	grabber    *urlgrabber.Grabber

	// Role views of the top-level application, all fanned out from a single
	// Options.App via setAppServices. Because they share one source they are
	// nil together or non-nil together; a nil-check on any one is equivalent
	// to the old single-field "app wired?" probe.
	jobs      JobManager
	downloads DownloaderControl
	reload    ConfigReloader
	status    StatusReporter

	events *Broadcaster

	shutdownFunc func()

	mu       sync.RWMutex
	warnings []string

	modes   modeTable
	mux     *http.ServeMux
	handler http.Handler

	// sessionKey is an ephemeral, in-memory credential generated fresh
	// each time New is called. It is the only key value ever placed in
	// the "gonzbd_apikey" cookie served to the embedded web SPA — see
	// AuthConfig.SessionKey for why it is kept distinct from APIKey.
	sessionKey string
}

// New constructs an API Server. It does not bind or listen; the returned
// Server's Handler method provides the HTTP handler for the caller to serve.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "api")

	events := opts.Broadcaster
	if events == nil {
		events = NewBroadcaster(log)
	}

	s := &Server{
		version:      opts.Version,
		commit:       opts.Commit,
		date:         opts.Date,
		startTime:    time.Now(),
		log:          log,
		dispatcher:   opts.Dispatcher,
		history:      opts.History,
		config:       opts.Config,
		configPath:   opts.ConfigPath,
		grabber:      opts.Grabber,
		shutdownFunc: opts.ShutdownFunc,
		events:       events,
		mux:          http.NewServeMux(),
		sessionKey:   generateSessionKey(),
	}
	s.setAppServices(opts.App)
	s.registerModes()

	// /api handles all API calls via mode= dispatch.
	s.mux.HandleFunc("/api", s.handleAPI)

	// /api/ws handles real-time events.
	s.mux.HandleFunc("/api/ws", s.handleWS)

	// Wrap the mux with logging middleware. Auth is checked per-mode
	// inside handleAPI (each mode has its own access level), not as
	// blanket middleware on the mux.
	s.handler = s.loggingMiddleware(s.mux)

	return s
}

// setAppServices fans a single application aggregate out into the narrow role
// fields. It is the only place those fields are assigned, which is what makes
// them provably nil-together / non-nil-together: every handler nil-guard on a
// single role field is therefore exactly equivalent to the old "app wired?"
// check. A nil argument clears all four (used by tests exercising the
// missing-app paths).
func (s *Server) setAppServices(a AppServices) {
	s.jobs = a
	s.downloads = a
	s.reload = a
	s.status = a
}

// Handler returns the server's root HTTP handler. Useful for
// httptest.NewServer in tests, and for mounting into the production
// router built by cmd/gonzbd.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// SessionKey returns the ephemeral, in-memory key generated for this
// server instance. It is the value the web SPA's "gonzbd_apikey" cookie
// must carry — see AuthConfig.SessionKey.
func (s *Server) SessionKey() string {
	return s.sessionKey
}

// generateSessionKey returns a fresh random hex key for the lifetime of
// this process. It is never persisted or derived from config — restarting
// the process invalidates every previously issued session cookie.
//
// crypto/rand.Read failing means the OS entropy source is unavailable,
// which makes it impossible to run this binary securely at all; that is
// an unrecoverable platform error, not a normal error path to design
// around (see project panic-for-control-flow exception).
func generateSessionKey() string { //nocover: unrecoverable OS entropy error
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("api: failed to generate session key: %w", err))
	}
	return hex.EncodeToString(b)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// LevelProtected is required for real-time events.
	if callerLevel(r, s.getAuth()) < LevelProtected {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	s.events.Handle(w, r)
}

// AddWarning adds a warning message to the internal store.
// Old warnings are dropped when the limit is exceeded.
// A "warnings_updated" event is broadcast to connected WebSocket clients
// so the UI can react without polling.
func (s *Server) AddWarning(msg string) {
	s.mu.Lock()
	s.warnings = append(s.warnings, msg)
	if len(s.warnings) > constants.MaxWarnings {
		s.warnings = s.warnings[len(s.warnings)-constants.MaxWarnings:]
	}
	s.mu.Unlock()

	s.events.Broadcast(Event{Type: "warnings_updated"})
}

// ClearWarnings empties the warning list.
func (s *Server) ClearWarnings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.warnings = []string{}
}

// Warnings returns a snapshot of the current warnings.
func (s *Server) Warnings() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.warnings))
	copy(out, s.warnings)
	return out
}

// EventBroadcaster returns the WebSocket event broadcaster.
func (s *Server) EventBroadcaster() *Broadcaster {
	return s.events
}

// getAuth returns a snapshot of the current authentication configuration.
func (s *Server) getAuth() AuthConfig {
	if s.config == nil {
		return AuthConfig{}
	}
	gen := s.config.GetGeneral()
	var auth AuthConfig
	auth.APIKey = gen.APIKey
	auth.NZBKey = gen.NZBKey
	// LocalRanges is validated at load time, so a parse error here is
	// not expected; on the off chance one slips through, fall back to
	// loopback-only (nil ranges) rather than trusting everything.
	auth.TrustedRanges, _ = config.ParseLocalRanges(gen.LocalRanges)
	auth.VerifyXFF = gen.VerifyXFFHeader
	auth.ForwardHeader = gen.TrustedForwardHeader
	auth.SessionKey = s.sessionKey
	auth.Logger = s.log
	return auth
}

// Dispatcher returns the job dispatcher singleton.
func (s *Server) Dispatcher() *dispatch.Dispatcher {
	return s.dispatcher
}

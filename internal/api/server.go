package api

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/internal/queue"
	"github.com/hobeone/sabnzbd-go/internal/urlgrabber"
)

// Options configures the API server at construction time.
type Options struct {
	// Auth configures API key authentication.
	Auth AuthConfig

	// Version is the application version string returned by mode=version.
	Version string

	// Logger is the structured logger. Defaults to slog.Default() when nil.
	Logger *slog.Logger

	// Queue is the download queue singleton. May be nil; handlers that need it
	// will respond with 500 if it is absent.
	Queue *queue.Queue

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
	// core components like the downloader.
	App ApplicationReloader
}

// ApplicationReloader defines the subset of Application methods needed
// by the API for hot-reloading and job lifecycle management.
type ApplicationReloader interface {
	ReloadDownloader(scs []config.ServerConfig) error
	RetryHistoryJob(ctx context.Context, jobID string) error
	AddJob(ctx context.Context, job *queue.Job, rawNZB []byte) error
	RemoveJob(id string, deleteFiles bool) error
}

// Server is the HTTP API server. It owns a net/http.Server and the mode
// dispatch table. Construct with New, start with Start, shut down with
// Shutdown.
type Server struct {
	auth    AuthConfig
	version string
	log     *slog.Logger

	queue      *queue.Queue
	history    *history.Repository
	config     *config.Config
	configPath string
	grabber    *urlgrabber.Grabber
	app        ApplicationReloader

	mu       sync.RWMutex
	warnings []string

	modes modeTable
	mux   *http.ServeMux
	srv   *http.Server
}

// New constructs an API Server. It does not bind or listen; call Start
// to begin serving.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "api")

	s := &Server{
		auth:       opts.Auth,
		version:    opts.Version,
		log:        log,
		queue:      opts.Queue,
		history:    opts.History,
		config:     opts.Config,
		configPath: opts.ConfigPath,
		grabber:    opts.Grabber,
		app:        opts.App,
		mux:        http.NewServeMux(),
	}
	s.registerModes()

	// /api handles all API calls via mode= dispatch.
	s.mux.HandleFunc("/api", s.handleAPI)

	// Wrap the mux with logging middleware. Auth is checked per-mode
	// inside handleAPI (each mode has its own access level), not as
	// blanket middleware on the mux.
	handler := loggingMiddleware(s.mux)

	s.srv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	return s
}

// Addr returns the listener address after Start succeeds. Useful in tests
// to discover the ephemeral port when ":0" is passed to Start.
func (s *Server) Addr() net.Addr {
	if s.srv == nil {
		return nil
	}
	// The address is captured by the listener in Start; retrieve it via
	// a stored reference. For now, the caller can use the addr returned
	// from StartOnListener.
	return nil
}

// Start binds to addr and serves HTTP until Shutdown is called. It
// returns immediately after the listener is open; the server runs in a
// background goroutine. Returns the listener address (useful when addr
// is ":0" for ephemeral port selection).
func (s *Server) Start(addr string) (net.Addr, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("api: listen %s: %w", addr, err)
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Error("api: serve", "error", err)
		}
	}()
	return ln.Addr(), nil
}

// Shutdown gracefully stops the server, waiting for in-flight requests
// to complete within the context deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// Handler returns the server's root HTTP handler. Useful for
// httptest.NewServer in tests (bypasses Start/listener).
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}

// AddWarning adds a warning message to the internal store.
func (s *Server) AddWarning(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.warnings = append(s.warnings, msg)
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

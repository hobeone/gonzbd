package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/queue"
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

	// ShutdownFunc is called by mode=shutdown and mode=restart to initiate
	// a graceful application exit. When nil, those modes return 501.
	ShutdownFunc func()
}

// ApplicationReloader defines the subset of Application methods needed
// by the API for hot-reloading and job lifecycle management.
type ApplicationReloader interface {
	ReloadDownloader(scs []config.ServerConfig) error
	RetryHistoryJob(ctx context.Context, jobID string) error
	AddJob(ctx context.Context, job *queue.Job, rawNZB []byte, force bool) error
	RemoveJob(ctx context.Context, id string, deleteFiles bool) error
	RemoveHistoryJob(ctx context.Context, id string, deleteFiles bool) error
	SetSpeedLimit(bytesPerSec int64)
	SetBandwidthMax(bytesPerSec int64)
	SetBandwidthPerc(perc int)
	SetDownloadDir(dir string)
	SetCompleteDir(dir string)
	PauseDownloads()
	ResumeDownloads()
	DisconnectAll()
	ReloadPostProcOptions(pp config.PostProcConfig, scriptDir string)
	// ReloadDownloadOptions applies all hot-applicable download settings.
	// Same locking note as ReloadPostProcOptions.
	ReloadDownloadOptions(d config.DownloadConfig)
	// ReloadGeneralOptions applies all hot-applicable general settings.
	// Same locking note as ReloadPostProcOptions.
	ReloadGeneralOptions(g config.GeneralConfig)
	UnblockServer(name string) bool
	ServerStatus() []downloader.ServerSnapshot
	// Speed returns the current aggregate download speed in bytes/sec.
	Speed() float64
	DirectUnpackStatus(jobID string) (directunpack.Status, bool)
	// DirectUnpackStatuses returns a snapshot of every active direct-unpack
	// job's status, keyed by job ID. Used by queueList to take the
	// application-wide mutex once per request instead of once per job
	// (OPT-12).
	DirectUnpackStatuses() map[string]directunpack.Status
	// BinaryVersionsInfo returns resolved external-tool version strings
	// captured at startup, for the status page.
	BinaryVersionsInfo() app.BinaryVersions
	// ArticleCacheBytes returns current write-cache usage, for the status page.
	ArticleCacheBytes() int64
	// DownloadDirFreeBytes returns free disk space on the download directory.
	DownloadDirFreeBytes(ctx context.Context) (int64, error)
	// TestDownloadDirWriteSpeedMBPerSec runs an on-demand disk write-speed test.
	TestDownloadDirWriteSpeedMBPerSec(ctx context.Context) (float64, error)
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

	queue      *queue.Queue
	history    *history.Repository
	config     *config.Config
	configPath string
	grabber    *urlgrabber.Grabber
	app        ApplicationReloader
	events     *Broadcaster

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

	s := &Server{
		version:      opts.Version,
		commit:       opts.Commit,
		date:         opts.Date,
		startTime:    time.Now(),
		log:          log,
		queue:        opts.Queue,
		history:      opts.History,
		config:       opts.Config,
		configPath:   opts.ConfigPath,
		grabber:      opts.Grabber,
		app:          opts.App,
		shutdownFunc: opts.ShutdownFunc,
		events:       NewBroadcaster(log),
		mux:          http.NewServeMux(),
		sessionKey:   generateSessionKey(),
	}
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
	var auth AuthConfig
	s.config.WithRead(func(cfg *config.Config) {
		auth.APIKey = cfg.General.APIKey
		auth.NZBKey = cfg.General.NZBKey
		// LocalRanges is validated at load time, so a parse error here is
		// not expected; on the off chance one slips through, fall back to
		// loopback-only (nil ranges) rather than trusting everything.
		auth.TrustedRanges, _ = config.ParseLocalRanges(cfg.General.LocalRanges)
		auth.VerifyXFF = cfg.General.VerifyXFFHeader
		auth.ForwardHeader = cfg.General.TrustedForwardHeader
	})
	auth.SessionKey = s.sessionKey
	auth.Logger = s.log
	return auth
}

// TestQueue returns the underlying queue for test use. Do not use in
// production code — this exists solely so integration tests can add
// jobs directly to the queue without going through the HTTP handler.
func (s *Server) TestQueue() *queue.Queue {
	return s.queue
}

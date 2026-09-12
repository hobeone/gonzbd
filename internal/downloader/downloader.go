package downloader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/gonzbd/internal/bpsmeter"
	"github.com/hobeone/gonzbd/internal/dispatch"
)

// ErrAlreadyStarted is returned by Start when called twice without an
// intervening Stop. Callers that want idempotent start/stop should
// check this explicitly rather than papering over it.
var ErrAlreadyStarted = errors.New("downloader: already started")

// ErrNoServersLeft is emitted when an article has been tried on all
// available, eligible servers and failed on all of them.
var ErrNoServersLeft = errors.New("downloader: article failed on all servers")

// ErrArticleRemoved is emitted when an article body contains DMCA/takedown
// keywords, indicating the content was removed by the provider. Unlike
// transient decode errors, removed articles should not be retried on
// backup servers — the content is definitively unavailable.
var ErrArticleRemoved = errors.New("downloader: article removed (DMCA/takedown)")

// errServerPenalized is used internally when dialing a penalized server.
var errServerPenalized = errors.New("server penalized")

// ArticleResult is emitted by the Downloader for every fetched
// article, successful or not. Consumers (the assembler, future steps)
// read from Completions() and process in order of arrival — the
// dispatch loop makes no ordering promises across articles or
// servers.
type ArticleResult struct {
	// JobID and MessageID identify the article in the queue.
	JobID     string
	MessageID string

	// FileIdx is the index into the owning job's Files slice.
	FileIdx int

	// ArtIdx is the global index of the article within the job's manifest.
	ArtIdx int32

	// Subject is the filename or subject from the NZB for this article's file.
	Subject string

	// ServerName is the name of the server that served (or failed)
	// the request. Empty when the article was never dispatched.
	ServerName string

	// Data is the decoded article payload (after yEnc/UU decoding).
	// Nil when Err is non-nil.
	Data []byte

	// Offset is the byte position within the target file where Data should
	// be written.
	//
	// For yEnc it is derived from the =ypart begin= field. UU carries no
	// offset field at all, so the fallback in decodePayload asserts 0 —
	// correct for a single-part file and wrong for every part but the first
	// of a multi-part one (#346). Do not read this as "the sender told us
	// where the bytes go" without knowing which decoder produced it.
	Offset int64

	// CRC is the CRC32 of the decoded article data. It travels with the
	// article to the assembler, comes back in the drain, and is folded into
	// the durable run the barrier records — so every successful decode carries
	// one: the yEnc path takes the decoder's own checksum over the decoded
	// output, and the UU path computes one in decodePayload because the format
	// supplies none.
	//
	// Zero only when the article failed. A zero on a successful article
	// means the bytes genuinely hash to zero.
	CRC uint32

	// Err is the dispatch outcome. nil = success. errors.Is against
	// sentinels in internal/nntp to classify failures.
	Err error
}

// articleRequest is the unit of work flowing from the dispatcher to
// a per-server worker. Kept small because these are allocated every
// dispatch pass; heap churn shows up in benchmarks.
type articleRequest struct {
	jobID     string
	messageID string
	fileIdx   int
	artIdx    int32
	bytes     int
	subject   string
	// partNumber is the NZB segment number, carried so the served =ybegin
	// part= ordinal can be compared against it once the body decodes. Nothing
	// acts on a disagreement — see notePartNumberDisagreement.
	partNumber int
}

// Options tunes Downloader behavior. Defaults (zero values) are
// sensible; callers rarely need to set fields explicitly.
type Options struct {
	// CompletionsBuffer is the cap of the completions channel. Larger
	// values let the dispatcher absorb bursts when the decoder is
	// slow; smaller values backpressure the dispatcher sooner. Zero
	// picks a reasonable default (256).
	CompletionsBuffer int

	// PerServerQueue is the cap of each per-server work channel.
	// Larger values reduce dispatch-pass frequency at the cost of
	// giving up more in-flight articles on shutdown. Zero picks a
	// default of 2× connections.
	PerServerQueue int

	// OnJobHopeless is called when a job's health drops below the
	// mathematical threshold for repair.
	OnJobHopeless func(jobID string)

	// MaxArtTries caps the total number of download attempts for a
	// single article across all servers. Zero means unlimited.
	MaxArtTries int

	// MaxArtOpt caps the number of attempts on optional (backup)
	// servers per article. Zero means unlimited.
	MaxArtOpt int

	// TopOnly restricts article dispatch to the highest-priority server
	// group. When true, only servers at the lowest priority value
	// (most preferred) are tried.
	TopOnly bool

	// NoPenalties disables long server penalties; all penalties become
	// a short fixed duration instead of the class-specific defaults.
	NoPenalties bool

	// PreCheck sends a STAT command before BODY to verify article
	// existence on the server, avoiding wasted bandwidth on 430s.
	PreCheck bool

	// PropagationDelay is the minimum age a job must have before its
	// articles are dispatched. Zero means no delay.
	PropagationDelay time.Duration
}

// Downloader orchestrates article dispatch across a set of NNTP
// servers. A Downloader owns:
//
//   - a reference to the Dispatcher, whose Notify channel it selects on
//   - one or more *Server state records (each drives its own pool of
//     connection-worker goroutines)
//   - a main-loop goroutine that runs the dispatch pass
//   - a completions channel consumed by downstream (decoder)
//
// Start/Stop are one-shot — after Stop, create a new Downloader.
// Pause/Resume can toggle freely without bouncing the workers.
//
// Concurrency: public methods are safe for concurrent use. Internal
// state is split across three owners: the atomic pause flag and
// atomic rate-limiter pointer are read by workers without locking;
// the per-server work channels are written by the dispatcher and
// read by workers; the try-list has its own mutex.

// inflightArticle is one article a connection slot currently has in hand.
//
// "In hand", not "on the wire": setConnActivity runs at the top of
// handleRequest, before fetchArticle has dialled anything, and the matching
// clear is deferred to handleRequest's exit — which is after
// processFetchedArticle has decoded the body. An entry therefore covers the
// whole unit of work, wire time and decode time alike. See ConnSnapshot.
//
// It holds the request rather than copying its message-id, subject and
// size, because a copy would be a second place for the same values to live
// and drift. That is safe because an articleRequest is written once and
// only once: every field is unexported, so nothing outside this package can
// assign one, and within it there is a single production construction site.
// `git grep -n 'req := &articleRequest{' -- 'internal/downloader/*.go' ':(exclude)internal/downloader/*_test.go'`
// finds 2 — tryDispatch's composite literal, and this comment quoting the
// pattern at itself. Nothing assigns those fields afterwards:
// `git grep -nE '[.](messageID|subject|bytes) *=' -- 'internal/downloader/*.go' ':(exclude)internal/downloader/*_test.go'`
// finds 0.
//
// The pointer doubles as the entry's identity — tryDispatch allocates a
// fresh request per dispatch, so it stays unique for the life of the fetch
// and lets clearConnActivity remove exactly the entry its own handleRequest
// added.
type inflightArticle struct {
	req   *articleRequest
	since time.Time
}

// ConnActivity describes what a single NNTP connection slot is doing right
// now.
//
// A slot is not one article. With pipelining_requests N a connWorker runs
// up to N*2 concurrent handleRequest goroutines that all share one
// workerID, so several articles can be in hand for one slot at once — N on
// the wire, bounded by wireSem, and up to another N decoding. inflight
// holds all of them, which is what makes the slot report busy until the
// last one lands rather than the first.
//
// len(inflight) is therefore bounded by 2*pipelining_requests, not by
// pipelining_requests. Measured: at pipelining_requests 2 against the mock
// server with 400 KiB bodies, it peaks at 4.
//
// Read by ServerStatus() under connActivityMu.RLock.
type ConnActivity struct {
	ServerName string
	ConnIndex  int
	Connected  bool // true when the underlying TCP connection exists

	// inflight holds this slot's articles in start order, so index 0 is
	// the longest-running. Empty means idle. Written by setConnActivity
	// and clearConnActivity, both under connActivityMu.Lock.
	inflight []inflightArticle
}

// oldest returns the longest-running in-flight article for the slot, and
// whether one exists.
//
// ServerStatus reports this one rather than the newest because the newest
// resets the slot's clock on every pipelined dispatch, which would hide a
// slow article behind its successors.
//
// The elapsed time it yields measures the oldest unit of work start to
// finish, so a large value means the article is slow to fetch OR slow to
// decode. It is not a socket-stall signal on its own, because the oldest
// entry may have left the wire already.
func (ca *ConnActivity) oldest() (inflightArticle, bool) {
	if len(ca.inflight) == 0 {
		return inflightArticle{}, false
	}
	return ca.inflight[0], true
}

// ConnSnapshot is the JSON-serialisable view of a single connection.
//
// WARNING: every exported field's json tag here is part of the public API
// response (nested in ServerSnapshot.Connections). Do not remove or rename
// a field without checking internal/api's field-contract test — see #96.
type ConnSnapshot struct {
	Index int `json:"index"`
	// ArticleID, Subject, Bytes and SinceUnix describe the OLDEST article
	// this connection has in hand. A pipelined connection holds several at
	// once; InFlight says how many, so a reader can tell "one article" from
	// "one of four" without the other three needing wire fields of their
	// own. ArticleID == "" means the connection is idle and InFlight is 0.
	//
	// InFlight counts articles the connection worker is handling, which is
	// NOT the same as articles on the socket. An entry is added before the
	// dial and removed after the body is decoded, so InFlight spans wire
	// time and decode time and is bounded by 2*pipelining_requests rather
	// than by pipelining_requests. SinceUnix inherits that: it measures the
	// oldest unit of work end to end, not how long the socket has been
	// waiting.
	ArticleID string `json:"article_id"`
	Subject   string `json:"subject"`
	Bytes     int    `json:"bytes"`
	SinceUnix int64  `json:"since_unix"`
	InFlight  int    `json:"in_flight"`
	Connected bool   `json:"connected"`
}

// ServerSnapshot is a point-in-time view of a single NNTP server,
// combining config, health, metrics, and per-connection activity.
//
// WARNING: every exported field's json tag here is part of the public API
// response (internal/api serves it directly, and it crosses the WebSocket
// in Event.Servers). Do not remove or rename a field without checking
// internal/api's field-contract test — see issue #96.
type ServerSnapshot struct {
	Name           string         `json:"name"`
	Host           string         `json:"host"`
	Port           int            `json:"port"`
	SSL            bool           `json:"ssl"`
	Priority       int            `json:"priority"`
	Pipelining     int            `json:"pipelining"`
	MaxConnections int            `json:"max_connections"`
	ActiveConns    int            `json:"active_conns"`
	Active         bool           `json:"active"`
	Enabled        bool           `json:"enabled"`
	Optional       bool           `json:"optional"`
	Required       bool           `json:"required"`
	PenaltyUntil   int64          `json:"penalty_until"`
	BPS            float64        `json:"bps"`
	TotalBytes     int64          `json:"total_bytes"`
	Connections    []ConnSnapshot `json:"connections"`
}

// Downloader orchestrates article dispatch across a set of NNTP servers.
type Downloader struct {
	log        *slog.Logger
	dispatcher *dispatch.Dispatcher
	servers    []*Server
	meter      *bpsmeter.Meter
	opts       Options

	// onJobHopeless is guarded by optsMu — set via SetOnJobHopeless,
	// read via snapshot in buildDispatchPlan.
	onJobHopeless func(jobID string)

	// workCh routes requests to per-server worker pools. Keyed by
	// server name (cfg.Name). Created once in New and not resized.
	workCh map[string]chan *articleRequest

	// completions is the buffered output channel consumed by the
	// downstream decoder.
	completions chan *ArticleResult

	// dispatchReady is a cap-1 signal: workers poke it after each
	// result so the main loop knows to scan for more work. Coalesces
	// like dispatcher.Notify.
	dispatchReady chan struct{}

	limiter *bpsmeter.Limiter

	// optsMu protects the mutable dispatch options below. buildDispatchPlan
	// takes RLock once per pass and snapshots the values into locals;
	// SetDispatchOptions takes Lock. The per-article tryDispatch path reads
	// from locals, so this lock is never held during article processing.
	optsMu           sync.RWMutex
	maxArtTries      int
	maxArtOpt        int
	topOnly          bool
	propagationDelay time.Duration

	// paused short-circuits the dispatch pass without tearing down
	// worker goroutines. Independent of dispatcher.Paused (either flag
	// suppresses dispatch).
	paused atomic.Bool

	// pauseMu protects pauseCtx/pauseCancel. Workers snapshot pauseCtx
	// under RLock before each fetch; Pause() cancels and replaces it
	// under Lock.
	pauseMu     sync.RWMutex
	pauseCtx    context.Context //nolint:containedctx // pause lifecycle
	pauseCancel context.CancelFunc

	tracker *dispatchTracker

	// connActivityMu guards connActivity and the inflight slice inside
	// every entry. handleRequest goroutines add and remove their own
	// article via setConnActivity/clearConnActivity — several of them
	// share one entry under pipelining — and ServerStatus() reads all
	// entries under RLock.
	connActivityMu sync.RWMutex
	connActivity   map[string]*ConnActivity // key: "serverName#connIndex"

	// disconnectMu guards check-and-act lifecycle transitions on disconnectPtr.
	disconnectMu sync.Mutex
	// disconnectPtr holds the current disconnect channel. DisconnectAll
	// closes the channel (broadcasting to all connWorkers) and leaves it
	// closed until ensureDisconnectChan restores an open channel when dialing.
	disconnectPtr atomic.Pointer[chan struct{}]

	ctx    context.Context //nolint:containedctx // lifecycle context stored for Stop()
	cancel context.CancelFunc
	wg     sync.WaitGroup

	started atomic.Bool
	stopped atomic.Bool
}

// New constructs a Downloader bound to q and the given servers. The
// returned Downloader is inert until Start; no goroutines run, no
// sockets open. Servers are iterated in slice order as the fallback
// preference — callers should sort by priority before passing.
//
// An empty server slice is allowed — the downloader will start but
// dispatch nothing until servers are added via ReloadDownloader.
func New(disp *dispatch.Dispatcher, servers []*Server, meter *bpsmeter.Meter, opts Options, log *slog.Logger) *Downloader {
	if disp == nil {
		panic("downloader: dispatcher is nil")
	}
	if opts.CompletionsBuffer <= 0 {
		opts.CompletionsBuffer = 256
	}
	if log == nil {
		log = slog.Default()
	}
	d := &Downloader{
		log:              log.With("component", "downloader"),
		dispatcher:       disp,
		servers:          servers,
		meter:            meter,
		opts:             opts,
		onJobHopeless:    opts.OnJobHopeless,
		workCh:           make(map[string]chan *articleRequest, len(servers)),
		completions:      make(chan *ArticleResult, opts.CompletionsBuffer),
		dispatchReady:    make(chan struct{}, 1),
		limiter:          bpsmeter.NewLimiter(0),
		tracker:          newDispatchTracker(),
		connActivity:     make(map[string]*ConnActivity),
		maxArtTries:      opts.MaxArtTries,
		maxArtOpt:        opts.MaxArtOpt,
		topOnly:          opts.TopOnly,
		propagationDelay: opts.PropagationDelay,
	}
	d.ensureDisconnectChan()
	for _, srv := range servers {
		name := srv.Cfg().Name
		perServer := opts.PerServerQueue
		if perServer <= 0 {
			pipelineDepth := max(1, srv.Cfg().PipeliningRequests)
			perServer = 2 * srv.Connections() * pipelineDepth
		}
		perServer = max(1, perServer)
		d.workCh[name] = make(chan *articleRequest, perServer)

		// Disabled servers are kept in d.servers (so ServerStatus can still
		// list them), but never get connection workers or activity
		// entries -- they're skipped at dispatch (isServerCandidate) and
		// would otherwise show idle connection slots that never connect.
		if !srv.Cfg().Enable {
			continue
		}

		// Pre-populate connection activity entries (all idle).
		conns := max(srv.Connections(), 1)
		for i := range conns {
			wid := fmt.Sprintf("%s#%d", name, i)
			d.connActivity[wid] = &ConnActivity{
				ServerName: name,
				ConnIndex:  i,
			}
		}
	}
	return d
}

// Completions returns the receive-only channel carrying fetched
// article bodies (and errors). The decoder consumes from this
// channel. The channel is closed by Stop after all workers have
// drained.
func (d *Downloader) Completions() <-chan *ArticleResult { return d.completions }

// Speed returns the current download speed in bytes per second.
func (d *Downloader) Speed() float64 {
	if d.paused.Load() {
		return 0
	}
	return d.meter.BPS("")
}

// Start launches all worker and dispatcher goroutines. The returned
// context cancel is accessible via Stop; callers do not need to hold
// their own. Returns ErrAlreadyStarted on a second call to a live
// Downloader.
func (d *Downloader) Start(ctx context.Context) error {
	if !d.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}
	d.ctx, d.cancel = context.WithCancel(ctx)

	// Initialize the pause context before spawning workers — they
	// snapshot pauseCtx in handleRequest and will panic on nil if we
	// defer this until after the goroutines are running.
	d.pauseMu.Lock()
	d.pauseCtx, d.pauseCancel = context.WithCancel(d.ctx)
	d.pauseMu.Unlock()

	// Per-server worker pools — one goroutine per configured
	// connection, each lazily dials its own *nntp.Conn.
	totalWorkers := 0
	for i, srv := range d.servers {
		if !srv.Cfg().Enable {
			d.log.Info("server disabled, skipping connection setup", "server", srv.Cfg().Name)
			continue
		}
		conns := max(srv.Connections(), 1)
		for j := range conns {
			wid := fmt.Sprintf("%s#%d", srv.Cfg().Name, j)
			d.wg.Go(func() {
				d.connWorker(d.ctx, srv, i, wid)
			})
		}
		d.log.Info("creating connections", "server", srv.Cfg().Name, "host", srv.Cfg().Host, "connections", conns)
		totalWorkers += conns
	}

	d.wg.Go(func() {
		d.run(d.ctx)
	})

	d.log.Info("started", "servers", len(d.servers), "workers", totalWorkers)

	// Kick off an initial dispatch in case the queue was populated
	// before Start.
	d.signalDispatch()
	return nil
}

// Stop cancels the lifecycle context, waits for all goroutines to
// finish draining, and closes the completions channel. Safe to call
// multiple times and before Start; returns nil either way.
//
// After Stop, the Downloader is inert; callers construct a new
// Downloader to resume operation. In-flight articles at Stop time
// are discarded — their queue entries remain unfinished and will be
// re-dispatched by the next Downloader.
func (d *Downloader) Stop() error {
	if !d.started.Load() {
		return nil
	}
	if !d.stopped.CompareAndSwap(false, true) {
		return nil
	}
	d.log.Debug("stopping")
	d.cancel()
	d.wg.Wait()
	close(d.completions)
	d.log.Info("stopped")
	return nil
}

// Pause suspends dispatch and cancels all in-flight Fetch calls so
// bandwidth drops to zero immediately. Workers stay alive and will
// re-dial on Resume. Articles whose fetch was cancelled will be
// re-dispatched (their Emitted flag is cleared, and context
// cancellation is not penalized).
func (d *Downloader) Pause() {
	d.log.Info("pausing")
	d.paused.Store(true)
	d.pauseMu.Lock()
	if d.pauseCancel != nil {
		d.pauseCancel()
	}
	// Replace with a pre-cancelled context so any new fetches
	// attempted before Resume() fail immediately.
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	d.pauseCtx, d.pauseCancel = context.WithCancel(parent)
	d.pauseCancel() // cancel immediately — we're paused
	d.pauseMu.Unlock()
	if d.meter != nil {
		d.meter.Flush()
	}
	// Close all idle connections. There's no point keeping sockets
	// open while paused; they'll reconnect lazily on Resume.
	d.DisconnectAll()
}

// Resume clears the pause flag, creates a fresh fetch context, and
// pokes the main loop so queued work is re-considered immediately.
func (d *Downloader) Resume() {
	d.pauseMu.Lock()
	if d.pauseCancel != nil {
		d.pauseCancel()
	}
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	d.pauseCtx, d.pauseCancel = context.WithCancel(parent)
	d.pauseMu.Unlock()
	// --- No lock held below this line ---
	d.paused.Store(false)
	d.log.Info("resumed")
	d.signalDispatch()
}

// SetSpeedLimit sets the aggregate byte-rate cap in bytes per second.
// Zero or negative disables throttling. The value takes effect on
// the next article fetch across all workers. The meter is flushed so
// the UI graph reflects the new rate immediately.
//
// The limiter is integrated at the NNTP connection level via
// WithLimiter, providing byte-level rate shaping on the read path.
func (d *Downloader) SetSpeedLimit(bytesPerSec int64) {
	d.limiter.SetRate(float64(bytesPerSec))
	if d.meter != nil {
		d.meter.Flush()
	}
}

// SetDispatchOptions updates the mutable dispatch options without restarting
// the downloader or dropping any connections. Takes effect on the next
// dispatch pass. Thread-safe.
func (d *Downloader) SetDispatchOptions(maxArtTries, maxArtOpt int, topOnly bool, propagationDelay time.Duration) {
	d.optsMu.Lock()
	d.maxArtTries = maxArtTries
	d.maxArtOpt = maxArtOpt
	d.topOnly = topOnly
	d.propagationDelay = propagationDelay
	d.optsMu.Unlock()
}

// IsPaused reports the downloader's own pause flag. Orthogonal to
// dispatcher.Paused(); either being true suppresses dispatch.
func (d *Downloader) IsPaused() bool { return d.paused.Load() }

// DisconnectAll signals all connWorker goroutines to close their idle
// NNTP connections. Workers remain alive and will lazily reconnect when
// new work arrives. This is a no-op if no connections are open or if
// disconnect was already signaled.
//
// Used when the download queue empties to free server resources.
func (d *Downloader) DisconnectAll() {
	d.disconnectMu.Lock()
	signaled := false
	chPtr := d.disconnectPtr.Load()
	if chPtr != nil {
		select {
		case <-*chPtr:
			// Disconnect signal was already sent and is still active.
		default:
			close(*chPtr)
			signaled = true
		}
	}
	d.disconnectMu.Unlock()
	// --- No lock held below this line ---

	if signaled {
		d.log.Info("disconnect: signaled all connections to close")
	}
}

// ensureDisconnectChan restores an open disconnect channel if the current
// one is nil or closed. Called when a worker dials a new NNTP connection.
func (d *Downloader) ensureDisconnectChan() {
	d.disconnectMu.Lock()
	defer d.disconnectMu.Unlock()

	chPtr := d.disconnectPtr.Load()
	if chPtr == nil {
		ch := make(chan struct{})
		d.disconnectPtr.Store(&ch)
		return
	}
	select {
	case <-*chPtr:
		ch := make(chan struct{})
		d.disconnectPtr.Store(&ch)
	default:
	}
}

// disconnectSnapshot returns the current disconnect channel; when
// DisconnectAll closes it, a select on it unblocks and the worker closes
// its connection.
//
// Workers do not call this directly — they go through disconnectChanFor,
// which returns this channel only for a worker that has, or may imminently
// acquire, a connection. A fully idle worker gets nil instead and never
// reaches here; see disconnectChanFor for why.
func (d *Downloader) disconnectSnapshot() <-chan struct{} {
	ch := d.disconnectPtr.Load()
	if ch != nil {
		return *ch
	}
	return nil
}

// disconnectChanFor returns the disconnect channel a worker owning mc
// should select on, or nil when that worker has nothing to disconnect.
//
// DisconnectAll broadcasts by *closing* the channel, which leaves it
// permanently ready, and only ensureDisconnectChan (called when a worker
// dials) ever replaces it with a fresh open one. A worker that has already
// closed its connection therefore keeps re-reading a ready channel, and on
// an idle daemon nothing ever dials to reset it — so every connWorker spun
// at full tilt on the workDisconnect branch forever after the first
// idle-disconnect, doing no work and logging nothing above Debug.
//
// The signal only means anything to a worker that has, or may imminently
// acquire, a connection. Such a worker selects on the real channel; a
// fully idle one selects on nil, which blocks forever in select and parks
// it on workCh/ctx until real work arrives. The next Get() dials and
// re-arms the channel via ensureDisconnectChan, so the worker is again
// responsive to a subsequent DisconnectAll.
//
// inFlight must be true while any handleRequest goroutine sharing mc is
// still running. Those goroutines dial through mc.Get, so a worker that
// parked on nil purely because mc happened to be closed at check time
// could otherwise end up holding a connection opened behind its back,
// with no way to be woken — leaking an idle connection until the next
// unit of work arrived.
//
// mc.isOpen() is a lock-free atomic read: mc.mu is held across nntp.Dial,
// so taking it here would stall this select for the duration of the very
// dial that inFlight is covering for. A stale read is safe in both
// directions — stale-open selects on the real channel and simply loops
// once more, stale-closed is covered by inFlight.
func (d *Downloader) disconnectChanFor(mc *managedConn, inFlight bool) <-chan struct{} {
	if !mc.isOpen() && !inFlight {
		return nil
	}
	return d.disconnectSnapshot()
}

// setConnActivity records that the slot identified by workerID has begun
// fetching req. Called at the start of handleRequest.
func (d *Downloader) setConnActivity(workerID string, req *articleRequest) {
	d.connActivityMu.Lock()
	if ca, ok := d.connActivity[workerID]; ok {
		ca.inflight = append(ca.inflight, inflightArticle{req: req, since: time.Now()})
	}
	d.connActivityMu.Unlock()
}

// clearConnActivity removes req from its slot, leaving that slot's other
// pipelined articles in place. Called via defer at the end of
// handleRequest.
//
// Removing one entry rather than resetting the slot is the whole point:
// several handleRequest goroutines share a workerID under pipelining, and
// a reset by whichever finished first marked a busy connection idle while
// the rest were still in hand.
func (d *Downloader) clearConnActivity(workerID string, req *articleRequest) {
	d.connActivityMu.Lock()
	if ca, ok := d.connActivity[workerID]; ok {
		for i := range ca.inflight {
			if ca.inflight[i].req == req {
				ca.inflight = slices.Delete(ca.inflight, i, i+1)
				break
			}
		}
	}
	d.connActivityMu.Unlock()
}

// setConnConnected updates the Connected flag for the given worker.
// Called by connWorker when a TCP connection is established or closed.
func (d *Downloader) setConnConnected(workerID string, connected bool) {
	d.connActivityMu.Lock()
	if ca, ok := d.connActivity[workerID]; ok {
		ca.Connected = connected
	}
	d.connActivityMu.Unlock()
}

// hasActiveConnections reports whether any connection worker currently
// has an open TCP connection (idle or busy). Used as a guard to avoid
// calling DisconnectAll repeatedly when connections are already closed.
func (d *Downloader) hasActiveConnections() bool {
	d.connActivityMu.RLock()
	defer d.connActivityMu.RUnlock()
	for _, ca := range d.connActivity {
		if ca.Connected {
			return true
		}
	}
	return false
}

// UnblockServer clears any active penalty on the named server, returning
// it to the dispatch pool immediately. Returns false if the server name
// is not found.
func (d *Downloader) UnblockServer(name string) bool {
	for _, srv := range d.servers {
		if srv.Cfg().Name == name {
			srv.ClearDeactivation()
			d.signalDispatch()
			return true
		}
	}
	return false
}

// ServerStatus returns a point-in-time snapshot of all servers,
// including per-connection activity. Safe for concurrent use.
func (d *Downloader) ServerStatus() []ServerSnapshot {
	now := time.Now()

	// Grab meter snapshot once.
	var meterSnap bpsmeter.MeterSnapshot
	if d.meter != nil {
		meterSnap = d.meter.Snapshot()
	}

	// Snapshot connection activity.
	d.connActivityMu.RLock()
	activityByServer := make(map[string][]ConnSnapshot)
	for _, ca := range d.connActivity {
		snap := ConnSnapshot{
			Index:     ca.ConnIndex,
			Connected: ca.Connected,
		}
		// A pipelined slot has several articles in hand; report the
		// oldest. ArticleID staying empty is what marks the slot idle,
		// and it is how activeCount below counts busy connections.
		if a, ok := ca.oldest(); ok {
			snap.ArticleID = a.req.messageID
			snap.Subject = a.req.subject
			snap.Bytes = a.req.bytes
			snap.InFlight = len(ca.inflight)
			if !a.since.IsZero() {
				snap.SinceUnix = a.since.Unix()
			}
		}
		activityByServer[ca.ServerName] = append(activityByServer[ca.ServerName], snap)
	}
	d.connActivityMu.RUnlock()

	snapshots := make([]ServerSnapshot, 0, len(d.servers))
	for _, srv := range d.servers {
		cfg := srv.Cfg()
		name := cfg.Name

		var penaltyUnix int64
		if pe := srv.PenaltyExpiry(); !pe.IsZero() && pe.After(now) {
			penaltyUnix = pe.Unix()
		}

		var bps float64
		var totalBytes int64
		if meterSnap.Servers != nil {
			if ss, ok := meterSnap.Servers[name]; ok {
				if !d.paused.Load() {
					bps = ss.BPS
				}
				totalBytes = ss.Total
			}
		}

		conns := activityByServer[name]
		if conns == nil {
			conns = []ConnSnapshot{}
		}

		// Busy CONNECTIONS, not articles in flight: a pipelined
		// connection carrying several articles contributes one.
		//
		// ActiveConns cannot exceed MaxConnections, and that holds by
		// construction rather than by arithmetic here. New pre-populates
		// connActivity with max(srv.Connections(), 1) entries for each
		// ENABLED server, one per workerID, and that is the only insertion
		// — `git grep -n 'd.connActivity\[wid\] = ' internal/downloader`
		// finds 1, and `git grep -n 'delete(.*connActivity' internal/downloader`
		// finds 1, which is this comment quoting the pattern at itself
		// rather than any code — so len(conns) never moves after startup.
		//
		// Two branches make len(conns) and MaxConnections differ, and
		// neither breaks the inequality:
		//
		//   - Disabled server: New allocates it no entries at all, so
		//     len(conns) is 0 while MaxConnections still reports the
		//     configured count. activeCount is then 0, which is <= any
		//     MaxConnections.
		//   - cfg.Connections < 1: the max(..., 1) above would allocate one
		//     entry against a MaxConnections of 0, and activeCount could
		//     reach 1. Unreachable through a validated config —
		//     config.validate calls positive("connections", ...) — so the
		//     inequality assumes a server that came through validation.
		activeCount := 0
		for _, c := range conns {
			if c.ArticleID != "" {
				activeCount++
			}
		}

		snapshots = append(snapshots, ServerSnapshot{
			Name:           name,
			Host:           cfg.Host,
			Port:           cfg.Port,
			SSL:            cfg.SSL,
			Priority:       cfg.Priority,
			Pipelining:     cfg.PipeliningRequests,
			MaxConnections: cfg.Connections,
			ActiveConns:    activeCount,
			Active:         srv.Active(now),
			Enabled:        cfg.Enable,
			Optional:       cfg.Optional,
			Required:       cfg.Required,
			PenaltyUntil:   penaltyUnix,
			BPS:            bps,
			TotalBytes:     totalBytes,
			Connections:    conns,
		})
	}
	return snapshots
}

// SpeedLimit returns the current speed limit in bytes/sec.
// Returns 0 when unlimited.
func (d *Downloader) SpeedLimit() int64 { return int64(d.limiter.Rate()) }

// CancelJob stops tracking for the specified job and clears its try-list
// and in-flight state.
func (d *Downloader) CancelJob(jobID string) {
	d.tracker.ClearJob(jobID)
}

// Wake non-blocking-pokes the main loop to run a dispatch pass.
func (d *Downloader) Wake() { d.signalDispatch() }

// signalDispatch non-blocking-pokes the main loop. Coalesces rapid
// signals (cap-1 channel); callers never block.
func (d *Downloader) signalDispatch() {
	select {
	case d.dispatchReady <- struct{}{}:
	default:
	}
}

func (d *Downloader) checkExpiredPenalties() {
	now := time.Now()
	for _, srv := range d.servers {
		// Active returns true if the server is enabled and has no active penalty.
		// By clearing deactivation on all active servers, we ensure any expired
		// penalties are completely erased from the state, making Server.Active()
		// side-effect free.
		if srv.Active(now) {
			srv.ClearDeactivation()
		}
	}
}

// run is the main dispatcher loop. One goroutine. Selects on three
// sources:
//
//   - ctx.Done       — begin shutdown
//   - dispatcher.Notify — new work was added / resumed / reordered
//   - dispatchReady  — a worker freed up
//
// The loop must stay tight: all heavy lifting (per-article iteration,
// send to workCh) happens inside dispatchPass, which itself must not
// block. Blocking the main loop stalls rate-limit updates and
// shutdown.
func (d *Downloader) run(ctx context.Context) {
	// Periodic ticker ensures the dispatcher wakes up to discover
	// expired server penalties even when no workers are active (all
	// servers penalized → no dispatchReady signals arriving).
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.dispatcher.Notify():
			d.checkExpiredPenalties()
			d.dispatchPass(ctx)
		case <-d.dispatchReady:
			d.checkExpiredPenalties()
			d.dispatchPass(ctx)
		case <-ticker.C:
			d.checkExpiredPenalties()
			d.dispatchPass(ctx)
		}
	}
}

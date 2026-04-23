package downloader

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/hobeone/sabnzbd-go/internal/bpsmeter"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// ErrAlreadyStarted is returned by Start when called twice without an
// intervening Stop. Callers that want idempotent start/stop should
// check this explicitly rather than papering over it.
var ErrAlreadyStarted = errors.New("downloader: already started")

// ErrNoServersLeft is emitted when an article has been tried on all
// available, eligible servers and failed on all of them.
var ErrNoServersLeft = errors.New("downloader: article failed on all servers")

// ArticleResult is emitted by the Downloader for every fetched
// article, successful or not. Consumers (the decoder, future steps)
// read from Completions() and process in order of arrival — the
// dispatch loop makes no ordering promises across articles or
// servers.
type ArticleResult struct {
	// JobID and MessageID identify the article in the queue.
	JobID     string
	MessageID string

	// FileIdx is the index into the owning job's Files slice. The
	// decoder uses it to route yEnc-decoded bytes back to the right
	// file's assembly buffer.
	FileIdx int

	// Subject is the filename or subject from the NZB for this article's file.
	Subject string

	// ServerName is the name of the server that served (or failed)
	// the request. Empty when the article was never dispatched.
	ServerName string

	// Body is the raw NNTP response body, with dot-stuffing removed
	// but yEnc/UU decoding still to happen downstream. Nil when Err
	// is non-nil.
	Body []byte

	// Err is the dispatch outcome. nil = success. errors.Is against
	// sentinels in internal/nntp to classify failures.
	Err error
}

// articleKey identifies an article globally, for try-list tracking.
type articleKey struct {
	jobID     string
	messageID string
}

// articleRequest is the unit of work flowing from the dispatcher to
// a per-server worker. Kept small because these are allocated every
// dispatch pass; heap churn shows up in benchmarks.
type articleRequest struct {
	jobID     string
	messageID string
	fileIdx   int
	bytes     int
	subject   string
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
}

// Downloader orchestrates article dispatch across a set of NNTP
// servers. A Downloader owns:
//
//   - a reference to the Queue, whose Notify channel it selects on
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
type Downloader struct {
	log     *slog.Logger
	queue   *queue.Queue
	servers []*Server

	onJobHopeless func(jobID string)

	// workCh routes requests to per-server worker pools. Keyed by
	// server name (cfg.Name). Created once in New and not resized.
	workCh map[string]chan *articleRequest

	// completions is the buffered output channel consumed by the
	// downstream decoder.
	completions chan *ArticleResult

	// dispatchReady is a cap-1 signal: workers poke it after each
	// result so the main loop knows to scan for more work. Coalesces
	// like queue.Notify.
	dispatchReady chan struct{}

	limiter *bpsmeter.Limiter

	// paused short-circuits the dispatch pass without tearing down
	// worker goroutines. Independent of queue.IsPaused (either flag
	// suppresses dispatch).
	paused atomic.Bool

	tryMu   sync.Mutex
	tryList map[articleKey]map[string]struct{}

	// inFlight counts outstanding requests per article across all
	// servers. An article with inFlight > 0 is not re-dispatched even
	// if its try-list has untried servers remaining; the in-flight
	// request must resolve first. This prevents speculative fan-out to
	// multiple servers, which would double-charge paid bandwidth and
	// produce duplicate completions.
	inFlight map[articleKey]int

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
// servers must be non-empty. A zero-length slice is a programming
// error; New panics to surface it at config time rather than having
// the dispatch loop silently do nothing.
func New(q *queue.Queue, servers []*Server, opts Options, log *slog.Logger) *Downloader {
	if q == nil {
		panic("downloader: queue is nil")
	}
	if len(servers) == 0 {
		panic("downloader: at least one server is required")
	}
	if opts.CompletionsBuffer <= 0 {
		opts.CompletionsBuffer = 256
	}
	if log == nil {
		log = slog.Default()
	}
	d := &Downloader{
		log:           log.With("component", "downloader"),
		queue:         q,
		servers:       servers,
		onJobHopeless: opts.OnJobHopeless,
		workCh:        make(map[string]chan *articleRequest, len(servers)),
		completions:   make(chan *ArticleResult, opts.CompletionsBuffer),
		dispatchReady: make(chan struct{}, 1),
		limiter:       bpsmeter.NewLimiter(0),
		tryList:       make(map[articleKey]map[string]struct{}),
		inFlight:      make(map[articleKey]int),
	}
	for _, srv := range servers {
		perServer := opts.PerServerQueue
		if perServer <= 0 {
			perServer = 2 * srv.Connections()
		}
		if perServer < 1 {
			perServer = 1
		}
		d.workCh[srv.Cfg().Name] = make(chan *articleRequest, perServer)
	}
	return d
}

// Completions returns the receive-only channel carrying fetched
// article bodies (and errors). The decoder consumes from this
// channel. The channel is closed by Stop after all workers have
// drained.
func (d *Downloader) Completions() <-chan *ArticleResult { return d.completions }

// Start launches all worker and dispatcher goroutines. The returned
// context cancel is accessible via Stop; callers do not need to hold
// their own. Returns ErrAlreadyStarted on a second call to a live
// Downloader.
func (d *Downloader) Start(ctx context.Context) error {
	if !d.started.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}
	d.ctx, d.cancel = context.WithCancel(ctx)

	// Per-server worker pools — one goroutine per configured
	// connection, each lazily dials its own *nntp.Conn.
	totalWorkers := 0
	for _, srv := range d.servers {
		conns := srv.Connections()
		if conns < 1 {
			conns = 1
		}
		for i := 0; i < conns; i++ {
			d.wg.Go(func() {
				d.connWorker(d.ctx, srv)
			})
		}
		d.log.Debug("server workers started", "server", srv.Cfg().Name, "workers", conns)
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

// Pause suspends dispatch. Workers currently mid-Fetch run to
// completion; no new requests are handed out until Resume.
func (d *Downloader) Pause() { d.paused.Store(true) }

// Resume clears the pause flag and pokes the main loop so any
// queued work is re-considered immediately.
func (d *Downloader) Resume() {
	d.paused.Store(false)
	d.signalDispatch()
}

// SetSpeedLimit sets the aggregate byte-rate cap in bytes per second.
// Zero or negative disables throttling. The value takes effect on
// the next article completion across all workers.
//
// The throttle is coarse-grained: it delays workers between fetches
// based on body size, rather than shaping the wire-level byte stream.
// Fine-grained byte-level shaping requires pushing the limiter into
// the NNTP read path; that is deliberately deferred until the decoder
// integration step so the limiter can account for yEnc overhead too.
func (d *Downloader) SetSpeedLimit(bytesPerSec int64) {
	d.limiter.SetRate(float64(bytesPerSec))
}

// IsPaused reports the downloader's own pause flag. Orthogonal to
// queue.IsPaused; either being true suppresses dispatch.
func (d *Downloader) IsPaused() bool { return d.paused.Load() }

// signalDispatch non-blocking-pokes the main loop. Coalesces rapid
// signals (cap-1 channel); callers never block.
func (d *Downloader) signalDispatch() {
	select {
	case d.dispatchReady <- struct{}{}:
	default:
	}
}

// run is the main dispatcher loop. One goroutine. Selects on three
// sources:
//
//   - ctx.Done       — begin shutdown
//   - queue.Notify   — new work was added / resumed / reordered
//   - dispatchReady  — a worker freed up
//
// The loop must stay tight: all heavy lifting (per-article iteration,
// send to workCh) happens inside dispatchPass, which itself must not
// block. Blocking the main loop stalls rate-limit updates and
// shutdown.
func (d *Downloader) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.queue.Notify():
			d.dispatchPass(ctx)
		case <-d.dispatchReady:
			d.dispatchPass(ctx)
		}
	}
}

package nntp

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

type limitReader struct {
	r      io.Reader
	lim    RateLimiter
	ctx    context.Context
	rec    ByteRecorder
	server string
}

func (lr *limitReader) Read(p []byte) (int, error) {
	n, err := lr.r.Read(p)
	if n > 0 {
		if lr.rec != nil {
			lr.rec.RecordBytes(lr.server, int64(n))
		}
		if lr.lim != nil {
			// Wait after reading. This introduces minimal overhead
			// because it is invoked by bufio.Reader which chunks reads.
			// We use the connection context to unblock if the socket closes.
			if err := lr.lim.Wait(lr.ctx, n); err != nil {
				return n, err
			}
		}
	}
	return n, err
}

// Conn is a live connection to a single NNTP server. It is safe for
// concurrent use by multiple goroutines. See the package doc for the
// overall concurrency model.
//
// A Conn progresses through a linear state machine (see state.go); it
// is usable only while State() == StateReady. Once Close returns the
// Conn is inert — discard it and Dial a fresh one to reconnect.
type Conn struct {
	log *slog.Logger
	cfg config.ServerConfig

	nc net.Conn
	bw *bufio.Writer
	br *bufio.Reader

	stateLock sync.Mutex
	state     State

	sendLock sync.Mutex

	pendingLock sync.Mutex
	pending     []*pendingCmd

	// sem bounds the number of in-flight commands. Cap equals
	// PipeliningRequests; defaults to 1 when misconfigured.
	sem chan struct{}

	closed     atomic.Bool
	closeErr   error
	closeOnce  sync.Once
	readerDone chan struct{}

	caps *Capabilities

	ctx    context.Context
	cancel context.CancelFunc

	// sslInfo is the negotiated TLS protocol+cipher for UI display.
	// Empty for plain-text connections.
	sslInfo string

	// readTimeout is the idle read deadline applied before each response
	// read. If no data arrives within this period, the socket errors and
	// the connection is torn down. Prevents runReader from blocking
	// indefinitely on silently-dead connections.
	readTimeout time.Duration
}

// State returns the current lifecycle state. The value is a snapshot;
// callers racing with a concurrent Close may observe stale values.
func (c *Conn) State() State {
	c.stateLock.Lock()
	defer c.stateLock.Unlock()
	return c.state
}

// SSLInfo returns "TLSv1.x / CIPHER_NAME" for a TLS-wrapped connection,
// or empty string for plain NNTP.
func (c *Conn) SSLInfo() string { return c.sslInfo }

// Caps returns the capabilities advertised by the server at connect
// time. Nil if no CAPABILITIES probe has been performed (e.g. server
// rejected the command).
func (c *Conn) Caps() *Capabilities { return c.caps }

// setState transitions to next under the state lock. Returns
// errInvalidTransition (wrapping ErrInvalidState) if the move is not
// permitted by canTransition.
func (c *Conn) setState(next State) error {
	c.stateLock.Lock()
	defer c.stateLock.Unlock()
	if !c.state.canTransition(next) {
		return errInvalidTransition{from: c.state, to: next}
	}
	c.state = next
	return nil
}

// RateLimiter is the interface for an external bandwidth shaper.
// Dial accepts one via WithLimiter.
type RateLimiter interface {
	Wait(ctx context.Context, n int) error
}

// ByteRecorder receives byte counts as they are read from the wire.
// Dial accepts one via WithRecorder. The meter records each TCP
// read chunk (~1.5 KiB) immediately, giving the UI a smooth
// real-time speed display.
type ByteRecorder interface {
	RecordBytes(server string, n int64)
}

// DialOption tunes NNTP connection establishment.
type DialOption func(*dialOptions)

// WithLimiter attaches an external bandwidth shaper to the connection.
// All Read calls on the resulting Conn gate on the limiter.
func WithLimiter(l RateLimiter) DialOption {
	return func(o *dialOptions) {
		o.limiter = l
	}
}

// WithRecorder attaches a byte recorder to the connection. Each TCP
// read is reported to the recorder with the given server name so
// the UI speed graph updates in real-time.
func WithRecorder(r ByteRecorder, server string) DialOption {
	return func(o *dialOptions) {
		o.recorder = r
		o.recorderServer = server
	}
}

// WithLogger attaches a custom logger to the connection.
func WithLogger(l *slog.Logger) DialOption {
	return func(o *dialOptions) {
		o.log = l
	}
}

// dialOptions are the knobs Dial tunes internally. They live in a
// struct instead of separate Dial arguments so callers don't have to
// care about defaults — ServerConfig carries everything.
type dialOptions struct {
	host           string
	port           int
	useTLS         bool
	tlsConfig      *tls.Config
	dialer         *net.Dialer
	pipelining     int
	readBuf        int
	limiter        RateLimiter
	recorder       ByteRecorder
	recorderServer string
	log            *slog.Logger
}

// newDialOptions derives the per-dial knobs from a ServerConfig,
// filling defaults where the config is zero-valued.
func newDialOptions(cfg config.ServerConfig) (*dialOptions, error) {
	port := cfg.Port
	if port == 0 {
		if cfg.SSL {
			port = 563
		} else {
			port = 119
		}
	}
	pipe := max(1, cfg.PipeliningRequests)
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout <= 0 {
		timeout = 60 * time.Second
	}

	opts := &dialOptions{
		host:       cfg.Host,
		port:       port,
		useTLS:     cfg.SSL,
		dialer:     &net.Dialer{Timeout: timeout},
		pipelining: pipe,
		readBuf:    256 * 1024,
		log:        slog.Default(),
	}
	if cfg.SSL {
		tc, err := buildTLSConfig(cfg.Host, cfg.SSLVerify, cfg.SSLCiphers)
		if err != nil {
			return nil, err
		}
		opts.tlsConfig = tc
	}
	return opts, nil
}

// Dial connects to the server described by cfg, performs the greeting
// handshake, authenticates if credentials are supplied, probes
// capabilities, and returns a ready-to-use *Conn. The context governs
// the full handshake; once Dial returns, cancellation is per-request
// via Fetch's ctx.
//
// On any error during handshake the socket is closed before the error
// is returned; the caller does not need to Close a *Conn that never
// escaped Dial.
func Dial(ctx context.Context, cfg config.ServerConfig, opts ...DialOption) (*Conn, error) {
	dopts, err := newDialOptions(cfg)
	if err != nil {
		return nil, err
	}
	for _, opt := range opts {
		opt(dopts)
	}

	l := dopts.log.With("component", "nntp", "server", cfg.Name, "host", cfg.Host)

	addr := net.JoinHostPort(dopts.host, strconv.Itoa(dopts.port))
	l.Debug("dialing", "addr", addr, "ssl", dopts.useTLS, "timeout", dopts.dialer.Timeout)

	var nc net.Conn
	if dopts.useTLS {
		d := &tls.Dialer{NetDialer: dopts.dialer, Config: dopts.tlsConfig}
		nc, err = d.DialContext(ctx, "tcp", addr)
	} else {
		nc, err = dopts.dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		l.Debug("dial failed", "addr", addr, "error", err)
		return nil, fmt.Errorf("nntp: dial %s: %w", addr, err)
	}
	l.Debug("TCP connected", "addr", addr)

	ctxConn, cancelConn := context.WithCancel(context.Background())

	var br io.Reader = nc
	if dopts.dialer.Timeout > 0 {
		br = &idleTimeoutReader{nc: nc, timeout: dopts.dialer.Timeout}
	}
	if dopts.limiter != nil || dopts.recorder != nil {
		br = &limitReader{
			r:      br,
			lim:    dopts.limiter,
			ctx:    ctxConn,
			rec:    dopts.recorder,
			server: dopts.recorderServer,
		}
	}

	c := &Conn{
		log:         l,
		cfg:         cfg,
		nc:          nc,
		bw:          bufio.NewWriter(nc),
		br:          bufio.NewReaderSize(br, dopts.readBuf),
		state:       StateDisconnected,
		sem:         make(chan struct{}, dopts.pipelining),
		readerDone:  make(chan struct{}),
		ctx:         ctxConn,
		cancel:      cancelConn,
		readTimeout: dopts.dialer.Timeout, // kept for reference; actual idle enforced by idleTimeoutReader
	}

	if tc, ok := nc.(*tls.Conn); ok {
		st := tc.ConnectionState()
		c.sslInfo = fmt.Sprintf("%s / %s", tls.VersionName(st.Version), tls.CipherSuiteName(st.CipherSuite))
		l.Debug("TLS established", "tls", c.sslInfo)
	}

	if err := c.handshake(ctx, cfg); err != nil {
		l.Debug("handshake failed", "error", err)
		cancelConn()   // release context resources on handshake failure
		_ = nc.Close() //nolint:errcheck // handshake failed; socket is being torn down regardless
		return nil, err
	}

	l.Debug("handshake complete, connection ready")
	go c.runReader()
	return c, nil
}

// handshake runs synchronously on the caller's goroutine before the
// reader loop starts. That simplifies the auth/caps sequence: each
// step reads exactly one response with no FIFO bookkeeping.
func (c *Conn) handshake(ctx context.Context, cfg config.ServerConfig) error {
	cleanup := c.setupHandshakeDeadline(ctx)
	defer cleanup()

	if err := c.expectGreeting(); err != nil {
		return err
	}

	if cfg.Username != "" {
		c.log.Debug("handshake: authenticating", "user", cfg.Username)
		if err := c.authenticate(cfg.Username, cfg.Password); err != nil {
			c.log.Debug("handshake: auth failed", "error", err)
			return err
		}
		c.log.Debug("handshake: authenticated")
	}

	// Capability probe is best-effort: servers that refuse it still
	// work for BODY/STAT via the fallback in capabilities.go.
	c.log.Debug("handshake: probing capabilities")
	c.caps = probeCapabilities(c.log, c.bw, c.br)
	c.log.Debug("handshake: capabilities",
		"version", c.caps.Version,
		"body", c.caps.HasBody, "stat", c.caps.HasStat,
		"over", c.caps.HasOver, "hdr", c.caps.HasHDR,
		"post", c.caps.HasPost, "compress", c.caps.HasCompress)

	// Whether or not we authenticated, the connection is now dispatch-
	// ready. Advance to Ready.
	if err := c.setState(StateReady); err != nil {
		return err
	}
	return nil
}

// expectGreeting reads the server greeting and advances to StateConnected.
func (c *Conn) expectGreeting() error {
	c.log.Debug("handshake: waiting for greeting")
	line, err := readResponseLine(c.br)
	if err != nil {
		return fmt.Errorf("nntp: read greeting: %w", err)
	}
	code, text, err := parseStatus(line)
	if err != nil {
		return err
	}
	c.log.Debug("handshake: greeting received", "code", code, "text", text)
	if code != 200 && code != 201 {
		return &ServerError{Code: code, Text: text}
	}
	return c.setState(StateConnected)
}

// setupHandshakeDeadline arranges to force-unblock any pending read/write
// on the socket if ctx ends before the handshake does — whether ctx
// carries its own deadline (as the admin test-connection handlers'
// context.WithTimeout callers do) or ends only via cancellation (as the
// download path's pauseCtx, which never carries a deadline, does).
// Returns a cleanup function to be deferred by the caller.
//
// context.AfterFunc's stop() is race-free against the scheduled func by
// construction (gated by an internal sync.Once): whichever of stop() or
// ctx's own cancellation reaches it first is authoritative, so cleanup
// racing a concurrent cancellation can no longer stamp a stray deadline on
// a connection that has already returned to service (#396).
func (c *Conn) setupHandshakeDeadline(ctx context.Context) func() {
	stop := context.AfterFunc(ctx, func() {
		_ = c.nc.SetDeadline(time.Now()) //nolint:errcheck // best-effort unblock
	})
	return func() { stop() }
}

// validateCredential rejects credentials that contain characters
// capable of injecting additional NNTP commands via CR/LF or null
// bytes. It has no Message-ID counterpart here any more: those are
// validated at parse time in internal/nzb, whereas credentials come
// from config and never pass through that layer.
// Empty strings are allowed — some servers accept empty passwords.
func validateCredential(val, label string) error {
	if strings.ContainsAny(val, "\r\n\x00") {
		return fmt.Errorf("%w in %s", ErrInvalidCredential, label)
	}
	return nil
}

// authenticate drives the AUTHINFO USER / AUTHINFO PASS dance
// synchronously. On success state advances to Authenticated; on
// failure ErrInvalidCredential (bad input) or ErrAuthRejected
// (server 481/482) is returned and the Conn is unusable (caller
// should close).
func (c *Conn) authenticate(user, pass string) error {
	if err := validateCredential(user, "username"); err != nil {
		return err
	}
	if err := validateCredential(pass, "password"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.bw, "AUTHINFO USER %s\r\n", user); err != nil {
		return fmt.Errorf("nntp: write AUTHINFO USER: %w", err)
	}
	if err := c.bw.Flush(); err != nil {
		return fmt.Errorf("nntp: flush AUTHINFO USER: %w", err)
	}
	line, err := readResponseLine(c.br)
	if err != nil {
		return fmt.Errorf("nntp: read AUTHINFO USER: %w", err)
	}
	code, text, err := parseStatus(line)
	if err != nil {
		return err
	}
	switch code {
	case 281:
		return c.setState(StateAuthenticated) // no password needed
	case 381:
		// password prompt
	case 481, 482:
		return fmt.Errorf("%w: %s", ErrAuthRejected, text)
	default:
		return &ServerError{Code: code, Text: text}
	}

	if _, err := fmt.Fprintf(c.bw, "AUTHINFO PASS %s\r\n", pass); err != nil {
		return fmt.Errorf("nntp: write AUTHINFO PASS: %w", err)
	}
	if err := c.bw.Flush(); err != nil {
		return fmt.Errorf("nntp: flush AUTHINFO PASS: %w", err)
	}
	line, err = readResponseLine(c.br)
	if err != nil {
		return fmt.Errorf("nntp: read AUTHINFO PASS: %w", err)
	}
	code, text, err = parseStatus(line)
	if err != nil {
		return err
	}
	switch code {
	case 281:
		return c.setState(StateAuthenticated)
	case 481, 482:
		return fmt.Errorf("%w: %s", ErrAuthRejected, text)
	default:
		return &ServerError{Code: code, Text: text}
	}
}

// Fetch retrieves the body of the article with the given Message-ID.
// The returned byte slice is the raw, un-dotstuffed body as sent by
// the server — yEnc/UU decoding happens downstream.
//
// If ctx is cancelled while the command is in flight, Fetch returns
// ctx.Err() promptly; the corresponding response is still read from
// the wire and discarded so the connection stays in protocol sync.
// The pipelining slot is released either way.
func (c *Conn) Fetch(ctx context.Context, messageID string) ([]byte, error) {
	if c.State() != StateReady {
		return nil, ErrInvalidState
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Acquire a pipelining slot. Also watch c.ctx so we unblock if
	// the connection dies (runReader exits without draining sem).
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.closeError()
	}

	pc, cmd := newArticleCmd(cmdBody, messageID)
	if err := c.submit(pc, cmd); err != nil {
		return nil, err
	}

	select {
	case <-pc.done:
		if pc.result.err != nil {
			c.log.Debug("fetch error", "msgid", messageID, "error", pc.result.err)
			return nil, pc.result.err
		}
		if sentinel := classifyStatus(pc.result.code); sentinel != nil {
			c.log.Debug("fetch rejected", "msgid", messageID, "code", pc.result.code)
			return nil, fmt.Errorf("%w: %d %s", sentinel, pc.result.code, pc.result.line)
		}
		// !success rather than a literal 222: the reader decides what
		// success means for this kind, and a second copy of that code
		// here could drift into accepting a response the reader never
		// identity-checked.
		if !pc.result.success {
			return nil, &ServerError{Code: pc.result.code, Text: pc.result.line}
		}
		c.log.Debug("fetch ok", "msgid", messageID, "bytes", len(pc.result.body))
		return pc.result.body, nil
	case <-ctx.Done():
		// Mark the pending entry orphaned. The reader goroutine
		// will still read+discard the response, freeing the
		// semaphore slot.
		pc.orphaned.Store(true)
		c.log.Debug("fetch cancelled", "msgid", messageID)
		return nil, ctx.Err()
	}
}

// Message-ID validity is not checked here, deliberately. internal/nzb
// normalises the angle-bracket wrapper away and refuses any ID carrying SP,
// HT, CR, LF, NUL, '<' or '>' — a strict superset of what this layer used to
// reject — so no such ID can reach a Conn.
//
// The restore path is covered by a check rather than by that argument.
// internal/queue's Manifest.UnmarshalJSON rebuilds a resumed job's article
// list straight from disk and cannot know which build wrote it, so it
// re-applies nzb.MessageIDIsFetchable and refuses the manifest outright if
// any ID fails. Without that, a manifest persisted before the parse-time
// rule existed would replay an unchecked ID into the command line below.
//
// The contract is therefore: a Message-ID handed to Fetch or Stat is already
// safe to interpolate. See docs/article-validation-contract.md, which argues
// for enforcing a claim at the outermost layer that can decide it and letting
// inner layers assume it — a second check here would be a state that cannot
// occur, which is the class of code that document exists to delete.

// Stat is BODY's cheap cousin: it asks the server whether an article
// exists without transferring the body. Returns nil if present,
// ErrNoArticle (or another sentinel) if not. Useful for capability
// probing and dupe-checking flows.
func (c *Conn) Stat(ctx context.Context, messageID string) error {
	if c.State() != StateReady {
		return ErrInvalidState
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.closeError()
	}

	pc, cmd := newArticleCmd(cmdStat, messageID)
	if err := c.submit(pc, cmd); err != nil {
		return err
	}

	select {
	case <-pc.done:
		if pc.result.err != nil {
			return pc.result.err
		}
		if sentinel := classifyStatus(pc.result.code); sentinel != nil {
			return fmt.Errorf("%w: %d %s", sentinel, pc.result.code, pc.result.line)
		}
		if !pc.result.success { // see Fetch — the reader owns what success means
			return &ServerError{Code: pc.result.code, Text: pc.result.line}
		}
		return nil
	case <-ctx.Done():
		pc.orphaned.Store(true)
		return ctx.Err()
	}
}

// Close terminates the connection. If the Conn is Ready, Close first
// sends QUIT (with a short deadline) so the server can log a clean
// disconnect; any error from that path is ignored in favour of
// surfacing the underlying close reason. Idempotent and safe to call
// from any goroutine.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.cancel != nil {
			c.cancel()
		}
		// Best-effort polite QUIT. Swallow errors — we're tearing
		// down anyway.
		if c.State() == StateReady {
			if c.sendLock.TryLock() {
				_ = c.nc.SetWriteDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck // best-effort during teardown
				_, _ = c.bw.WriteString("QUIT\r\n")                        //nolint:errcheck // best-effort
				_ = c.bw.Flush()                                           //nolint:errcheck // best-effort
				c.sendLock.Unlock()
			}
		}
		_ = c.nc.Close()            //nolint:errcheck // caller gets closeErr via return below
		_ = c.setState(StateClosed) //nolint:errcheck // terminal transition; ignore invalid-state error if already closed

		c.wakeOrphans(ErrClosed)
	})
	<-c.readerDone
	return c.closeErr
}

// idleTimeoutReader wraps a net.Conn and resets the read deadline before
// every Read call, turning an absolute socket deadline into an idle
// timeout. This is critical when speed limiting is active: a large
// article body may take many minutes to transfer, but each individual
// Read should complete within the timeout period.
type idleTimeoutReader struct {
	nc      net.Conn
	timeout time.Duration
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	_ = r.nc.SetReadDeadline(time.Now().Add(r.timeout)) //nolint:errcheck // best-effort; actual idle enforced by Read
	return r.nc.Read(p)
}

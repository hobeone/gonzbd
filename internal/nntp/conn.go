package nntp

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/config"
)

// Sentinel errors. Callers can errors.Is against these to branch on
// error class without string matching.
var (
	// ErrClosed is returned when Fetch/Stat/... are called on a Conn
	// whose socket has been closed, either via Close or by a server
	// disconnect. The underlying cause (if any) is accessible via
	// errors.Unwrap.
	ErrClosed = errors.New("nntp: connection closed")

	// ErrInvalidState is returned when a Conn method is called in a
	// state where it is not valid (e.g. Fetch before Authenticate).
	ErrInvalidState = errors.New("nntp: invalid state")

	// ErrAuthRequired is returned when the server responds with 480
	// "authentication required" to a command. Callers should close
	// the Conn and Dial a fresh one — re-authentication mid-session
	// is supported by the protocol but not implemented here because
	// the downloader prefers to cycle connections on auth failure.
	ErrAuthRequired = errors.New("nntp: authentication required")

	// ErrAuthRejected is returned when AUTHINFO USER or AUTHINFO
	// PASS is rejected (481/482). Usually means bad credentials;
	// the dispatcher applies PENALTY_PERM.
	ErrAuthRejected = errors.New("nntp: authentication rejected")

	// ErrNoArticle is returned when the server responds 430/423 —
	// the article was not found. Callers move on to the next server
	// in the try-list.
	ErrNoArticle = errors.New("nntp: article not available")

	// ErrServerUnavailable is returned for 502/503 — the server is
	// refusing service for the time being. Callers apply PENALTY_502.
	ErrServerUnavailable = errors.New("nntp: server unavailable")

	// ErrTransient is returned for generic 4xx responses without a
	// more specific meaning. Callers typically retry on another
	// connection or apply PENALTY_VERYSHORT.
	ErrTransient = errors.New("nntp: transient server error")
)

// ServerError wraps an unexpected NNTP status code so callers can
// inspect the wire response when logging or debugging. The sentinel
// returned by errors.Unwrap is one of the Err* constants above when
// the code maps to a known category; otherwise it is bare.
type ServerError struct {
	Code int
	Text string
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("nntp: server responded %d %s", e.Code, e.Text)
}

// classifyStatus maps an NNTP status code onto one of the sentinel
// errors so callers can branch without knowing the wire codes.
func classifyStatus(code int) error {
	switch code {
	case 430, 423:
		return ErrNoArticle
	case 480:
		return ErrAuthRequired
	case 481, 482:
		return ErrAuthRejected
	case 502, 503:
		return ErrServerUnavailable
	}
	if code >= 400 && code < 600 {
		return ErrTransient
	}
	return nil
}

// Conn is a live connection to a single NNTP server. It is safe for
// concurrent use by multiple goroutines. See the package doc for the
// overall concurrency model.
//
// A Conn progresses through a linear state machine (see state.go); it
// is usable only while State() == StateReady. Once Close returns the
// Conn is inert — discard it and Dial a fresh one to reconnect.
type Conn struct {
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

	// sslInfo is the negotiated TLS protocol+cipher for UI display.
	// Empty for plain-text connections.
	sslInfo string
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

// dialOptions are the knobs Dial tunes internally. They live in a
// struct instead of separate Dial arguments so callers don't have to
// care about defaults — ServerConfig carries everything.
type dialOptions struct {
	host       string
	port       int
	useTLS     bool
	tlsConfig  *tls.Config
	dialer     *net.Dialer
	pipelining int
	readBuf    int
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
	pipe := cfg.PipeliningRequests
	if pipe < 1 {
		pipe = 1
	}
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
func Dial(ctx context.Context, cfg config.ServerConfig) (*Conn, error) {
	opts, err := newDialOptions(cfg)
	if err != nil {
		return nil, err
	}

	addr := net.JoinHostPort(opts.host, strconv.Itoa(opts.port))
	var nc net.Conn
	if opts.useTLS {
		d := &tls.Dialer{NetDialer: opts.dialer, Config: opts.tlsConfig}
		nc, err = d.DialContext(ctx, "tcp", addr)
	} else {
		nc, err = opts.dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("nntp: dial %s: %w", addr, err)
	}

	c := &Conn{
		cfg:        cfg,
		nc:         nc,
		bw:         bufio.NewWriter(nc),
		br:         bufio.NewReaderSize(nc, opts.readBuf),
		state:      StateDisconnected,
		sem:        make(chan struct{}, opts.pipelining),
		readerDone: make(chan struct{}),
	}

	if tc, ok := nc.(*tls.Conn); ok {
		st := tc.ConnectionState()
		c.sslInfo = fmt.Sprintf("%s / %s", tlsVersionString(st.Version), tls.CipherSuiteName(st.CipherSuite))
	}

	if err := c.handshake(ctx, cfg); err != nil {
		_ = nc.Close() //nolint:errcheck // handshake failed; socket is being torn down regardless
		return nil, err
	}

	go c.runReader()
	return c, nil
}

// handshake runs synchronously on the caller's goroutine before the
// reader loop starts. That simplifies the auth/caps sequence: each
// step reads exactly one response with no FIFO bookkeeping.
func (c *Conn) handshake(ctx context.Context, cfg config.ServerConfig) error {
	if deadline, ok := ctx.Deadline(); ok {
		if err := c.nc.SetDeadline(deadline); err != nil {
			return fmt.Errorf("nntp: set deadline: %w", err)
		}
		defer func() { _ = c.nc.SetDeadline(time.Time{}) }() //nolint:errcheck // clearing deadline on path out; any error is cosmetic
	}

	// Greeting.
	line, err := readResponseLine(c.br)
	if err != nil {
		return fmt.Errorf("nntp: read greeting: %w", err)
	}
	code, text, err := parseStatus(line)
	if err != nil {
		return err
	}
	if code != 200 && code != 201 {
		return &ServerError{Code: code, Text: text}
	}
	if err := c.setState(StateConnected); err != nil {
		return err
	}

	if cfg.Username != "" {
		if err := c.authenticate(cfg.Username, cfg.Password); err != nil {
			return err
		}
	}

	// Capability probe is best-effort: servers that refuse it still
	// work for BODY/STAT via the fallback in capabilities.go.
	c.caps = probeCapabilities(c.bw, c.br)

	// Whether or not we authenticated, the connection is now dispatch-
	// ready. Advance to Ready.
	if err := c.setState(StateReady); err != nil {
		return err
	}
	return nil
}

// authenticate drives the AUTHINFO USER / AUTHINFO PASS dance
// synchronously. On success state advances to Authenticated; on
// failure the sentinel ErrAuthRejected is returned and the Conn is
// unusable (caller should close).
func (c *Conn) authenticate(user, pass string) error {
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

	// Acquire a pipelining slot.
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	pc := &pendingCmd{kind: cmdBody, done: make(chan struct{})}
	cmd := fmt.Appendf(nil, "BODY <%s>\r\n", strings.TrimFunc(messageID, trimAngle))
	if err := c.submit(pc, cmd); err != nil {
		return nil, err
	}

	select {
	case <-pc.done:
		if pc.result.err != nil {
			return nil, pc.result.err
		}
		if sentinel := classifyStatus(pc.result.code); sentinel != nil {
			return nil, fmt.Errorf("%w: %d %s", sentinel, pc.result.code, pc.result.line)
		}
		if pc.result.code != 222 {
			return nil, &ServerError{Code: pc.result.code, Text: pc.result.line}
		}
		return pc.result.body, nil
	case <-ctx.Done():
		// Mark the pending entry orphaned. The reader goroutine
		// will still read+discard the response, freeing the
		// semaphore slot.
		pc.orphaned.Store(true)
		return nil, ctx.Err()
	}
}

// trimAngle removes angle-bracket decoration. Callers may pass
// message IDs with or without brackets; the wire format strictly
// requires them.
func trimAngle(r rune) bool { return r == '<' || r == '>' }

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
	}

	pc := &pendingCmd{kind: cmdStat, done: make(chan struct{})}
	cmd := fmt.Appendf(nil, "STAT <%s>\r\n", strings.TrimFunc(messageID, trimAngle))
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
		if pc.result.code != 223 {
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
		// Best-effort polite QUIT. Swallow errors — we're tearing
		// down anyway.
		if c.state == StateReady {
			_ = c.nc.SetWriteDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck // best-effort during teardown
			_, _ = c.bw.WriteString("QUIT\r\n")                        //nolint:errcheck // best-effort
			_ = c.bw.Flush()                                           //nolint:errcheck // best-effort
		}
		_ = c.nc.Close()            //nolint:errcheck // caller gets closeErr via return below
		_ = c.setState(StateClosed) //nolint:errcheck // terminal transition; ignore invalid-state error if already closed

		// Wake any orphaned callers.
		c.pendingLock.Lock()
		orphans := c.pending
		c.pending = nil
		c.pendingLock.Unlock()
		for _, pc := range orphans {
			pc.result = cmdResult{err: ErrClosed}
			close(pc.done)
		}
	})
	<-c.readerDone
	return c.closeErr
}

// tlsVersionString maps a tls version constant to a human-readable
// name for SSLInfo. Unknown versions render as hex.
func tlsVersionString(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLSv1.3"
	case tls.VersionTLS12:
		return "TLSv1.2"
	case tls.VersionTLS11:
		return "TLSv1.1"
	case tls.VersionTLS10:
		return "TLSv1.0"
	}
	return fmt.Sprintf("TLS(0x%04x)", v)
}

package nntp

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

// Pre-existing helpers in conn.go with no direct test. They are reached
// through Dial today, which means a change to one is only caught if it
// happens to break a full dial — these exercise each at its own level.

// newPipeConn wires a Conn to one end of a net.Pipe, so the helpers below can
// be driven without a listener. The returned server end is the test's script.
func newPipeConn(t *testing.T) (*Conn, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	c := &Conn{
		nc:  client,
		br:  bufio.NewReader(client),
		bw:  bufio.NewWriter(client),
		log: slog.New(slog.DiscardHandler),
	}
	return c, server
}

// startBlockedRead launches a read against c.br in its own goroutine and
// returns a channel that carries its result. The setupHandshakeDeadline
// tests below use it to observe whether a deadline landed on the socket:
// a blocked read means none did, a returned error means one did.
func startBlockedRead(c *Conn) <-chan error {
	result := make(chan error, 1)
	go func() {
		_, err := c.br.ReadString('\n')
		result <- err
	}()
	return result
}

// setState enforces the transition table rather than assigning blindly: a
// rejected transition leaves the previous state intact, which is what stops a
// closed connection from being resurrected into Ready.
func TestSetState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		from    State
		to      State
		wantErr bool
	}{
		{"disconnected to connected", StateDisconnected, StateConnected, false},
		{"connected to ready", StateConnected, StateReady, false},
		{"connected to authenticated", StateConnected, StateAuthenticated, false},
		{"ready to closed", StateReady, StateClosed, false},
		{"disconnected to ready", StateDisconnected, StateReady, true},
		{"closed to ready", StateClosed, StateReady, true},
		{"closed to connected", StateClosed, StateConnected, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Conn{state: tc.from}
			err := c.setState(tc.to)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("setState(%v -> %v) = nil, want an error", tc.from, tc.to)
				}
				if got := c.State(); got != tc.from {
					t.Errorf("state = %v after a rejected transition, want %v unchanged",
						got, tc.from)
				}
				if _, ok := errors.AsType[errInvalidTransition](err); !ok {
					t.Errorf("err = %v, want errInvalidTransition", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("setState(%v -> %v) = %v, want nil", tc.from, tc.to, err)
			}
			if got := c.State(); got != tc.to {
				t.Errorf("state = %v, want %v", got, tc.to)
			}
		})
	}
}

// expectGreeting accepts 200 and 201 and advances to Connected; any other
// code is a ServerError and leaves the state alone.
func TestExpectGreeting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		greeting  string
		wantErr   bool
		wantState State
		wantCode  int
	}{
		{"200 posting allowed", "200 welcome\r\n", false, StateConnected, 0},
		{"201 posting prohibited", "201 no posting\r\n", false, StateConnected, 0},
		{"400 service unavailable", "400 load shedding\r\n", true, StateDisconnected, 400},
		{"502 permanent", "502 no permission\r\n", true, StateDisconnected, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, server := newPipeConn(t)
			go func() { _, _ = server.Write([]byte(tc.greeting)) }()

			err := c.expectGreeting()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("expectGreeting() = %v, want nil", err)
				}
			} else {
				var se *ServerError
				if !errors.As(err, &se) {
					t.Fatalf("expectGreeting() = %v, want a *ServerError", err)
				}
				if se.Code != tc.wantCode {
					t.Errorf("code = %d, want %d", se.Code, tc.wantCode)
				}
			}
			if got := c.State(); got != tc.wantState {
				t.Errorf("state = %v, want %v", got, tc.wantState)
			}
		})
	}
}

// An unreadable greeting is wrapped rather than returned bare, so the caller
// can tell a dead socket from a refusing server.
func TestExpectGreetingReadError(t *testing.T) {
	c, server := newPipeConn(t)
	_ = server.Close()

	err := c.expectGreeting()
	if err == nil {
		t.Fatal("expectGreeting() = nil, want an error")
	}
	if _, ok := errors.AsType[*ServerError](err); ok {
		t.Errorf("err = %v, want a read error rather than a ServerError", err)
	}
}

// setupHandshakeDeadline watches ctx unconditionally — given a deadline, a
// cancellation, or both, it fires once when ctx ends.
func TestSetupHandshakeDeadlineWithDeadline(t *testing.T) {
	c, _ := newPipeConn(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(time.Hour))
	defer cancel()

	cleanup := c.setupHandshakeDeadline(ctx)
	if cleanup == nil {
		t.Fatal("cleanup = nil, want a callable")
	}

	// ctx doesn't end for another hour, so the AfterFunc registration stays
	// dormant and nothing has touched the socket's deadline yet — a read
	// blocks rather than returning immediately.
	blocked := startBlockedRead(c)
	select {
	case err := <-blocked:
		t.Fatalf("read returned %v, want it blocked under a far-future deadline", err)
	case <-time.After(100 * time.Millisecond):
	}
	cleanup()

	// Retire the reader inside the test body rather than leaving it parked on
	// the pipe until t.Cleanup runs. A net.Pipe read with no deadline blocks
	// forever, and cleanup() has just retired the AfterFunc registration, so
	// nothing else will touch the deadline — the test sets one itself.
	if err := c.nc.SetDeadline(time.Now()); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Error("the reader did not unblock; it would have outlived the test body")
	}
}

// Without a context deadline it still watches for cancellation via
// context.AfterFunc, and unblocks the socket when it fires — the case the
// watcher exists for. See TestSetupHandshakeDeadlineWithoutDeadlineCleanupBeatsCancel
// below for the companion case: cleanup() racing a concurrent cancellation.
func TestSetupHandshakeDeadlineWithoutDeadlineUnblocksOnCancel(t *testing.T) {
	c, _ := newPipeConn(t)
	ctx, cancel := context.WithCancel(context.Background())

	cleanup := c.setupHandshakeDeadline(ctx)
	defer cleanup()

	done := startBlockedRead(c)

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("read succeeded, want it unblocked with an error")
		}
	case <-time.After(2 * time.Second):
		t.Error("read stayed blocked after cancellation; the watcher did not fire")
	}
}

// Companion to the case above: a cleanup() that has already returned, with
// cancellation following it (the shape of a real caller — handshake's
// defer cleanup() runs and completes before the caller's own defer
// cancel() fires). The old hand-rolled watcher could still fire after
// cleanup in this exact ordering, because its select didn't know which of
// the two channels became ready first — only that both were ready by the
// time it got scheduled. context.AfterFunc's stop() has no such window: it
// is gated by an internal sync.Once that resolves synchronously inside the
// call, so a cleanup() that has returned has unconditionally retired the
// watcher before cancel() runs (see setupHandshakeDeadline).
func TestSetupHandshakeDeadlineWithoutDeadlineCleanupBeatsCancel(t *testing.T) {
	const (
		iterations = 30
		grace      = 5 * time.Millisecond
	)
	for i := range iterations {
		c, _ := newPipeConn(t)
		ctx, cancel := context.WithCancel(context.Background())
		cleanup := c.setupHandshakeDeadline(ctx)

		cleanup()
		cancel()

		// If the watcher fired after cleanup, the socket now carries a
		// SetDeadline(now) and a read returns immediately with an error. If
		// cleanup won, no deadline was set and the read stays blocked.
		blocked := startBlockedRead(c)
		select {
		case err := <-blocked:
			t.Fatalf("iteration %d: read returned %v, want it blocked — cleanup() should have prevented the watcher's SetDeadline", i, err)
		case <-time.After(grace):
		}

		// Retire the blocked reader before the next iteration / t.Cleanup.
		_ = c.nc.SetDeadline(time.Now())
		<-blocked
	}
}

// handshake composes the three helpers above. Its own contribution is the
// order it runs them in and the auth branch, so this drives the full happy
// path against a scripted server and asserts the connection ends Ready.
func TestHandshakeReachesReady(t *testing.T) {
	ms := newMockServer(t, func(mc *mockConn) {
		mc.send("200 welcome")
		mc.expect("CAPABILITIES")
		mc.sendCaps()
	})

	nc, err := net.Dial("tcp", ms.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })

	c := &Conn{
		nc:  nc,
		br:  bufio.NewReader(nc),
		bw:  bufio.NewWriter(nc),
		log: slog.New(slog.DiscardHandler),
	}
	if err := c.handshake(t.Context(), config.ServerConfig{}); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if got := c.State(); got != StateReady {
		t.Errorf("state = %v, want %v", got, StateReady)
	}
}

// A refused greeting aborts the handshake before any capability probe, so the
// connection never reaches Ready.
func TestHandshakeStopsOnRefusedGreeting(t *testing.T) {
	ms := newMockServer(t, func(mc *mockConn) {
		mc.send("502 no permission")
	})

	nc, err := net.Dial("tcp", ms.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })

	c := &Conn{
		nc:  nc,
		br:  bufio.NewReader(nc),
		bw:  bufio.NewWriter(nc),
		log: slog.New(slog.DiscardHandler),
	}
	err = c.handshake(t.Context(), config.ServerConfig{})
	if err == nil {
		t.Fatal("handshake() = nil, want an error")
	}
	var se *ServerError
	if !errors.As(err, &se) || se.Code != 502 {
		t.Errorf("err = %v, want a 502 ServerError", err)
	}
	if got := c.State(); got == StateReady {
		t.Error("state = Ready after a refused greeting")
	}
}

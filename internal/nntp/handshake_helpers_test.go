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
				var invalid errInvalidTransition
				if !errors.As(err, &invalid) {
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
	var se *ServerError
	if errors.As(err, &se) {
		t.Errorf("err = %v, want a read error rather than a ServerError", err)
	}
}

// setupHandshakeDeadline has two shapes. With a context deadline it sets one
// on the socket and hands back a cleanup that clears it.
func TestSetupHandshakeDeadlineWithDeadline(t *testing.T) {
	c, _ := newPipeConn(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(time.Hour))
	defer cancel()

	cleanup, err := c.setupHandshakeDeadline(ctx)
	if err != nil {
		t.Fatalf("setupHandshakeDeadline: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup = nil, want a callable")
	}

	// The socket carries a deadline an hour out, so a read blocks rather
	// than returning immediately.
	blocked := make(chan error, 1)
	go func() {
		_, err := c.br.ReadString('\n')
		blocked <- err
	}()
	select {
	case err := <-blocked:
		t.Fatalf("read returned %v, want it blocked under a far-future deadline", err)
	case <-time.After(100 * time.Millisecond):
	}
	cleanup()

	// Retire the reader inside the test body rather than leaving it parked on
	// the pipe until t.Cleanup runs. A net.Pipe read with no deadline blocks
	// forever, and cleanup() has just cleared the one that was set.
	if err := c.nc.SetDeadline(time.Now()); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Error("the reader did not unblock; it would have outlived the test body")
	}
}

// Without a context deadline it spawns a watcher that unblocks the socket on
// cancellation — the case the goroutine exists for.
//
// There is deliberately no companion test asserting that cleanup() reliably
// STOPS that watcher, because it does not. cleanup closes `done` while
// cancellation closes ctx.Done(), and when both land together the select
// picks pseudo-randomly, so the watcher can still stamp SetDeadline(now) on a
// connection that is past its handshake and serving fetches. Surfaced
// separately; a test asserting the opposite would flake about half the time,
// and writing one would have converted a real defect into a flaky test.
func TestSetupHandshakeDeadlineWithoutDeadlineUnblocksOnCancel(t *testing.T) {
	c, _ := newPipeConn(t)
	ctx, cancel := context.WithCancel(context.Background())

	cleanup, err := c.setupHandshakeDeadline(ctx)
	if err != nil {
		t.Fatalf("setupHandshakeDeadline: %v", err)
	}
	defer cleanup()

	done := make(chan error, 1)
	go func() {
		_, err := c.br.ReadString('\n')
		done <- err
	}()

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

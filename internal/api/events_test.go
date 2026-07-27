package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/hobeone/gonzbd/internal/config"
)

// ---------- Broadcaster unit tests ----------
//
// These test the Broadcaster's fan-out and overflow logic directly,
// without WebSocket connections (which require an HTTP server). The
// Handle() method is tested implicitly via integration tests.

func TestBroadcaster_NoClients(t *testing.T) {
	t.Parallel()
	b := NewBroadcaster(slog.Default())
	// Should not panic or block.
	b.Broadcast(Event{Type: "test"})
}

func TestBroadcaster_SingleClient(t *testing.T) {
	t.Parallel()
	b := NewBroadcaster(slog.Default())

	c := &client{send: make(chan []byte, 16)}
	b.mu.Lock()
	b.clients[c] = struct{}{}
	b.mu.Unlock()

	b.Broadcast(Event{Type: "status", Speed: 42})

	select {
	case msg := <-c.send:
		var ev Event
		if err := json.Unmarshal(msg, &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if ev.Type != "status" {
			t.Errorf("event type = %q, want %q", ev.Type, "status")
		}
		if ev.Speed != 42 {
			t.Errorf("speed = %d, want 42", ev.Speed)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for broadcast")
	}
}

func TestBroadcaster_MultipleClients(t *testing.T) {
	t.Parallel()
	b := NewBroadcaster(slog.Default())

	const n = 5
	clients := make([]*client, n)
	for i := range n {
		clients[i] = &client{send: make(chan []byte, 16)}
		b.mu.Lock()
		b.clients[clients[i]] = struct{}{}
		b.mu.Unlock()
	}

	b.Broadcast(Event{Type: "speed_update"})

	for i, c := range clients {
		select {
		case msg := <-c.send:
			var ev Event
			if err := json.Unmarshal(msg, &ev); err != nil {
				t.Errorf("client %d: unmarshal: %v", i, err)
			}
			if ev.Type != "speed_update" {
				t.Errorf("client %d: type = %q, want %q", i, ev.Type, "speed_update")
			}
		case <-time.After(time.Second):
			t.Errorf("client %d: timed out", i)
		}
	}
}

func TestBroadcaster_BufferOverflow(t *testing.T) {
	t.Parallel()
	b := NewBroadcaster(slog.Default())

	// Create a client with buffer size 1 (small buffer to trigger overflow).
	c := &client{send: make(chan []byte, 1)}
	b.mu.Lock()
	b.clients[c] = struct{}{}
	b.mu.Unlock()

	// First broadcast fills the buffer.
	b.Broadcast(Event{Type: "first"})

	// Second broadcast should overflow the buffer and disconnect the client.
	b.Broadcast(Event{Type: "second"})

	// The client should have been removed from the broadcaster.
	b.mu.RLock()
	_, exists := b.clients[c]
	b.mu.RUnlock()

	if exists {
		t.Error("overflowed client should have been removed")
	}

	// The client's send channel should be closed.
	_, ok := <-c.send // drain the first message
	if !ok {
		// Channel already closed (race between drain and close) — also acceptable.
		return
	}
	_, ok = <-c.send
	if ok {
		t.Error("expected send channel to be closed after overflow")
	}
}

func TestBroadcaster_ConcurrentBroadcasts(t *testing.T) {
	t.Parallel()
	b := NewBroadcaster(slog.Default())

	c := &client{send: make(chan []byte, 100)}
	b.mu.Lock()
	b.clients[c] = struct{}{}
	b.mu.Unlock()

	// Launch concurrent broadcasts — must not race.
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			b.Broadcast(Event{Type: "concurrent", Speed: int64(i)})
		})
	}
	wg.Wait()

	// Drain and count.
	close(c.send)
	var count int
	for range c.send {
		count++
	}
	if count != 20 {
		t.Errorf("received %d events, want 20", count)
	}
}

func TestBroadcaster_ConcurrentEvents(t *testing.T) {
	t.Parallel()

	t.Run("ConcurrentRLock", func(t *testing.T) {
		b := NewBroadcaster(slog.Default())
		c := &client{send: make(chan []byte, 10)}
		b.mu.Lock()
		b.clients[c] = struct{}{}
		b.mu.Unlock()

		// Hold RLock to simulate an ongoing read operation (e.g. NumClients or another broadcast).
		b.mu.RLock()
		defer b.mu.RUnlock()

		done := make(chan struct{})
		go func() {
			// If Broadcast attempts to take a full Lock(), this will deadlock/block
			// until RUnlock is called. Under RLock(), it will succeed immediately.
			b.Broadcast(Event{Type: "test", Speed: 100})
			close(done)
		}()

		select {
		case <-done:
			// Success: read broadcast ran concurrently under RLock without starvation or deadlock.
		case <-time.After(200 * time.Millisecond):
			t.Fatal("Broadcast blocked while RLock was held, indicating it requires full Lock() instead of RLock()")
		}
	})

	// RACE-ONLY: this subtest has no assertion of its own — its only
	// signal is `go test -race` catching a data race under concurrent
	// subscribe/unsubscribe/broadcast load. It relies entirely on the
	// project's mandatory `-race` gate; without it, this subtest always
	// passes regardless of whether a race exists.
	t.Run("HighLoadConcurrentBroadcastAndSubscribe", func(t *testing.T) {
		b := NewBroadcaster(slog.New(slog.DiscardHandler))

		var wg sync.WaitGroup
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		// 10 goroutines continuously subscribing and unsubscribing.
		for range 10 {
			wg.Go(func() {
				for {
					select {
					case <-ctx.Done():
						return
					default:
						c := &client{send: make(chan []byte, 5)}
						b.mu.Lock()
						b.clients[c] = struct{}{}
						b.mu.Unlock()

						time.Sleep(2 * time.Millisecond)

						b.mu.Lock()
						delete(b.clients, c)
						b.mu.Unlock()
					}
				}
			})
		}

		// 20 goroutines continuously broadcasting under high load.
		for i := range 20 {
			wg.Go(func() {
				for {
					select {
					case <-ctx.Done():
						return
					default:
						b.Broadcast(Event{Type: "high_load", Speed: int64(i)})
						time.Sleep(1 * time.Millisecond)
					}
				}
			})
		}

		// 5 goroutines subscribing with small buffers that will intentionally overflow and trigger cleanup.
		for range 5 {
			wg.Go(func() {
				for {
					select {
					case <-ctx.Done():
						return
					default:
						c := &client{send: make(chan []byte, 1)} // small buffer will overflow quickly
						b.mu.Lock()
						b.clients[c] = struct{}{}
						b.mu.Unlock()

						time.Sleep(5 * time.Millisecond)
					}
				}
			})
		}

		wg.Wait()
	})
}

func TestBroadcaster_Handle(t *testing.T) {
	b := NewBroadcaster(slog.Default())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Handle(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	defer conn.Close(websocket.StatusInternalError, "done")

	// Wait for client to be registered in broadcaster.
	waitClientRegister(ctx, t, b)

	// Broadcast an event
	b.Broadcast(Event{Type: "ws_test", Speed: 100})

	// Read from connection
	_, msg, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	var ev Event
	if err := json.Unmarshal(msg, &ev); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if ev.Type != "ws_test" || ev.Speed != 100 {
		t.Errorf("unexpected event: %+v", ev)
	}
}

// TestBroadcaster_Handle_CrossOriginRejected proves the WebSocket upgrade
// enforces the coder/websocket library's same-origin check independent of
// the application's callerLevel/isCrossOrigin logic (SEC-5). A handshake
// carrying an Origin header for a different host than the request's Host
// must be rejected at the library layer.
func TestBroadcaster_Handle_CrossOriginRejected(t *testing.T) {
	b := NewBroadcaster(slog.Default())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Handle(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Origin": {"http://evil.example.com"},
		},
	})
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err == nil {
		t.Fatal("expected cross-origin websocket dial to fail, but it succeeded")
	}
	if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d; want 403 Forbidden", resp.StatusCode)
	}

	// The client must never have been registered.
	if n := b.NumClients(); n != 0 {
		t.Errorf("cross-origin client was registered: NumClients() = %d", n)
	}
}

// TestBroadcaster_Handle_SameOriginAccepted proves same-origin handshakes
// (the SPA's own connection, where Origin matches Host, or no Origin header
// at all as sent by non-browser clients) still succeed after removing
// InsecureSkipVerify.
func TestBroadcaster_Handle_SameOriginAccepted(t *testing.T) {
	b := NewBroadcaster(slog.Default())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Handle(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Origin": {srv.URL},
		},
	})
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		t.Fatalf("same-origin websocket dial failed: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "done") }()
}

// TestBroadcaster_Handle_BufferOverflow exercises the Handle write loop's
// overflow teardown: when a connected client stops reading, its send channel
// fills, Broadcast closes the channel and deregisters the client, and the
// write loop tears the connection down — either by observing the closed
// channel (ok == false) or by hitting a write error/timeout. Both are the
// same real behavior and neither is reached by the happy-path Handle tests.
func TestBroadcaster_Handle_BufferOverflow(t *testing.T) {
	b := NewBroadcaster(slog.Default())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Handle(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "done") }()

	// Wait for the client to register with the broadcaster.
	waitClientRegister(ctx, t, b)

	// The client deliberately never reads. Broadcast large events until the
	// OS socket write buffer and the 16-slot send channel both fill; the next
	// Broadcast then overflows, closing the channel and deregistering the
	// client. The 4 KiB payload guarantees the socket buffer fills well within
	// the loop regardless of platform buffer autotuning — a tiny payload can
	// be fully absorbed by a multi-megabyte autotuned buffer and never
	// overflow.
	big := strings.Repeat("x", 4096)
	const maxBroadcasts = 20000
	sent := 0
	for ; sent < maxBroadcasts && b.NumClients() != 0; sent++ {
		b.Broadcast(Event{Type: "overflow_test", Line: big})
	}

	if n := b.NumClients(); n != 0 {
		t.Fatalf("client did not overflow after %d broadcasts; NumClients() = %d", sent, n)
	}

	// Drain the connection until the server tears it down (StatusGoingAway on
	// the closed-channel branch, or a write-error close). A read error here is
	// the expected terminal state.
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			break
		}
	}
}

func TestBroadcaster_HandleClientDisconnect(t *testing.T) {
	b := NewBroadcaster(slog.Default())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Handle(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}

	// Wait for connection to be registered.
	waitClientRegister(ctx, t, b)

	b.mu.RLock()
	clientCount := len(b.clients)
	b.mu.RUnlock()
	if clientCount != 1 {
		t.Errorf("expected 1 client, got %d", clientCount)
	}

	// Close from client side
	if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Errorf("conn.Close: %v", err)
	}

	// Wait for handler to detect disconnect and clean up.
	ctxPoll2, cancelPoll2 := context.WithTimeout(ctx, 2*time.Second)
	defer cancelPoll2()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for b.NumClients() > 0 {
		select {
		case <-ctxPoll2.Done():
			t.Fatal("timeout waiting for client disconnect cleanup")
		case <-ticker.C:
		}
	}

	b.mu.RLock()
	clientCount = len(b.clients)
	b.mu.RUnlock()
	if clientCount != 0 {
		t.Error("expected client to be removed from broadcaster after disconnect")
	}
}

func (h *recordHandler) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

func (h *recordHandler) hasMessage(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message == msg {
			return true
		}
	}
	return false
}

func waitDisconnectLog(ctx context.Context, t *testing.T, rec *recordHandler) {
	t.Helper()
	if rec.hasMessage("WebSocket client disconnected") {
		return
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("timeout waiting for client disconnect log")
		case <-ticker.C:
			if rec.hasMessage("WebSocket client disconnected") {
				return
			}
		}
	}
}

func waitClientRegister(ctx context.Context, t *testing.T, b *Broadcaster) {
	t.Helper()
	if b.NumClients() > 0 {
		return
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for b.NumClients() == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("timeout waiting for client registration")
		case <-ticker.C:
		}
	}
}

func TestBroadcaster_DebugLogging(t *testing.T) {
	t.Parallel()

	rec := &recordHandler{}
	logger := slog.New(rec)
	b := NewBroadcaster(logger)

	// Broadcast with 0 clients: should NOT log debug message.
	b.Broadcast(Event{Type: "no_clients"})
	if got := rec.len(); got != 0 {
		t.Errorf("expected 0 log records with no clients, got %d", got)
	}

	// Add 1 client.
	c := &client{send: make(chan []byte, 10)}
	b.mu.Lock()
	b.clients[c] = struct{}{}
	b.mu.Unlock()

	// Broadcast with 1 client: should log debug message.
	b.Broadcast(Event{Type: "with_client"})
	if got := rec.len(); got != 1 {
		t.Errorf("expected 1 log record with 1 client, got %d", got)
	}
}

func TestClientConnectionDetails(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	b := NewBroadcaster(logger)

	tests := []struct {
		name       string
		remoteAddr string
		expectedIP string
	}{
		{
			name:       "IPv4 with port",
			remoteAddr: "192.168.1.5:54321",
			expectedIP: "192.168.1.5",
		},
		{
			name:       "IPv6 with port",
			remoteAddr: "[2001:db8::1]:12345",
			expectedIP: "2001:db8::1",
		},
		{
			name:       "Bare IP without port",
			remoteAddr: "10.0.0.1",
			expectedIP: "10.0.0.1",
		},
		{
			name:       "Empty string fallback",
			remoteAddr: "",
			expectedIP: "",
		},
		{
			name:       "Whitespace string fallback",
			remoteAddr: "   ",
			expectedIP: "",
		},
	}

	// Verify connection ID incrementing
	c1 := &client{
		id:   b.nextID.Add(1),
		send: make(chan []byte, 16),
	}
	if c1.id != 1 {
		t.Errorf("got connection ID %d, want 1", c1.id)
	}

	c2 := &client{
		id:   b.nextID.Add(1),
		send: make(chan []byte, 16),
	}
	if c2.id != 2 {
		t.Errorf("got connection ID %d, want 2", c2.id)
	}

	// Verify IP extraction
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/ws", nil)
			req.RemoteAddr = tc.remoteAddr
			got := remoteIP(req)
			if got != tc.expectedIP {
				t.Errorf("remoteIP(%q) = %q, want %q", tc.remoteAddr, got, tc.expectedIP)
			}
		})
	}
}

// ---------- Server handleWS tests ----------

func TestHandleWS(t *testing.T) {
	t.Parallel()

	t.Run("forbidden without apikey", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		s := testServerWithConfig(t, cfg)
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/ws", nil)
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("status = %d; want 403 Forbidden", rr.Code)
		}
	})

	t.Run("forbidden with wrong apikey", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		s := testServerWithConfig(t, cfg)
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/ws?apikey=wrong_key", nil)
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("status = %d; want 403 Forbidden", rr.Code)
		}
	})

	t.Run("success with valid apikey", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		s := testServerWithConfig(t, cfg)
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws?apikey=" + testAPIKey
		conn, resp, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatalf("websocket dial failed: %v", err)
		}
		if resp != nil && resp.Body != nil {
			defer func() { _ = resp.Body.Close() }()
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "done") }()

		// Wait for connection to be registered on server.
		waitClientRegister(ctx, t, s.EventBroadcaster())

		// Broadcast an event and verify client receives it.
		s.EventBroadcaster().Broadcast(Event{Type: "test_ws_event", Speed: 1234})

		_, msg, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("conn.Read: %v", err)
		}
		var ev Event
		if err := json.Unmarshal(msg, &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if ev.Type != "test_ws_event" {
			t.Errorf("event type = %q; want test_ws_event", ev.Type)
		}
		if ev.Speed != 1234 {
			t.Errorf("speed = %d; want 1234", ev.Speed)
		}
	})

	t.Run("success with valid nzbkey", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Default()
		if err != nil {
			t.Fatalf("Default(): %v", err)
		}
		s := testServerWithConfig(t, cfg)
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws?apikey=" + testNZBKey
		conn, resp, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatalf("websocket dial with nzbkey failed: %v", err)
		}
		if resp != nil && resp.Body != nil {
			defer func() { _ = resp.Body.Close() }()
		}
		if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
			t.Errorf("conn.Close: %v", err)
		}
	})
}

func TestWebSocketLifecycleLogging(t *testing.T) {
	rec := &recordHandler{}
	logger := slog.New(rec)
	b := NewBroadcaster(logger)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Handle(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	// Close cleanly from client side
	if err := conn.Close(websocket.StatusNormalClosure, "clean exit"); err != nil {
		t.Errorf("conn.Close: %v", err)
	}

	// Wait for client disconnect to be cleaned up and logged on the server
	waitDisconnectLog(ctx, t, rec)

	// Verify log records
	rec.mu.Lock()
	defer rec.mu.Unlock()

	var hasConnected, hasDisconnected bool
	for _, r := range rec.records {
		if r.Message == "WebSocket client connected" {
			hasConnected = true
			var connID uint64
			var ip string
			r.Attrs(func(attr slog.Attr) bool {
				if attr.Key == "connection_id" {
					connID = attr.Value.Uint64()
				}
				if attr.Key == "remote_ip" {
					ip = attr.Value.String()
				}
				return true
			})
			if connID == 0 {
				t.Error("connected log missing connection_id")
			}
			if ip == "" {
				t.Error("connected log missing remote_ip")
			}
		}
		if r.Message == "WebSocket client disconnected" {
			hasDisconnected = true
			var connID uint64
			var ip, reason string
			var duration time.Duration
			r.Attrs(func(attr slog.Attr) bool {
				switch attr.Key {
				case "connection_id":
					connID = attr.Value.Uint64()
				case "remote_ip":
					ip = attr.Value.String()
				case "reason":
					reason = attr.Value.String()
				case "duration":
					duration = attr.Value.Duration()
				}
				return true
			})
			if connID == 0 {
				t.Error("disconnected log missing connection_id")
			}
			if ip == "" {
				t.Error("disconnected log missing remote_ip")
			}
			if reason != "close status 1000 (StatusNormalClosure)" {
				t.Errorf("disconnected log got unexpected reason: %q", reason)
			}
			if duration <= 0 {
				t.Errorf("disconnected log got invalid duration: %v", duration)
			}
		}
	}

	if !hasConnected {
		t.Error("missing connected log line")
	}
	if !hasDisconnected {
		t.Error("missing disconnected log line")
	}
}

func TestWebSocketLifecycleLogging_BufferOverflow(t *testing.T) {
	rec := &recordHandler{}
	logger := slog.New(rec)
	b := NewBroadcaster(logger)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Handle(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// Wait for client to register
	waitClientRegister(ctx, t, b)

	// Send messages until buffer overflow
	big := strings.Repeat("x", 4096)
	for b.NumClients() > 0 {
		select {
		case <-ctx.Done():
			t.Fatal("timeout waiting for buffer overflow")
		default:
			b.Broadcast(Event{Type: "overflow", Line: big})
			time.Sleep(1 * time.Millisecond)
		}
	}

	// Now that the broadcaster has detected the overflow and closed the channel,
	// start reading from the client side to unblock the server's blocked conn.Write.
	go func() {
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				break
			}
		}
	}()

	// Wait for disconnect log to appear to avoid race condition
	waitDisconnectLog(ctx, t, rec)

	// Verify log records
	rec.mu.Lock()
	defer rec.mu.Unlock()

	var hasBufferFull, hasDisconnected bool
	var disconnectReason string
	for _, r := range rec.records {
		if r.Message == "WebSocket client buffer full, disconnecting" {
			hasBufferFull = true
			var connID uint64
			var ip string
			r.Attrs(func(attr slog.Attr) bool {
				if attr.Key == "connection_id" {
					connID = attr.Value.Uint64()
				}
				if attr.Key == "remote_ip" {
					ip = attr.Value.String()
				}
				return true
			})
			if connID == 0 {
				t.Error("buffer full log missing connection_id")
			}
			if ip == "" {
				t.Error("buffer full log missing remote_ip")
			}
		}
		if r.Message == "WebSocket client disconnected" {
			hasDisconnected = true
			r.Attrs(func(attr slog.Attr) bool {
				if attr.Key == "reason" {
					disconnectReason = attr.Value.String()
				}
				return true
			})
		}
	}

	if !hasBufferFull {
		t.Error("missing buffer full log line")
	}
	if !hasDisconnected {
		t.Error("missing disconnected log line")
	}
	if disconnectReason != "buffer overflow" {
		t.Errorf("unexpected disconnect reason for overflow: got %q, want \"buffer overflow\"", disconnectReason)
	}
}

func TestWebSocketLifecycleLogging_AbnormalClose(t *testing.T) {
	rec := &recordHandler{}
	logger := slog.New(rec)
	b := NewBroadcaster(logger)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Handle(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	// Close the connection abruptly from client side without a clean websocket close handshake.
	if err := conn.CloseNow(); err != nil {
		t.Errorf("conn.CloseNow: %v", err)
	}

	// Wait for client disconnect to be cleaned up and logged on the server
	waitDisconnectLog(ctx, t, rec)

	// Verify log records
	rec.mu.Lock()
	defer rec.mu.Unlock()

	var hasDisconnected bool
	var disconnectReason string
	for _, r := range rec.records {
		if r.Message == "WebSocket client disconnected" {
			hasDisconnected = true
			r.Attrs(func(attr slog.Attr) bool {
				if attr.Key == "reason" {
					disconnectReason = attr.Value.String()
				}
				return true
			})
		}
	}

	if !hasDisconnected {
		t.Error("missing disconnected log line")
	}
	// An abrupt client-side close races two independent detectors in
	// Handle: the read goroutine's conn.Read (which normally surfaces the
	// frame-level EOF) against the write loop's ctx.Done() case, which
	// fires directly whenever r.Context() is canceled by net/http's own
	// connection tracking of the same TCP close. Since conn.Read is itself
	// bound by that same ctx, either detector can legitimately win
	// depending on OS/goroutine scheduling — this is not deterministic and
	// varies under CI load, so both outcomes are accepted.
	wantReasons := map[string]bool{
		"failed to get reader: failed to read frame header: EOF": true,
		"context canceled": true,
	}
	if !wantReasons[disconnectReason] {
		t.Errorf("abnormal disconnect reason got: %q, want one of %v", disconnectReason, wantReasons)
	}
}

func TestWebSocketLifecycleLogging_WriteError(t *testing.T) {
	rec := &recordHandler{}
	logger := slog.New(rec)
	b := NewBroadcaster(logger)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Handle(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	// Wait for client to register.
	waitClientRegister(ctx, t, b)

	// Close the connection abruptly from client side.
	if err := conn.CloseNow(); err != nil {
		t.Errorf("conn.CloseNow: %v", err)
	}

	// Immediately broadcast a message. The write loop will try to write it
	// to the closed connection and hit a write error.
	b.Broadcast(Event{Type: "test_write_error"})

	// Wait for client disconnect to be cleaned up and logged on the server
	waitDisconnectLog(ctx, t, rec)

	// Verify log records contains write error or EOF
	rec.mu.Lock()
	defer rec.mu.Unlock()

	var hasDisconnected bool
	var disconnectReason string
	for _, r := range rec.records {
		if r.Message == "WebSocket client disconnected" {
			hasDisconnected = true
			r.Attrs(func(attr slog.Attr) bool {
				if attr.Key == "reason" {
					disconnectReason = attr.Value.String()
				}
				return true
			})
		}
	}

	if !hasDisconnected {
		t.Error("missing disconnected log line")
	}
	if !strings.HasPrefix(disconnectReason, "write error:") && !strings.Contains(disconnectReason, "EOF") {
		t.Errorf("expected write error or EOF, got: %q", disconnectReason)
	}
}

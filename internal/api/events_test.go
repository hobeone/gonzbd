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
	ctxPoll, cancelPoll := context.WithTimeout(ctx, 2*time.Second)
	for b.NumClients() == 0 {
		select {
		case <-ctxPoll.Done():
			cancelPoll()
			t.Fatal("timeout waiting for client registration in broadcaster")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancelPoll()

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
	ctxPoll, cancelPoll := context.WithTimeout(ctx, 2*time.Second)
	for b.NumClients() == 0 {
		select {
		case <-ctxPoll.Done():
			cancelPoll()
			t.Fatal("timeout waiting for client registration")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancelPoll()

	b.mu.RLock()
	clientCount := len(b.clients)
	b.mu.RUnlock()
	if clientCount != 1 {
		t.Errorf("expected 1 client, got %d", clientCount)
	}

	// Close from client side
	_ = conn.Close(websocket.StatusNormalClosure, "done")

	// Wait for handler to detect disconnect and clean up.
	ctxPoll2, cancelPoll2 := context.WithTimeout(ctx, 2*time.Second)
	for b.NumClients() > 0 {
		select {
		case <-ctxPoll2.Done():
			cancelPoll2()
			t.Fatal("timeout waiting for client disconnect cleanup")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancelPoll2()

	b.mu.RLock()
	clientCount = len(b.clients)
	b.mu.RUnlock()
	if clientCount != 0 {
		t.Error("expected client to be removed from broadcaster after disconnect")
	}
}

type recordHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *recordHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *recordHandler) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
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

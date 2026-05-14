package api

import (
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"
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

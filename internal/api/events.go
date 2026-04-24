package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

// Event represents a message sent over the WebSocket.
type Event struct {
	Type      string `json:"event"`
	Speed     int64  `json:"speed,omitempty"`
	Remaining int64  `json:"remaining,omitempty"`
}

// Broadcaster manages active WebSocket connections and distributes events.
type Broadcaster struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
}

type client struct {
	send chan []byte
}

// NewBroadcaster constructs a new event Broadcaster.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		clients: make(map[*client]struct{}),
	}
}

// Broadcast sends an event to all connected clients.
func (b *Broadcaster) Broadcast(event Event) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if len(b.clients) > 0 {
		slog.Info("WebSocket broadcast", "event", event.Type, "clients", len(b.clients))
	}

	for c := range b.clients {
		select {
		case c.send <- data:
		default:
			// Client's buffer is full; they will be cleaned up in Handle.
		}
	}
}

// Handle upgrades the HTTP connection and manages the client lifecycle.
func (b *Broadcaster) Handle(w http.ResponseWriter, r *http.Request) {
	opts := &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	}
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		slog.Error("WebSocket accept failed", "err", err)
		return
	}
	defer conn.Close(websocket.StatusInternalError, "closing")

	c := &client{
		send: make(chan []byte, 16),
	}

	b.mu.Lock()
	b.clients[c] = struct{}{}
	b.mu.Unlock()

	slog.Info("WebSocket client connected", "remote", r.RemoteAddr)

	defer func() {
		b.mu.Lock()
		delete(b.clients, c)
		b.mu.Unlock()
		slog.Info("WebSocket client disconnected", "remote", r.RemoteAddr)
	}()

	// Read loop (keep-alive/wait for close)
	ctx := r.Context()
	go func() {
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	}()

	// Write loop
	for {
		select {
		case msg := <-c.send:
			err := conn.Write(ctx, websocket.MessageText, msg)
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

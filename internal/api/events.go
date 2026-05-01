package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Event represents a message sent over the WebSocket.
type Event struct {
	Type          string `json:"event"`
	Speed         int64  `json:"speed,omitempty"`
	Remaining     int64  `json:"remaining,omitempty"`
	SpeedLimit    int64  `json:"speed_limit"`
	BandwidthMax  int64  `json:"bandwidth_max"`
	BandwidthPerc int    `json:"bandwidth_perc"`
}

// Broadcaster manages active WebSocket connections and distributes events.
type Broadcaster struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
	log     *slog.Logger
}

type client struct {
	send chan []byte
}

// NewBroadcaster constructs a new event Broadcaster.
func NewBroadcaster(log *slog.Logger) *Broadcaster {
	return &Broadcaster{
		clients: make(map[*client]struct{}),
		log:     log,
	}
}

// Broadcast sends an event to all connected clients.
func (b *Broadcaster) Broadcast(event Event) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.clients) > 0 {
		b.log.Debug("WebSocket broadcast", "event", event.Type, "clients", len(b.clients))
	}

	for c := range b.clients {
		select {
		case c.send <- data:
		default:
			// Client's buffer is full — close the channel to force the
			// write loop to exit and the client to reconnect. This
			// prevents permanent UI desync from silently dropped events.
			close(c.send)
			delete(b.clients, c)
			b.log.Warn("WebSocket client buffer full, disconnecting")
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
		b.log.Error("WebSocket accept failed", "err", err)
		return
	}
	defer conn.Close(websocket.StatusInternalError, "closing") //nolint:errcheck // best-effort close on exit

	c := &client{
		send: make(chan []byte, 16),
	}

	b.mu.Lock()
	b.clients[c] = struct{}{}
	b.mu.Unlock()

	b.log.Info("WebSocket client connected", "remote", r.RemoteAddr)

	defer func() {
		b.mu.Lock()
		delete(b.clients, c)
		b.mu.Unlock()
		b.log.Info("WebSocket client disconnected", "remote", r.RemoteAddr)
	}()

	// Read loop (keep-alive/wait for close). Cancels ctx when the
	// client disconnects so the write loop exits promptly.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		defer cancel() // signal write loop on disconnect
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	}()

	// Write loop with periodic pings to detect dead connections.
	// Without pings, a dead TCP connection (no FIN) keeps the read
	// goroutine and client struct alive indefinitely.
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				// Channel closed by Broadcast (buffer overflow) —
				// close connection so UI reconnects.
				_ = conn.Close(websocket.StatusGoingAway, "buffer overflow") //nolint:errcheck // best-effort close on disconnect
				return
			}
			writeCtx, writeCancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Write(writeCtx, websocket.MessageText, msg)
			writeCancel()
			if err != nil {
				return
			}
		case <-pingTicker.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				b.log.Debug("WebSocket ping failed, closing", "err", err)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/hobeone/gonzbd/internal/downloader"
)

// Event represents a message sent over the WebSocket.
type Event struct {
	Type          string                      `json:"event"`
	Speed         int64                       `json:"speed,omitempty"`
	Remaining     int64                       `json:"remaining,omitempty"`
	SpeedLimit    int64                       `json:"speed_limit"`
	BandwidthMax  int64                       `json:"bandwidth_max"`
	BandwidthPerc int                         `json:"bandwidth_perc"`
	NzoID         string                      `json:"nzo_id,omitempty"`
	Tool          string                      `json:"tool,omitempty"`  // subprocess tool name (par2, unrar, 7z, script)
	Line          string                      `json:"line,omitempty"`  // single output line from subprocess
	Stage         string                      `json:"stage,omitempty"` // pipeline stage name (repair, unpack)
	Servers       []downloader.ServerSnapshot `json:"servers,omitempty"`
}

// Broadcaster manages active WebSocket connections and distributes events.
type Broadcaster struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
	log     *slog.Logger
	nextID  atomic.Uint64
}

type client struct {
	id          uint64
	ip          string
	send        chan []byte
	mu          sync.Mutex
	closeReason string
}

func (c *client) setCloseReason(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeReason == "" {
		c.closeReason = reason
	}
}

func (c *client) getCloseReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeReason
}

// NewBroadcaster constructs a new event Broadcaster.
func NewBroadcaster(log *slog.Logger) *Broadcaster {
	return &Broadcaster{
		clients: make(map[*client]struct{}),
		log:     log,
	}
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return strings.TrimSpace(host)
}

// Broadcast sends an event to all connected clients.
func (b *Broadcaster) Broadcast(event Event) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}

	b.mu.RLock()
	numClients := len(b.clients)
	var overflow []*client
	for c := range b.clients {
		select {
		case c.send <- data:
		default:
			overflow = append(overflow, c)
		}
	}
	b.mu.RUnlock()
	// --- No lock held below this line ---
	if numClients > 0 {
		b.log.Debug("WebSocket broadcast", "event", event.Type, "clients", numClients)
	}

	if len(overflow) > 0 {
		type disconnected struct {
			connID   uint64
			remoteIP string
		}
		var closed []disconnected
		b.mu.Lock()
		for _, c := range overflow {
			if _, exists := b.clients[c]; exists {
				// Client's buffer is full — close the channel to force the
				// write loop to exit and the client to reconnect. This
				// prevents permanent UI desync from silently dropped events.
				c.setCloseReason("buffer overflow")
				close(c.send)
				delete(b.clients, c)
				closed = append(closed, disconnected{connID: c.id, remoteIP: c.ip})
			}
		}
		b.mu.Unlock()
		// --- No lock held below this line ---
		for _, d := range closed {
			b.log.Warn("WebSocket client buffer full, disconnecting", "connection_id", d.connID, "remote_ip", d.remoteIP)
		}
	}
}

// Handle upgrades the HTTP connection and manages the client lifecycle.
func (b *Broadcaster) Handle(w http.ResponseWriter, r *http.Request) {
	// Leave OriginPatterns empty and InsecureSkipVerify false so the
	// coder/websocket library enforces its default same-origin check
	// (Origin host must match Request.Host, or no Origin header is
	// present at all — the case for non-browser API clients). This is a
	// defense-in-depth control independent of callerLevel/isCrossOrigin.
	opts := &websocket.AcceptOptions{}
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		b.log.Error("WebSocket accept failed", "err", err)
		return
	}
	defer conn.Close(websocket.StatusInternalError, "closing") //nolint:errcheck // best-effort close on exit

	c := &client{
		id:   b.nextID.Add(1),
		ip:   remoteIP(r),
		send: make(chan []byte, 16),
	}

	startTime := time.Now()

	b.mu.Lock()
	b.clients[c] = struct{}{}
	b.mu.Unlock()

	b.log.Info("WebSocket client connected", "connection_id", c.id, "remote_ip", c.ip)

	defer func() {
		b.mu.Lock()
		delete(b.clients, c)
		b.mu.Unlock()

		reason := c.getCloseReason()
		if reason == "" {
			reason = "unknown"
		}
		b.log.Info("WebSocket client disconnected",
			"connection_id", c.id,
			"remote_ip", c.ip,
			"duration", time.Since(startTime),
			"reason", reason,
		)
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
				if status := websocket.CloseStatus(err); status != -1 {
					c.setCloseReason(fmt.Sprintf("close status %d (%s)", status, status.String()))
				} else {
					c.setCloseReason(err.Error())
				}
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
				c.setCloseReason("buffer overflow")
				// Channel closed by Broadcast (buffer overflow) —
				// close connection so UI reconnects.
				_ = conn.Close(websocket.StatusGoingAway, "buffer overflow") //nolint:errcheck // best-effort close on disconnect
				return
			}
			writeCtx, writeCancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Write(writeCtx, websocket.MessageText, msg)
			writeCancel()
			if err != nil {
				c.setCloseReason("write error: " + err.Error())
				return
			}
		case <-pingTicker.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				c.setCloseReason("ping failed")
				b.log.Debug("WebSocket ping failed, closing", "err", err)
				return
			}
		case <-ctx.Done():
			c.setCloseReason("context canceled")
			return
		}
	}
}

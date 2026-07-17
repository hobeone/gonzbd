package app

import (
	"context"
	"time"

	"github.com/hobeone/gonzbd/internal/downloader"
)

// FileComplete is emitted when a file assembly is finished.
type FileComplete struct {
	JobID   string
	FileIdx int
	// CRC32 is the whole-file CRC32 computed by the assembler from
	// per-article CRCs combined in offset order. Zero if unavailable
	// (e.g. UU-encoded articles or failed articles).
	CRC32 uint32
}

// JobComplete is emitted when all files in a job are assembled.
type JobComplete struct {
	JobID string
}

// PostProcComplete is emitted when post-processing finished.
type PostProcComplete struct {
	JobID string
}

// EventEmitter defines the interface for broadcasting real-time events.
type EventEmitter interface {
	Broadcast(event Event)
}

// Event represents a real-time notification sent to the UI.
type Event struct {
	Type          string                      `json:"event"`
	Speed         int64                       `json:"speed,omitempty"`
	Remaining     int64                       `json:"remaining,omitempty"`
	SpeedLimit    int64                       `json:"speed_limit"`
	BandwidthMax  int64                       `json:"bandwidth_max"`
	BandwidthPerc int                         `json:"bandwidth_perc"`
	NzoID         string                      `json:"nzo_id,omitempty"`
	Tool          string                      `json:"tool,omitempty"`
	Line          string                      `json:"line,omitempty"`
	Stage         string                      `json:"stage,omitempty"`
	Servers       []downloader.ServerSnapshot `json:"servers,omitempty"`
}

type dummyEmitter struct{}

func (d dummyEmitter) Broadcast(_ Event) {} //nocover: no-op interface stub

// emit broadcasts e through the currently-registered emitter. The emitter
// pointer is read under app.mu so a concurrent SetEmitter (e.g. a test swapping
// in a fake) cannot race the read; Broadcast is called after releasing the lock
// so a slow broadcast never blocks other app.mu holders (the "snapshot under
// lock, release, then act" rule from AGENTS.md). emitter is never nil — it is
// initialized to dummyEmitter and SetEmitter substitutes dummyEmitter for nil.
func (app *Application) emit(e Event) {
	app.mu.Lock()
	em := app.emitter
	app.mu.Unlock()
	em.Broadcast(e)
}

// SetEmitter injects a broadcaster for real-time events.
func (app *Application) SetEmitter(e EventEmitter) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if e == nil {
		app.emitter = dummyEmitter{}
		return
	}
	app.emitter = e
}

func (app *Application) runMetricsPush(ctx context.Context) {
	ticker := time.NewTicker(1000 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			remaining := app.queue.TotalRemainingBytes()
			app.mu.Lock()
			stats := app.downloaderStats
			app.mu.Unlock()
			// --- No lock held below this line ---
			var speed float64
			var limit int64
			var servers []downloader.ServerSnapshot
			if stats != nil {
				speed = stats.Speed()
				limit = stats.SpeedLimit()
				servers = stats.ServerStatus()
			}
			app.emit(Event{
				Type:          "metrics",
				Speed:         int64(speed),
				Remaining:     remaining,
				SpeedLimit:    limit,
				BandwidthMax:  app.bandwidthMax.Load(),
				BandwidthPerc: int(app.bandwidthPerc.Load()),
				Servers:       servers,
			})
			if speed > 0 {
				app.emit(Event{Type: "queue_updated"})
			}
		}
	}
}

package app

import (
	"context"
	"time"

	"github.com/hobeone/gonzbd/internal/downloader"
)

// FileComplete is emitted when a file assembly is finished.
//
// It carries no CRC32. The assembler used to compute a whole-file value by
// combining the per-article CRCs it happened to see, which was #349: a resumed
// run is never sent the articles an earlier run completed, so those parts
// never tile the file and the figure described a fragment while claiming to
// describe the whole.
//
// The honest whole-file value is the crc32 of a file's single durable run.
// A run combines the CRCs of the articles that abut as they join it, across
// restarts, so a file whose articles all arrive collapses to one row at offset
// 0 whose crc32 IS the file's — already computed, with no read of the file
// (R24). Application.recordAssembledCRC threads it to Queue.SetFileCRC32 when
// the file finalizes, which is what QuickCheck and on-demand par2 read.
type FileComplete struct {
	JobID   string
	FileIdx int
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

// emit broadcasts e through the currently-registered emitter. Because emitter
// is assigned strictly during New() and never mutated afterward, reading it
// requires no mutex synchronization. emitter is never nil — it is initialized
// to dummyEmitter by default.
func (app *Application) emit(e Event) {
	app.emitter.Broadcast(e)
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

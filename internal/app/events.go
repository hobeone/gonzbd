package app

import (
	"context"
	"time"

	"github.com/hobeone/gonzbd/internal/downloader"
)

// FileComplete is emitted when a file assembly is finished: the assembler
// finished the file on its writing handle, or verification finished it by path.
//
// It carries no CRC32. The whole-file CRC is derived from the file's written
// rows when the completion is consumed (Job.SettleFileCRC).
type FileComplete struct {
	JobID   string
	FileIdx int
	// Resumed marks a completion the verifier produced at hydration rather
	// than one this process assembled. The hydration already settled the
	// file's CRC and marked it complete, so its consumer skips both and needs
	// no manifest. It is not fed to DirectUnpack: an unpacker starts only from
	// volume 1 of a live download, and post-processing's normal unpack is the
	// backstop.
	Resumed bool
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
	interval := app.metricsPushInterval
	if interval <= 0 {
		interval = 1000 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var remaining int64
			if app.dispatcher != nil {
				for _, row := range app.dispatcher.List() {
					remaining += row.RemainingBytes
				}
			}
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

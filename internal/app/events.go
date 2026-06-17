package app

import (
	"context"
	"time"

	"github.com/hobeone/gonzbd/internal/downloader"
)

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
			var speed float64
			var limit int64
			var servers []downloader.ServerSnapshot
			if app.downloaderStats != nil {
				speed = app.downloaderStats.Speed()
				limit = app.downloaderStats.SpeedLimit()
				servers = app.downloaderStats.ServerStatus()
			}
			app.emitter.Broadcast(Event{
				Type:          "metrics",
				Speed:         int64(speed),
				Remaining:     remaining,
				SpeedLimit:    limit,
				BandwidthMax:  app.bandwidthMax.Load(),
				BandwidthPerc: int(app.bandwidthPerc.Load()),
				Servers:       servers,
			})
			if speed > 0 {
				app.emitter.Broadcast(Event{Type: "queue_updated"})
			}
		}
	}
}

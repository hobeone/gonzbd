// Package notifier dispatches download and processing events to pluggable
// notification sinks (email, apprise, script). The Dispatcher fans each
// Event out to every registered Notifier whose EventMask includes the event
// type. Per-notifier failures are logged but never propagated.
package notifier

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// EventType identifies the kind of notification event.
type EventType int

// Event type constants. Each value represents one notification trigger.
// DownloadStarted is the first; Error is the last.
//
//nolint:revive // the block comment above covers all constants in this group
const (
	DownloadStarted EventType = iota + 1
	DownloadComplete
	DownloadFailed
	PostProcessingComplete
	PostProcessingFailed
	DiskFull
	QueueDone
	Warning
	Error
)

var eventTypeNames = map[EventType]string{
	DownloadStarted:        "DownloadStarted",
	DownloadComplete:       "DownloadComplete",
	DownloadFailed:         "DownloadFailed",
	PostProcessingComplete: "PostProcessingComplete",
	PostProcessingFailed:   "PostProcessingFailed",
	DiskFull:               "DiskFull",
	QueueDone:              "QueueDone",
	Warning:                "Warning",
	Error:                  "Error",
}

// String returns the human-readable name of the event type.
func (e EventType) String() string {
	if s, ok := eventTypeNames[e]; ok {
		return s
	}
	return fmt.Sprintf("EventType(%d)", int(e))
}

// Event is the payload delivered to each notifier.
type Event struct {
	Type      EventType
	Title     string
	Body      string
	JobName   string
	Timestamp time.Time
}

// Notifier is one sink for events.
type Notifier interface {
	Name() string
	Accepts(EventType) bool
	Send(ctx context.Context, e Event) error
}

// Dispatcher fans events out to every registered notifier whose mask
// includes the event type.
type Dispatcher struct {
	notifiers []Notifier
	logger    *slog.Logger
}

// NewDispatcher creates a Dispatcher that logs via logger.
func NewDispatcher(logger *slog.Logger) *Dispatcher {
	return &Dispatcher{logger: logger}
}

// Register adds n to the set of notifiers consulted on each Dispatch call.
func (d *Dispatcher) Register(n Notifier) {
	d.notifiers = append(d.notifiers, n)
}

// Dispatch sends e to every registered notifier that accepts e.Type.
// Failures from individual notifiers are logged; Dispatch never returns an error.
func (d *Dispatcher) Dispatch(ctx context.Context, e Event) {
	for _, n := range d.notifiers {
		if !n.Accepts(e.Type) {
			continue
		}
		if err := n.Send(ctx, e); err != nil {
			d.logger.Error("notifier send failed",
				slog.String("notifier", n.Name()),
				slog.String("event", e.Type.String()),
				slog.Any("err", err),
			)
		}
	}
}

// acceptsAny returns true if t is present in mask.
func acceptsAny(mask []EventType, t EventType) bool {
	for _, m := range mask {
		if m == t {
			return true
		}
	}
	return false
}

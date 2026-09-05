package dispatch

import (
	"context"
	"fmt"
)

type occupyContextKey struct{}

// Occupy marks the calling context as actively occupying the job with ID id.
//
// While Occupy is active:
//   - Dispatcher.Stop waits on waitLive (up to its timeout) and skips Park and
//     Evict if active occupiers do not yield in time.
//   - Dispatcher.Remove called with the occupied context bypasses waiting on
//     waitLive (since the caller is the occupier), preventing self-deadlocks.
//   - Other external Remove calls will wait for active occupiers to reach 0.
//
// Returns ErrNotFound if the job is not currently registered.
func (d *Dispatcher) Occupy(ctx context.Context, id string, fn func(ctx context.Context)) error {
	d.mu.Lock()
	if d.byID[id] == nil {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: Occupy %s: %w", id, ErrNotFound)
	}
	d.occupiers[id]++
	d.mu.Unlock()

	occupyCtx := context.WithValue(ctx, occupyContextKey{}, id)
	defer func() {
		d.mu.Lock()
		d.occupiers[id]--
		if d.occupiers[id] <= 0 {
			delete(d.occupiers, id)
			if ch, ok := d.occupyDrained[id]; ok {
				close(ch)
				delete(d.occupyDrained, id)
			}
		}
		d.mu.Unlock()
	}()

	fn(occupyCtx)
	return nil
}

// waitLive waits for all active occupiers of job id to drain or for ctx to expire.
// Returns nil immediately if no active occupiers are registered.
func (d *Dispatcher) waitLive(ctx context.Context, id string) error {
	d.mu.Lock()
	if d.occupiers[id] <= 0 {
		d.mu.Unlock()
		return nil
	}
	ch, ok := d.occupyDrained[id]
	if !ok {
		ch = make(chan struct{})
		d.occupyDrained[id] = ch
	}
	d.mu.Unlock()

	select {
	case <-ch:
		return nil
	default:
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// IsOccupied reports whether the given job ID currently has active occupiers.
func (d *Dispatcher) IsOccupied(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.occupiers[id] > 0
}

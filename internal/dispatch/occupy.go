package dispatch

import (
	"context"
	"fmt"
)

type occupyContextKey struct{}

type occupyToken struct {
	id    string
	token any
}

// Occupy leases execution on a resident job for post-processing and job
// finalization teardown.
//
// While Occupy is active:
//   - Dispatcher.Stop waits for active occupiers to yield (up to its per-job
//     timeout) and skips Park and Evict if they do not yield in time.
//   - Dispatcher.Remove called by an active occupier with its lease context
//     bypasses waiting on its own token (via waitLiveExcept), preventing
//     self-deadlocks while still waiting on any concurrent occupiers to yield.
//   - Other external Remove calls will wait for active occupiers to reach 0.
//
// Note that downloader fetch handoffs rely on connection pool cancellation
// signaling (CancelJob) rather than the occupancy lease (which protects
// post-processing and finalization).
//
// Returns ErrNotFound if the job is not currently registered or if Remove is
// actively removing it.
func (d *Dispatcher) Occupy(ctx context.Context, id string, fn func(ctx context.Context)) error {
	d.mu.Lock()
	if d.byID[id] == nil || d.removing[id] > 0 {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: Occupy %s: %w", id, ErrNotFound)
	}
	tok := new(byte)
	tokens, ok := d.occupancyTokens[id]
	if !ok {
		tokens = make(map[any]struct{})
		d.occupancyTokens[id] = tokens
	}
	tokens[tok] = struct{}{}
	d.occupiers[id]++
	d.mu.Unlock()

	occupyCtx := context.WithValue(ctx, occupyContextKey{}, occupyToken{id: id, token: tok})
	defer func() {
		d.mu.Lock()
		if tokens, ok := d.occupancyTokens[id]; ok {
			delete(tokens, tok)
			if len(tokens) == 0 {
				delete(d.occupancyTokens, id)
			}
		}
		d.occupiers[id]--
		if d.occupiers[id] <= 0 {
			delete(d.occupiers, id)
			if ch, ok := d.occupyDrained[id]; ok {
				close(ch)
				delete(d.occupyDrained, id)
			}
			if ch, ok := d.occupyStep[id]; ok {
				close(ch)
				delete(d.occupyStep, id)
			}
		} else {
			if ch, ok := d.occupyStep[id]; ok {
				close(ch)
				d.occupyStep[id] = make(chan struct{})
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

// waitLiveExcept waits for all active occupiers of job id other than callerToken
// to drain or for ctx to expire.
// If callerToken is not active in d.occupancyTokens[id], it falls back to waitLive.
func (d *Dispatcher) waitLiveExcept(ctx context.Context, id string, callerToken any) error {
	for {
		d.mu.Lock()
		tokens, ok := d.occupancyTokens[id]
		if !ok || callerToken == nil {
			d.mu.Unlock()
			return d.waitLive(ctx, id)
		}
		if _, present := tokens[callerToken]; !present {
			d.mu.Unlock()
			return d.waitLive(ctx, id)
		}
		if len(tokens) <= 1 {
			d.mu.Unlock()
			return nil
		}
		ch, ok := d.occupyStep[id]
		if !ok {
			ch = make(chan struct{})
			d.occupyStep[id] = ch
		}
		d.mu.Unlock()

		select {
		case <-ch:
			continue
		case <-ctx.Done():
			d.mu.Lock()
			tokens, ok := d.occupancyTokens[id]
			_, callerPresent := tokens[callerToken]
			drained := !ok || len(tokens) == 0 || (callerPresent && len(tokens) == 1)
			d.mu.Unlock()
			if drained {
				return nil
			}
			return ctx.Err()
		}
	}
}

// IsOccupied reports whether the given job ID currently has active occupiers.
func (d *Dispatcher) IsOccupied(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.occupiers[id] > 0
}

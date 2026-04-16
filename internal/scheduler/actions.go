package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrUnknownAction is returned by Dispatch when no handler has been
// registered for the requested action name.
var ErrUnknownAction = errors.New("unknown action")

// Handler is a function that executes a named scheduler action. The arg
// parameter carries the optional string argument parsed from the schedule
// line (e.g. "1000" for "speedlimit 1000"). Handlers must honour ctx
// cancellation for long-running work.
type Handler func(ctx context.Context, arg string) error

// Registry maps action names to Handler functions. It is safe for concurrent
// use; reads (Dispatch) take a read lock and writes (Register) take a write
// lock, making it efficient for the common read-heavy runtime pattern.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{
		handlers: make(map[string]Handler),
	}
}

// Register associates name with h, replacing any previous registration.
// name is matched case-sensitively; callers are expected to normalise to
// lower-case before registering (matching Python's action_name.lower()).
func (r *Registry) Register(name string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[name] = h
}

// Dispatch looks up name in the registry and invokes the handler with arg.
// It returns ErrUnknownAction (wrapped) when no handler is found, or the
// handler's error when invocation fails.
func (r *Registry) Dispatch(ctx context.Context, name, arg string) error {
	r.mu.RLock()
	h, ok := r.handlers[name]
	r.mu.RUnlock()

	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownAction, name)
	}

	return h(ctx, arg)
}

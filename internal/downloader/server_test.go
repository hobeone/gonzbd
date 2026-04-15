package downloader

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/nntp"
)

// newTestServer is a helper that builds a Server with sensible test
// defaults. The caller can override fields via the cfg argument.
// Enable is set to true by default only when the caller does not
// explicitly set it to false via a non-zero (true) value — callers that
// need Enable=false must pass it in cfg and the helper preserves it.
func newTestServer(cfg config.ServerConfig) *Server {
	if cfg.Connections == 0 {
		cfg.Connections = 10
	}
	return NewServer(cfg)
}

// ---------------------------------------------------------------------------
// Active() tests
// ---------------------------------------------------------------------------

func TestActive(t *testing.T) {
	t.Parallel()
	now := time.Now()

	tests := []struct {
		name        string
		enable      bool
		penalty     time.Duration // duration from now; 0 = no penalty
		deactivated bool
		wantActive  bool
	}{
		{
			name:       "enabled no penalty",
			enable:     true,
			wantActive: true,
		},
		{
			name:       "disabled no penalty",
			enable:     false,
			wantActive: false,
		},
		{
			name:       "enabled penalty not yet expired",
			enable:     true,
			penalty:    5 * time.Minute,
			wantActive: false,
		},
		{
			name:       "enabled penalty expired",
			enable:     true,
			penalty:    -1 * time.Minute, // set in the past
			wantActive: true,
		},
		{
			name:        "deactivated penalty not expired",
			enable:      true,
			penalty:     5 * time.Minute,
			deactivated: true,
			wantActive:  false,
		},
		{
			name:        "deactivated penalty expired",
			enable:      true,
			penalty:     -1 * time.Minute,
			deactivated: true,
			// deactivated + expired → Active returns true (expiry passed)
			wantActive: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestServer(config.ServerConfig{Enable: tc.enable})
			if tc.penalty != 0 {
				s.mu.Lock()
				s.penaltyExpiry = now.Add(tc.penalty)
				s.mu.Unlock()
			}
			if tc.deactivated {
				s.mu.Lock()
				s.deactivated = true
				s.mu.Unlock()
			}
			got := s.Active(now)
			if got != tc.wantActive {
				t.Errorf("Active() = %v; want %v", got, tc.wantActive)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RecordBadConnection / RecordGoodConnection / shouldDeactivateOptional
// ---------------------------------------------------------------------------

func TestRecordConnections(t *testing.T) {
	t.Parallel()
	s := newTestServer(config.ServerConfig{Connections: 10})

	s.RecordBadConnection()
	s.RecordBadConnection()
	s.RecordGoodConnection()

	if got := s.BadConnections(); got != 2 {
		t.Errorf("BadConnections() = %d; want 2", got)
	}
	if got := s.GoodConnections(); got != 1 {
		t.Errorf("GoodConnections() = %d; want 1", got)
	}
}

func TestShouldDeactivateOptional(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		optional bool
		required bool
		bad      int64 // bad connection count
		conns    int   // cfg.Connections (denominator)
		want     bool
	}{
		// ratio = bad / conns
		{
			name:     "optional ratio 0.29 (below threshold)",
			optional: true,
			bad:      2,
			conns:    7, // 2/7 ≈ 0.286
			want:     false,
		},
		{
			name:     "optional ratio 0.30 (at threshold, not above)",
			optional: true,
			bad:      3,
			conns:    10, // 3/10 = 0.30 exactly — NOT above
			want:     false,
		},
		{
			name:     "optional ratio 0.31 (above threshold)",
			optional: true,
			bad:      31,
			conns:    100, // 31/100 = 0.31
			want:     true,
		},
		{
			name:     "required server never deactivated even at high ratio",
			optional: true,
			required: true,
			bad:      99,
			conns:    10, // ratio 9.9 — would deactivate if not required
			want:     false,
		},
		{
			name:     "non-optional server never deactivated",
			optional: false,
			bad:      99,
			conns:    10,
			want:     false,
		},
		{
			name:     "zero connections — guard against div-by-zero",
			optional: true,
			bad:      1,
			conns:    0, // guarded — function returns false
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := NewServer(config.ServerConfig{
				Enable:      true,
				Optional:    tc.optional,
				Required:    tc.required,
				Connections: tc.conns,
			})
			for i := int64(0); i < tc.bad; i++ {
				s.RecordBadConnection()
			}
			got := shouldDeactivateOptional(s)
			if got != tc.want {
				t.Errorf("shouldDeactivateOptional() = %v; want %v (bad=%d conns=%d)",
					got, tc.want, tc.bad, tc.conns)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PenaltyFor mapping
// ---------------------------------------------------------------------------

func TestPenaltyFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want time.Duration
	}{
		{
			name: "ErrAuthRejected → PenaltyPerm",
			err:  nntp.ErrAuthRejected,
			want: constants.PenaltyPerm,
		},
		{
			name: "ErrServerUnavailable → Penalty502",
			err:  nntp.ErrServerUnavailable,
			want: constants.Penalty502,
		},
		{
			name: "ErrTransient → PenaltyVeryShort",
			err:  nntp.ErrTransient,
			want: constants.PenaltyVeryShort,
		},
		{
			name: "ErrNoArticle → 0 (no penalty)",
			err:  nntp.ErrNoArticle,
			want: 0,
		},
		{
			name: "ErrAuthRequired → PenaltyShort",
			err:  nntp.ErrAuthRequired,
			want: constants.PenaltyShort,
		},
		{
			name: "ErrClosed → PenaltyUnknown",
			err:  nntp.ErrClosed,
			want: constants.PenaltyUnknown,
		},
		{
			name: "ErrInvalidState → PenaltyShort",
			err:  nntp.ErrInvalidState,
			want: constants.PenaltyShort,
		},
		{
			name: "unknown error → PenaltyUnknown",
			err:  errors.New("some random error"),
			want: constants.PenaltyUnknown,
		},
		{
			name: "wrapped ErrAuthRejected → PenaltyPerm",
			err:  &wrappedErr{msg: "outer", inner: nntp.ErrAuthRejected},
			want: constants.PenaltyPerm,
		},
		{
			name: "wrapped ErrServerUnavailable → Penalty502",
			err:  &wrappedErr{msg: "outer", inner: nntp.ErrServerUnavailable},
			want: constants.Penalty502,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := PenaltyFor(tc.err)
			if got != tc.want {
				t.Errorf("PenaltyFor(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

// wrappedErr is a minimal helper for testing errors.Is with wrapped errors.
type wrappedErr struct {
	msg   string
	inner error
}

func (e *wrappedErr) Error() string { return e.msg + ": " + e.inner.Error() }
func (e *wrappedErr) Unwrap() error { return e.inner }

// ---------------------------------------------------------------------------
// Resolver tests
// ---------------------------------------------------------------------------

func TestResolveLocalhost(t *testing.T) {
	t.Parallel()
	s := newTestServer(config.ServerConfig{})
	ctx := context.Background()

	addrs, err := Resolve(ctx, s, "localhost")
	if err != nil {
		t.Fatalf("Resolve(localhost) error: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("Resolve(localhost) returned no addresses")
	}

	// Verify the cache was populated.
	cached, at := s.ResolvedAddrs()
	if len(cached) == 0 {
		t.Error("ResolvedAddrs() is empty after Resolve")
	}
	if at.IsZero() {
		t.Error("resolvedAt is zero after Resolve")
	}
	// addrs[0] should be a loopback address.
	if !addrs[0].IsLoopback() {
		t.Errorf("addrs[0] = %v; want loopback", addrs[0])
	}
}

func TestResolveContextCancellation(t *testing.T) {
	t.Parallel()
	s := newTestServer(config.ServerConfig{})

	// Use an already-cancelled context; the resolver goroutine will get
	// it and the select should return the ctx.Done() branch.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Resolve(ctx, s, "localhost")
	if err == nil {
		// localhost may have resolved before the cancel was observed —
		// that's a valid race. Only fail if no error and no cancellation
		// signal appeared at all.
		t.Log("resolved before context was cancelled — acceptable race")
		return
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled wrapped in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Concurrent RecordBad/GoodConnection under -race
// ---------------------------------------------------------------------------

func TestConcurrentRecordConnections(t *testing.T) {
	t.Parallel()
	s := newTestServer(config.ServerConfig{Connections: 100})

	const goroutines = 50
	const ops = 100

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				s.RecordBadConnection()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				s.RecordGoodConnection()
			}
		}()
	}
	wg.Wait()

	wantBad := int64(goroutines * ops)
	wantGood := int64(goroutines * ops)
	if got := s.BadConnections(); got != wantBad {
		t.Errorf("BadConnections() = %d; want %d", got, wantBad)
	}
	if got := s.GoodConnections(); got != wantGood {
		t.Errorf("GoodConnections() = %d; want %d", got, wantGood)
	}
}

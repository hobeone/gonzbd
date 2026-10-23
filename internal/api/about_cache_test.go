package api

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubIPs injects a fetcher that serves canned bodies per URL and counts
// calls, plus a settable clock, so no test touches the network or sleeps.
type stubIPs struct {
	mu    sync.Mutex
	calls map[string]int
	body  map[string]string
	now   time.Time
}

func newStubIPs(v4, v6 string) *stubIPs {
	return &stubIPs{
		calls: map[string]int{},
		body:  map[string]string{publicIPv4URL: v4, publicIPv6URL: v6},
		now:   time.Unix(1_000_000, 0),
	}
}

func (st *stubIPs) fetch(_ context.Context, u string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.calls[u]++
	return st.body[u]
}

func (st *stubIPs) clock() time.Time { st.mu.Lock(); defer st.mu.Unlock(); return st.now }

func (st *stubIPs) advance(d time.Duration) { st.mu.Lock(); st.now = st.now.Add(d); st.mu.Unlock() }

func (st *stubIPs) count(u string) int { st.mu.Lock(); defer st.mu.Unlock(); return st.calls[u] }

func (st *stubIPs) set(u, v string) { st.mu.Lock(); st.body[u] = v; st.mu.Unlock() }

func stubbedServer(st *stubIPs) *Server {
	s := testServer()
	s.publicIPs.fetch = st.fetch
	s.publicIPs.now = st.clock
	return s
}

func aboutIPs(t *testing.T, s *Server) (v4, v6 any) {
	t.Helper()
	rr := apiGet(t, s.Handler(), "/api?mode=about&apikey="+testAPIKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	about, ok := decodeJSON(t, rr)["about"].(map[string]any)
	if !ok {
		t.Fatalf("no about map in response")
	}
	return about["public_ipv4"], about["public_ipv6"]
}

func TestModeAbout_CachesPublicIPs(t *testing.T) {
	t.Parallel()
	st := newStubIPs("192.0.2.1", "2001:db8::1")
	s := stubbedServer(st)

	for range 3 {
		v4, v6 := aboutIPs(t, s)
		if v4 != "192.0.2.1" || v6 != "2001:db8::1" {
			t.Fatalf("got v4=%v v6=%v", v4, v6)
		}
	}
	if n4, n6 := st.count(publicIPv4URL), st.count(publicIPv6URL); n4 != 1 || n6 != 1 {
		t.Errorf("fetches v4=%d v6=%d; want 1 each across 3 calls", n4, n6)
	}

	st.advance(publicIPTTL - time.Second)
	aboutIPs(t, s)
	if n := st.count(publicIPv4URL); n != 1 {
		t.Errorf("fetches just inside the TTL = %d; want 1", n)
	}
	st.advance(2 * time.Second)
	aboutIPs(t, s)
	if n := st.count(publicIPv4URL); n != 2 {
		t.Errorf("fetches after the TTL = %d; want 2", n)
	}
}

func TestModeAbout_RejectsInvalidPublicIPs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, v4, v6   string
		wantV4, wantV6 string
	}{
		{"garbage", "<html>nope</html>", "not an ip", "", ""},
		{"empty", "", "", "", ""},
		{"swapped families", "2001:db8::1", "192.0.2.1", "", ""},
		{"4in6 mapped is not v6", "192.0.2.1", "::ffff:192.0.2.1", "192.0.2.1", ""},
		{"4in6 is not v4", "::ffff:192.0.2.1", "2001:db8::1", "", "2001:db8::1"},
		{"zone", "192.0.2.1", "fe80::1%eth0", "192.0.2.1", ""},
		{"valid", "192.0.2.1", "2001:db8::1", "192.0.2.1", "2001:db8::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := stubbedServer(newStubIPs(tc.v4, tc.v6))
			v4, v6 := aboutIPs(t, s)
			if v4 != tc.wantV4 || v6 != tc.wantV6 {
				t.Errorf("v4=%q v6=%q; want %q %q", v4, v6, tc.wantV4, tc.wantV6)
			}
		})
	}
}

// A failed lookup is remembered only for publicIPNegativeTTL: long enough
// that a dead endpoint does not cost 3s per call, short enough that
// recovery shows up well before publicIPTTL.
func TestModeAbout_NegativeCacheIsShort(t *testing.T) {
	t.Parallel()
	st := newStubIPs("", "")
	s := stubbedServer(st)

	aboutIPs(t, s)
	aboutIPs(t, s)
	if n := st.count(publicIPv4URL); n != 1 {
		t.Fatalf("fetches within the negative TTL = %d; want 1", n)
	}

	st.set(publicIPv4URL, "192.0.2.1")
	st.advance(publicIPNegativeTTL + time.Second)
	if v4, _ := aboutIPs(t, s); v4 != "192.0.2.1" {
		t.Errorf("v4 after negative TTL = %v; want recovery to 192.0.2.1", v4)
	}
	if publicIPNegativeTTL >= publicIPTTL {
		t.Errorf("negative TTL %v must be shorter than positive TTL %v", publicIPNegativeTTL, publicIPTTL)
	}
}

func TestModeAbout_ConcurrentCallsShareOneFetch(t *testing.T) {
	t.Parallel()
	st := newStubIPs("192.0.2.1", "2001:db8::1")
	release := make(chan struct{})
	s := testServer()
	s.publicIPs.now = st.clock
	s.publicIPs.fetch = func(ctx context.Context, u string) string {
		<-release
		return st.fetch(ctx, u)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { s.publicIPs.v4(context.Background()) })
	}
	// Callers blocked inside singleflight are not observable, so this waits
	// by time. A sleep that is too short can only make the test pass
	// vacuously, never fail falsely.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := st.count(publicIPv4URL); n != 1 {
		t.Errorf("fetches for 8 concurrent callers = %d; want 1", n)
	}
}

// One caller's cancelled request must not turn the shared result into "".
func TestModeAbout_CancelledCallerDoesNotPoisonCache(t *testing.T) {
	t.Parallel()
	st := newStubIPs("192.0.2.1", "2001:db8::1")
	var sawCancel atomic.Bool
	s := testServer()
	s.publicIPs.now = st.clock
	s.publicIPs.fetch = func(ctx context.Context, u string) string {
		if ctx.Err() != nil {
			sawCancel.Store(true)
			return ""
		}
		return st.fetch(ctx, u)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The cancelled caller gives up with "", but the shared fetch it started
	// is detached from it, so the next caller gets the real address.
	s.publicIPs.v4(ctx)
	if got := s.publicIPs.v4(context.Background()); got != "192.0.2.1" || sawCancel.Load() {
		t.Errorf("v4 = %q (fetch saw cancel: %v); want 192.0.2.1 from a detached fetch", got, sawCancel.Load())
	}
}

// The modes that return config_path, the configured directories and the
// resolved tool paths refuse the upload-only NZB key.
func TestHostPathModes_RequireFullKey(t *testing.T) {
	t.Parallel()
	s := stubbedServer(newStubIPs("192.0.2.1", "2001:db8::1"))

	for _, mode := range []string{"about", "status_overview"} {
		rr := apiGet(t, s.Handler(), "/api?mode="+mode+"&apikey="+testNZBKey)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s with the NZB key: status = %d; want 403", mode, rr.Code)
		}
		rr = apiGet(t, s.Handler(), "/api?mode="+mode+"&apikey="+testAPIKey)
		if rr.Code != http.StatusOK {
			t.Errorf("%s with the full key: status = %d; want 200", mode, rr.Code)
		}
	}
}

// get passes its URL to the fetcher untouched and keeps slots independent:
// a value cached in one slot is not served for another.
func TestPublicIPCache_GetSlotsAreIndependent(t *testing.T) {
	t.Parallel()
	st := newStubIPs("", "")
	st.set("http://example.test/a", "192.0.2.7")
	st.set("http://example.test/b", "2001:db8::7")
	c := &publicIPCache{fetch: st.fetch, now: st.clock}
	var a, b publicIPSlot

	if got := c.get(context.Background(), &a, "http://example.test/a", false); got != "192.0.2.7" {
		t.Errorf("slot a = %q; want 192.0.2.7", got)
	}
	if got := c.get(context.Background(), &b, "http://example.test/b", true); got != "2001:db8::7" {
		t.Errorf("slot b = %q; want 2001:db8::7", got)
	}
	if got := c.get(context.Background(), &a, "http://example.test/b", false); got != "192.0.2.7" {
		t.Errorf("slot a re-read = %q; want its cached 192.0.2.7", got)
	}
	if n := st.count("http://example.test/b"); n != 1 {
		t.Errorf("fetches of b = %d; want 1", n)
	}
}

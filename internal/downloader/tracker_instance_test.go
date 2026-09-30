package downloader

import (
	"context"
	"log/slog"
	"runtime"
	"testing"
	"weak"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestTracker_TwoInstancesUnderOneIDAreTrackedSeparately: a retry registers a
// new instance under the ID of the one it replaces, and the two have separate
// entries. ClearJob, which takes an ID, clears both.
func TestTracker_TwoInstancesUnderOneIDAreTrackedSeparately(t *testing.T) {
	tr := newDispatchTracker()
	removed, retry := bareJob("j1"), bareJob("j1")
	kRemoved, kRetry := keyFor(removed, 0), keyFor(retry, 0)
	var mask serverMask
	mask.set(0)

	tr.Lock()
	tr.IncrementInFlightLocked(kRetry)
	tr.SetTriedLocked(kRetry, mask)
	tr.IncrementInFlightLocked(kRemoved)
	tr.SetTriedLocked(kRemoved, mask)
	tr.Unlock()

	tr.DecrementInFlight(kRemoved)
	tr.UnmarkTried(kRemoved, 0)

	tr.Lock()
	n := tr.InFlightLocked(kRetry)
	got, ok := tr.TryListLocked(kRetry)
	tr.Unlock()
	if n != 1 || !ok || !got.has(0) {
		t.Errorf("the retry's entries changed with the removed instance's: in-flight=%d, try-list present=%v has server 0=%v; want 1, true, true", n, ok, got.has(0))
	}

	tr.ClearJob("j1")
	if tryLen, inLen := tr.Len(); tryLen != 0 || inLen != 0 {
		t.Errorf("after ClearJob(j1): try-list=%d in-flight=%d entries, want 0 and 0", tryLen, inLen)
	}
}

// trackUnreachable records a try-list entry and an in-flight count for a job
// instance nothing else refers to once it returns, and returns a weak pointer
// to that instance.
//
//go:noinline
func trackUnreachable(tr *dispatchTracker) weak.Pointer[job.Job] {
	j := bareJob("j-gone")
	var mask serverMask
	mask.set(0)
	tr.Lock()
	tr.SetTriedLocked(keyFor(j, 0), mask)
	tr.IncrementInFlightLocked(keyFor(j, 0))
	tr.Unlock()
	return weak.Make(j)
}

// TestTracker_AnEntryDoesNotKeepItsInstanceReachable: an entry an instance
// leaves behind does not hold its job, and with it the manifest, in memory.
func TestTracker_AnEntryDoesNotKeepItsInstanceReachable(t *testing.T) {
	tr := newDispatchTracker()
	wp := trackUnreachable(tr)
	runtime.GC()
	runtime.GC()
	if wp.Value() != nil {
		t.Error("a job instance referred to only by tracker entries is still reachable after GC")
	}
	if tryLen, inLen := tr.Len(); tryLen != 1 || inLen != 1 {
		t.Fatalf("fixture: try-list=%d in-flight=%d entries, want 1 and 1", tryLen, inLen)
	}
}

// trackerState reads the tracker's in-flight count and try-list for j's
// article artIdx.
func trackerState(d *Downloader, j *job.Job, artIdx int32) (inFlight int, mask serverMask, tried bool) {
	d.tracker.Lock()
	defer d.tracker.Unlock()
	key := keyFor(j, artIdx)
	mask, tried = d.tracker.TryListLocked(key)
	return d.tracker.InFlightLocked(key), mask, tried
}

// retryInFlight builds the state #665 is about: a request for a removed
// instance still outstanding, and a retry under the same ID whose own fetch of
// the same article is in flight on server 0. It returns the downloader, the
// removed instance's request and the retry.
func retryInFlight(t *testing.T) (*Downloader, *dispatch.Dispatcher, *articleRequest, *job.Job) {
	t.Helper()
	disp, req, second := staleRequest(t)
	// Two servers, so a second dispatch has somewhere to go once the retry's
	// first is no longer counted.
	backup := NewServer(config.ServerConfig{Name: "s2", Enable: true, Host: "127.0.0.1", Port: 1, Connections: 1})
	d := New(disp, []*Server{unreachableServer(), backup}, nil, Options{}, slog.New(slog.DiscardHandler))
	d.pauseCtx = t.Context()

	a := UnfinishedArticle{Job: second, ArtIdx: req.artIdx, MessageID: "a@h", Bytes: 100, PartNumber: 1}
	if handled, exReq := d.tryDispatch(t.Context(), a, defaultOpts(d.servers)); !handled || exReq != nil {
		t.Fatalf("fixture: the retry's article was not dispatched (handled=%v exReq=%v)", handled, exReq)
	}
	<-d.workCh["s1"] // the retry's fetch is now "on a connection"; free the slot for a second dispatch
	if n, mask, _ := trackerState(d, second, req.artIdx); n != 1 || !mask.has(0) {
		t.Fatalf("fixture: retry in-flight=%d mask has server 0=%v, want 1 and true", n, mask.has(0))
	}
	return d, disp, req, second
}

// TestTracker_ARemovedInstancesLateCompletionLeavesTheRetrysEntries pins
// #665: a fetch dispatched for an instance that has since been removed, and
// completes after a retry registered under the same ID dispatched the same
// article, must leave the retry's in-flight count and try-list alone.
//
// Each case drives one production path by which a request's completion
// updates the tracker.
func TestTracker_ARemovedInstancesLateCompletionLeavesTheRetrysEntries(t *testing.T) {
	cases := []struct {
		name string
		// complete runs the removed instance's completion.
		complete func(t *testing.T, d *Downloader, req *articleRequest)
		// keepsInFlight reports whether the path decrements an in-flight
		// count, and so whether the retry's count is observable through it.
		keepsInFlight bool
	}{
		{"a request dropped before the fetch", func(t *testing.T, d *Downloader, req *articleRequest) {
			d.handleRequest(t.Context(), d.servers[0], 0, &managedConn{}, req, "s1#0", nil)
		}, true},
		{"a successful fetch", func(t *testing.T, d *Downloader, req *articleRequest) {
			d.processFetchedArticle(t.Context(), NewServer(config.ServerConfig{Name: "s1"}), req,
				yencBody("test.bin", []byte("payload")))
		}, false},
		{"a terminal decode error", func(t *testing.T, d *Downloader, req *articleRequest) {
			d.processFetchedArticle(t.Context(), NewServer(config.ServerConfig{Name: "s1"}), req,
				[]byte("neither yEnc nor UU\r\n"))
		}, false},
		{"an exhausted try-list", func(t *testing.T, d *Downloader, req *articleRequest) {
			d.applyDispatchPlan(t.Context(), dispatchPlan{exhausted: []*articleRequest{req}}, dispatchOpts{})
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, req, second := retryInFlight(t)

			tc.complete(t, d, req)

			n, mask, tried := trackerState(d, second, req.artIdx)
			if !tried || !mask.has(0) {
				t.Errorf("the retry's try-list lost server 0 to a completion for the removed instance (entry present=%v)", tried)
			}
			if !tc.keepsInFlight {
				return
			}
			if n != 1 {
				t.Errorf("the retry's in-flight count is %d after a completion for the removed instance, want 1", n)
			}
			// What the count is for: a dispatch pass must not send the retry's
			// article to a second connection while its own fetch is running.
			// Two fetches of it both carry the retry's instance, so the
			// pipeline would accept both results.
			a := UnfinishedArticle{Job: second, ArtIdx: req.artIdx, MessageID: "a@h", Bytes: 100, PartNumber: 1}
			if handled, _ := d.tryDispatch(context.Background(), a, defaultOpts(d.servers)); handled {
				t.Error("the retry's article was dispatched a second time while its own fetch is in flight")
			}
		})
	}
}

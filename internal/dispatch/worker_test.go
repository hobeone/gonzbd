package dispatch

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// TestCancelledWorker_SettlesRatherThanReAbortingForever is the most
// important test in this plan. Without the dispatcher's exit path, a
// cancelled worker's Abort never returns any resource: HoldsLease stays
// true, running() stays true, and every subsequent tick routes IntentCancel
// back to finishCancel, which calls Abort again and returns nil — the job
// never settles and holds pool-A capacity for the life of the process.
func TestCancelledWorker_SettlesRatherThanReAbortingForever(t *testing.T) {
	w := &stubWorkers{}
	d := newTestDispatcher(t, withWorkers(w))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !d.q.Render(j).Running {
		t.Fatal("setup: job is not running")
	}

	if err := d.Cancel(j.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(w.aborted) != 1 {
		t.Fatalf("Abort called %d times, want 1", len(w.aborted))
	}

	// The worker notices the abort and exits without finishing.
	if err := d.Yielded(j.ID()); err != nil {
		t.Fatalf("Yielded: %v", err)
	}
	d.tick(context.Background())

	if got := d.q.Render(j).Outcome; got != job.OutcomeCancelled {
		t.Errorf("Outcome = %v, want Cancelled — without an exit path the job holds its lease, running() stays true, and finishCancel re-Aborts every tick forever", got)
	}
	if len(w.aborted) != 1 {
		t.Errorf("Abort called %d times in total, want 1 — a second call means the job never settled and the loop is live", len(w.aborted))
	}
}

func TestYielded_UnderPauseReturnsTheLease(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !j.HoldsLease() {
		t.Fatal("setup: job holds no lease")
	}

	d.Pause()
	if err := d.Yielded(j.ID()); err != nil {
		t.Fatalf("Yielded: %v", err)
	}

	if j.HoldsLease() {
		t.Error("lease still held after a pause yield — Advance branch 2 returns early while holds() is true, so only the dispatcher's exit path can return it")
	}
}

// TestFinished_SucceedsForARunningJob pins Finished's success path: a job
// with an open attempt settles and its resources are released.
func TestFinished_SucceedsForARunningJob(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !d.q.Render(j).Running {
		t.Fatal("setup: job is not running")
	}

	if err := d.Finished(j.ID(), job.OutcomeFailed); err != nil {
		t.Fatalf("Finished: %v", err)
	}
	if got := d.q.Render(j).Outcome; got != job.OutcomeFailed {
		t.Errorf("Outcome = %v, want OutcomeFailed", got)
	}
}

// TestFinished_PropagatesASettleError pins Finished's error-wrapping branch:
// a job with no open attempt cannot be settled, and Settle's refusal must
// come back through Finished rather than being swallowed.
func TestFinished_PropagatesASettleError(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// No tick: the job never opened an attempt.

	if err := d.Finished(j.ID(), job.OutcomeOK); err == nil {
		t.Fatal("Finished on a job with no open attempt returned nil, want an error")
	}
}

func TestFinished_RefusesCancelledAsAnOutcome(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())

	if err := d.Finished(j.ID(), job.OutcomeCancelled); err == nil {
		t.Fatal("Finished accepted OutcomeCancelled, want an error — only the cancel latch may produce it, and a worker reporting it would let any exit masquerade as a user deletion")
	}
}

// TestClaimLaunched_ClaimsOnce pins claimLaunched's contract directly: the
// first call to claim an ID succeeds, a second call before clearLaunched
// fails, and clearLaunched makes the ID claimable again.
func TestClaimLaunched_ClaimsOnce(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !d.claimLaunched("j1") {
		t.Fatal("first claim should succeed")
	}
	if d.claimLaunched("j1") {
		t.Fatal("second claim before clear should fail")
	}
	d.clearLaunched("j1")
	if !d.claimLaunched("j1") {
		t.Fatal("claim after clearLaunched should succeed")
	}
}

// TestLaunch_DirectCallStartsWhenRunningAndClaimable calls launch directly
// (rather than through tick) to pin its two guards independently: it starts
// the runner only for a job that is Running and not already claimed.
func TestLaunch_DirectCallStartsWhenRunningAndClaimable(t *testing.T) {
	runner := &fakeRunner{}
	d := newTestDispatcher(t, withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	d.tick(context.Background())
	d.tick(context.Background())
	if !d.q.Render(j).Running {
		t.Fatal("setup: job is not running")
	}

	// The tick's own launch already claimed the job; clear it so this direct
	// call is the one that observes claimLaunched return true.
	d.clearLaunched(j.ID())
	runner.mu.Lock()
	runner.seen = map[string]bool{}
	runner.mu.Unlock()

	d.launch(j)

	if !runner.started(j.ID()) {
		t.Error("launch did not start the runner for a running, claimable job")
	}
}

// TestLaunch_DirectCallSkipsWhenNotRunning pins launch's Running guard: a job
// that has never ticked holds nothing and is not Running, so a direct launch
// call must not start it.
func TestLaunch_DirectCallSkipsWhenNotRunning(t *testing.T) {
	runner := &fakeRunner{}
	d := newTestDispatcher(t, withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d.launch(j)

	if runner.started(j.ID()) {
		t.Error("launch started the runner for a job that is not Running")
	}
}

func TestLaunch_SkippedWhenIntentTurnedToCancelDuringHydration(t *testing.T) {
	runner := &fakeRunner{}
	res := &fakeResidency{}
	d := newTestDispatcher(t, withResidency(res), withRunner(runner))
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Cancel lands while the manifest read is in flight.
	res.onHydrate = func(string) { _ = d.Cancel(j.ID()) }

	d.tick(context.Background())
	d.tick(context.Background())

	if runner.started(j.ID()) {
		t.Error("worker launched for a job cancelled during hydration — the launch path must re-read the snapshot after the unlocked read")
	}
}

// TestWorkerExits_ClearTheLaunchClaimBeforeKicking pins the call ORDER inside
// Finished and Yielded: clearLaunched must precede kick.
//
// Why this is a source-order check and not a behavioural one. The consequence
// is real but the window is nanoseconds — between kick() returning and a
// deferred clear running — so a runtime test would be flaky in the direction
// that matters (it would usually pass while the bug was present) and would
// pin nothing. The ordering is invisible at runtime and plain in the source,
// which is the same argument
// TestRenderAll_LocksOnceOutsideTheRowLoop (internal/sched) is built on.
//
// What goes wrong when the order flips: kick wakes the ticker, tick reaches
// launch(), claimLaunched finds the claim still held and declines to start a
// worker — consuming the wake in the process. The job then holds its
// resources with nobody working it until the next timer tick.
func TestWorkerExits_ClearTheLaunchClaimBeforeKicking(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "worker.go", nil, 0)
	if err != nil {
		t.Fatalf("parse worker.go: %v", err)
	}

	want := map[string]bool{"Finished": false, "Yielded": false}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		if _, tracked := want[fd.Name.Name]; !tracked {
			continue
		}
		want[fd.Name.Name] = true

		var clearPos, kickPos []token.Pos
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "clearLaunched":
				clearPos = append(clearPos, call.Pos())
			case "kick":
				kickPos = append(kickPos, call.Pos())
			}
			return true
		})

		if len(kickPos) == 0 {
			t.Errorf("%s calls kick 0 times — this test no longer covers what it names", fd.Name.Name)
			continue
		}
		if len(clearPos) == 0 {
			t.Errorf("%s never clears the launch claim", fd.Name.Name)
			continue
		}
		// Every kick must be preceded by a clear.
		for _, k := range kickPos {
			cleared := false
			for _, c := range clearPos {
				if c < k {
					cleared = true
				}
			}
			if !cleared {
				t.Errorf("%s kicks before clearing the launch claim — the woken "+
					"tick can reach launch() while the claim is still held, decline "+
					"to start a worker, and consume the wake doing it", fd.Name.Name)
			}
		}
		// A deferred clear runs after kick however it is written.
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			ds, ok := n.(*ast.DeferStmt)
			if !ok {
				return true
			}
			if sel, ok := ds.Call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "clearLaunched" {
				t.Errorf("%s clears the launch claim with defer — that runs on the "+
					"way out, which is after kick, whatever the statement order says",
					fd.Name.Name)
			}
			return true
		})
	}
	for name, found := range want {
		if !found {
			t.Errorf("%s not found in worker.go; this test cannot silently pass because its subject moved", name)
		}
	}
}

// TestWorkerExits_RejectAnUnknownID covers the branch the id-taking signature
// introduces. A Runner reporting for a job that has since been removed —
// cancelled and evicted, or torn down — must get an error rather than a nil
// dereference.
func TestWorkerExits_RejectAnUnknownID(t *testing.T) {
	d := newTestDispatcher(t)
	if err := d.Finished("nope", job.OutcomeOK); err == nil {
		t.Error("Finished on an unregistered id = nil, want an error")
	}
	if err := d.Yielded("nope"); err == nil {
		t.Error("Yielded on an unregistered id = nil, want an error")
	}
}

func TestClaimLaunched_GuardsAgainstRemovingAndEvictedJobs(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Normal case: claimLaunched succeeds.
	if !d.claimLaunched("j1") {
		t.Fatal("claimLaunched failed for healthy registered job")
	}
	// Duplicate claim fails.
	if d.claimLaunched("j1") {
		t.Fatal("duplicate claimLaunched succeeded")
	}
	d.clearLaunched("j1")

	// While job is being removed, claimLaunched must refuse and leak no channel.
	// Through the real protocol, not by poking d.removing: a test that sets
	// the marker itself passes whether or not any production path sets it the
	// same way, which is how #513's missing marker survived.
	rm, ok := d.beginRemoval("j1")
	if !ok {
		t.Fatal("beginRemoval: job not registered")
	}

	if d.claimLaunched("j1") {
		t.Fatal("claimLaunched succeeded while job was marked removing")
	}
	d.mu.Lock()
	if _, ok := d.launched["j1"]; ok {
		d.mu.Unlock()
		t.Fatal("claimLaunched leaked a channel in d.launched while removing")
	}
	d.mu.Unlock()
	rm.abort()

	// Once job is removed from byID, claimLaunched must refuse and leak no channel.
	rm, ok = d.beginRemoval("j1")
	if !ok {
		t.Fatal("beginRemoval (second): job not registered")
	}
	rm.end()

	if d.claimLaunched("j1") {
		t.Fatal("claimLaunched succeeded for job not in byID")
	}
	d.mu.Lock()
	if _, ok := d.launched["j1"]; ok {
		d.mu.Unlock()
		t.Fatal("claimLaunched leaked a channel in d.launched for evicted job")
	}
	d.mu.Unlock()
}

func TestRemove_RefcountsConcurrentRemovals(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Two concurrent removals, driven through the protocol itself. This test
	// used to assign d.removing["j1"] = 2 and hand-roll the decrement, which
	// asserted that the guards respect a count rather than that any removal
	// path maintains one — the gap #513 lived in.
	first, ok := d.beginRemoval("j1")
	if !ok {
		t.Fatal("beginRemoval (first): job not registered")
	}
	second, ok := d.beginRemoval("j1")
	if !ok {
		t.Fatal("beginRemoval (second): job not registered")
	}

	// First removal hits an error and releases its marker.
	first.abort()

	// Second removal is still in-flight: removing count is 1, so guards must remain active.
	d.mu.Lock()
	count := d.removing["j1"]
	d.mu.Unlock()
	if count != 1 {
		t.Fatalf("d.removing[j1] = %d, want 1 — one removal aborted, one still outstanding", count)
	}

	if d.claimLaunched("j1") {
		t.Fatal("claimLaunched succeeded while second removal was still in-flight")
	}

	d.markResident("j1")
	if d.isResident("j1") {
		t.Fatal("markResident marked job resident while second removal was in-flight")
	}

	// An aborted removal must be inert: a second abort or end from it would
	// decrement the count the surviving removal is holding.
	first.abort()
	first.end()
	d.mu.Lock()
	count = d.removing["j1"]
	d.mu.Unlock()
	if count != 1 {
		t.Fatalf("d.removing[j1] = %d after re-using a finished removal, want 1", count)
	}

	// Second removal completes and cleans up.
	second.end()

	d.mu.Lock()
	_, stillRemoving := d.removing["j1"]
	d.mu.Unlock()
	if stillRemoving {
		t.Fatal("d.removing[j1] still present after remove")
	}
}

// TestWaitLaunched_PrefersClosedChannelOverCancelledContext verifies that when
// both the worker channel is closed and ctx is expired, waitLaunched prioritizes
// the closed channel via its pre-select rather than returning ctx.Err() at random.
func TestWaitLaunched_PrefersClosedChannelOverCancelledContext(t *testing.T) {
	d := newTestDispatcher(t)
	j := job.New("j1", "n", job.Policy{})
	if err := d.Add(j, Header{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for i := range 200 {
		if !d.claimLaunched("j1") {
			t.Fatalf("iteration %d: claimLaunched failed", i)
		}
		// Close the channel (worker exited).
		d.clearLaunched("j1")

		// Re-populate launched map with a closed channel to test waitLaunched directly
		// while id is in launched. Note clearLaunched deletes it from launched, so
		// re-add a closed channel to test waitLaunched when ch != nil.
		closedCh := make(chan struct{})
		close(closedCh)
		d.mu.Lock()
		d.launched["j1"] = closedCh
		d.mu.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // context is cancelled immediately

		if err := d.waitLaunched(ctx, "j1"); err != nil {
			t.Fatalf("iteration %d: waitLaunched returned %v, want nil (closed channel must take priority over cancelled context)", i, err)
		}

		d.mu.Lock()
		delete(d.launched, "j1")
		d.mu.Unlock()
	}
}

func TestYieldedFor_JobMismatch_NoOpsAndPreservesNewAttempt(t *testing.T) {
	d := newTestDispatcher(t)
	j1 := job.New("j1", "first", job.Policy{})
	if err := d.Add(j1, Header{}); err != nil {
		t.Fatalf("Add(j1): %v", err)
	}

	// j1 runs and claims launch latch
	if !d.claimLaunched("j1") {
		t.Fatal("claimLaunched(j1) = false, want true")
	}

	// Simulate j1 being finalized and deregistered
	rm, ok := d.beginRemoval("j1")
	if !ok {
		t.Fatal("beginRemoval: job not registered")
	}
	rm.end()

	// New attempt under the same ID is registered
	j2 := job.New("j1", "second", job.Policy{})
	if err := d.Add(j2, Header{}); err != nil {
		t.Fatalf("Add(j2): %v", err)
	}

	// j2 claims launched
	if !d.claimLaunched("j1") {
		t.Fatal("claimLaunched(j2) = false, want true")
	}

	// Delayed yield from j1's abort arrives targeting j1
	err := d.YieldedFor("j1", j1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("YieldedFor(j1) with mismatched job = %v, want ErrNotFound", err)
	}

	// Assert j2's launched latch was NOT cleared
	d.mu.Lock()
	launchedCh := d.launched["j1"]
	d.mu.Unlock()
	if launchedCh == nil {
		t.Error("YieldedFor with stale job object cleared the new attempt's launched latch")
	}
}

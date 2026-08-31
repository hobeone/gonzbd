package app

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestStall_TheStoppingGuardCoversTheCleanShutdownBarrier pins the guard on the
// path it was written for, which is the path it did not cover.
//
// Application.Stall refuses to park a job while the process is stopping,
// because that pause cannot be undone: Shutdown's final queue.Save persists it,
// the stall list that would re-evaluate it is in-memory and dies with the
// process, and the startup sweep skips the job because its phase is no longer
// active. The job comes back Paused forever.
//
// The guard tested app.ctx.Err(), and Shutdown runs the clean-shutdown
// checkpoint at step 2 while app.cancel() runs at step 4. So during the very
// barrier that raises these faults, ctx.Err() was still nil and the guard was
// inert. Only a SIGTERM-cancelled parent context took the guarded path.
//
// The fixture reproduces the ordering rather than calling Shutdown, because
// Shutdown tears down collaborators this assertion needs alive.
func TestStall_TheStoppingGuardCoversTheCleanShutdownBarrier(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	// Exactly the state Shutdown is in when its step-2 barrier runs: stopping,
	// but the context not yet cancelled.
	application.stopping.Store(true)
	if application.ctx != nil && application.ctx.Err() != nil {
		t.Fatal("the fixture's context is already cancelled, so it cannot observe the " +
			"window between step 2 and step 4 — which is the whole of this test")
	}

	application.Stall(job.ID, storagefault.Classify("sync", "/d/a.bin", syscall.ETIMEDOUT))

	snap, err := application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status == constants.StatusPaused {
		t.Errorf("the job was parked by the clean-shutdown barrier: warning=%q. "+
			"Shutdown's final queue.Save persists that pause, the stall list dies with "+
			"the process, and the startup sweep skips a non-active job — so it comes "+
			"back Paused forever after a slow but perfectly normal stop", snap.Warning)
	}
}

// TestStall_StillParksWhenTheProcessIsNotStopping is the half that keeps the
// guard above honest. Without it, "never park" satisfies the test.
func TestStall_StillParksWhenTheProcessIsNotStopping(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)

	if application.stopping.Load() {
		t.Fatal("the fixture is already stopping, so it cannot observe the running case")
	}

	application.Stall(job.ID, storagefault.Classify("sync", "/d/a.bin", syscall.ETIMEDOUT))

	snap, err := application.queue.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != constants.StatusPaused {
		t.Fatal("a wedged mount left the job running; it sits at N% with no reason the " +
			"operator can act on (A2)")
	}
}

// TestPerJobShare pins the arithmetic that keeps the shutdown checkpoint's
// per-job isolation real.
//
// shutdownCheckpointTimeout was passed as BOTH the sweep context and the
// per-job budget. context.WithTimeout cannot exceed its parent, so a first job
// consuming most of the sweep left every job behind it with an already-expired
// context and an immediate failure — and per shutdownCheckpoint's own doc,
// everything those jobs downloaded since their last barrier is then re-fetched
// on the next start.
func TestPerJobShare(t *testing.T) {
	t.Parallel()
	const total = 10 * time.Second
	for _, tc := range []struct {
		jobs int
		want time.Duration
	}{
		// No jobs and one job both get the whole budget: there is nobody to
		// divide it with, and halving it would shorten the common case for
		// nothing.
		{0, total},
		{1, total},
		{2, 5 * time.Second},
		{4, 2500 * time.Millisecond},
	} {
		if got := perJobShare(total, tc.jobs); got != tc.want {
			t.Errorf("perJobShare(%v, %d) = %v, want %v", total, tc.jobs, got, tc.want)
		}
	}
}

// TestShutdown_MarksTheProcessStoppingBeforeItsBarrier pins the half the unit
// tests above cannot: they set the flag themselves, so they say nothing about
// whether Shutdown sets it, or sets it in time.
//
// In time means BEFORE stopWorkers, which is where the clean-shutdown barrier
// runs. app.cancel() is two steps later, which is exactly why the context test
// the guard used to rely on was inert during that barrier.
//
// The observation is taken from inside stopWorkers rather than after Shutdown
// returns, because "true at the end" holds under a store placed anywhere in
// the function — including after the barrier it is supposed to cover.
func TestShutdown_MarksTheProcessStoppingBeforeItsBarrier(t *testing.T) {
	t.Parallel()
	application, _ := newDurabilityTestApp(t, 1, 2)
	application.started.Store(true)
	// The fixture builds an application without running Start, so its
	// lifecycle context was never created and Shutdown's app.cancel() would
	// dereference nil. Supplied here rather than by starting the whole
	// application, which would pull in the downloader this test has no use for.
	ctx, cancel := context.WithCancel(context.Background())
	application.ctx, application.cancel = ctx, cancel
	t.Cleanup(cancel)

	var seenDuringBarrier bool
	application.checkpointHook = func() { seenDuringBarrier = application.stopping.Load() }

	if application.stopping.Load() {
		t.Fatal("the fixture is already stopping, so it cannot observe the transition")
	}
	if err := application.Shutdown(); err != nil {
		t.Logf("Shutdown reported %v; the flag is what is under test", err)
	}

	if !seenDuringBarrier {
		t.Error("the process was not marked stopping by the time the clean-shutdown " +
			"barrier ran. Every fault that barrier raises then parks its job, the pause " +
			"is persisted by Shutdown's own final queue.Save, and the job comes back " +
			"Paused forever")
	}
	if !application.stopping.Load() {
		t.Error("Shutdown never marked the process stopping")
	}
}

// TestCheckpointAllWithBudget_ImposesThePolicysDeadline drives the budget
// through the sweep rather than through the arithmetic alone, which is where it
// has to be applied to matter.
//
// The observable is the deadline each job's barrier is actually given. A sweep
// that imposed none, or that handed the job the caller's context unchanged,
// leaves a worker parked in an fsync blocking the single loop that also owns
// stall re-evaluation and the queue save.
func TestCheckpointAllWithBudget_ImposesThePolicysDeadline(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)
	writeFixtureArticle(t, application, job.ID, 0, 0)

	var got []time.Duration
	var askedFor int
	application.jobCheckpointHook = func(ctx context.Context) {
		if dl, ok := ctx.Deadline(); ok {
			got = append(got, time.Until(dl))
		}
	}

	application.checkpointAllWithBudget(t.Context(), func(jobs int) time.Duration {
		askedFor = jobs
		return perJobShare(4*time.Second, jobs)
	})

	if askedFor != 1 {
		t.Errorf("the budget policy was asked about %d jobs, want 1 — it is asked AFTER "+
			"the list is known precisely so a policy can divide by it", askedFor)
	}
	if len(got) == 0 {
		t.Fatal("no job was checkpointed, so this cannot observe the budget it was given")
	}
	if got[0] > 4*time.Second+time.Second {
		t.Errorf("per-job deadline %v exceeds the sweep budget; a worker parked in an "+
			"fsync would hold the checkpoint loop past the window it has", got[0])
	}
	if got[0] < time.Second {
		t.Errorf("per-job deadline %v — the job inherited an exhausted budget rather "+
			"than being given a share of it, so it fails immediately and everything it "+
			"downloaded since its last barrier is re-fetched", got[0])
	}

	// The shutdown path's own composition. With one job in the fixture the
	// share IS the whole budget, so this asserts the wiring rather than the
	// division — the division itself is pinned by TestPerJobShare, and a
	// fixture with two jobs holding open files would be needed to observe both
	// at once.
	got = nil
	application.checkpointAllShare(t.Context(), 4*time.Second)
	if len(got) != 1 || got[0] > 4*time.Second+time.Second {
		t.Errorf("checkpointAllShare gave deadlines %v, want one within its budget", got)
	}
}

// TestForgetJobBarrierState_DoesNotDropAMutexAHolderIsStandingOn pins the
// identity guarantee the deletion used to break.
//
// forgetJobBarrierState deleted the map entry unconditionally, so the next
// jobBarrierLock minted a SECOND mutex for the same job — which is the same as
// having no mutex at all. Drain is DESTRUCTIVE, so two concurrent barriers
// over one file split its articles between them: each acks only its own half
// while both believe they checkpointed the file, and whichever confirms
// releases the reports the other never saw — so those articles are neither
// acked nor re-reported, and only a restart recovers them. See
// jobBarrierLock, which states the same argument at the production site.
//
// The delete is reachable from INSIDE a live barrier, which is what makes this
// reachable rather than theoretical: routeFault → stall.Fail →
// Application.Fail → maybeFinalize → enqueuePostProc → forgetJobBarrierState,
// all beneath the checkpointJob call that holds the mutex.
func TestForgetJobBarrierState_DoesNotDropAMutexAHolderIsStandingOn(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)

	held := application.jobBarrierLock("job-a")
	held.Lock()

	// What Fail → maybeFinalize → enqueuePostProc reaches, from inside the
	// critical section above.
	application.forgetJobBarrierState("job-a")

	again := application.jobBarrierLock("job-a")
	if again != held {
		t.Fatal("a second mutex was minted for a job whose first one is still held. " +
			"Two barriers can now run concurrently over one job, and Drain is " +
			"destructive: they split the file's articles, each acks only its half, " +
			"and whichever calls Confirm releases the reports the other never saw — " +
			"so those articles are neither acked nor re-reported")
	}
	held.Unlock()
	application.releaseJobBarrierLock("job-a")

	// The deletion is deferred, not cancelled — otherwise the map grows by one
	// entry per job ever downloaded, which is what forgetJobBarrierState is
	// for.
	application.releaseJobBarrierLock("job-a")
	application.barrierMu.Lock()
	defer application.barrierMu.Unlock()
	if _, ok := application.jobBarrierMu["job-a"]; ok {
		t.Error("the entry survived after its last holder released it; the deferral " +
			"became a leak, one entry per job ever downloaded")
	}
}

// TestSettleJobBytes_LosesNothingToAConcurrentWrite pins the arithmetic that
// makes the read-then-settle pair safe WITHOUT a single acquisition.
//
// This used to be a read-and-clear under one lock, and it had to be: an article
// written between a separate read and a separate reset was added to the
// accumulator and then deleted with it — counted toward neither window, so the
// "bytes still at risk" figure drifted DOWN over a run of failed barriers. That
// is the direction R26 most needs it not to drift, because the figure is read
// as reassurance: it says nothing is at risk at the moment when everything
// written since the last real barrier is.
//
// settleJobBytes SUBTRACTS the figure the barrier read rather than clearing the
// entry, so the gap stopped mattering — a byte written inside it survives the
// settle instead of vanishing with it. That is what allows the retirement to
// move to the success path, which is what closes the window in which a job at
// risk had no entry for jobsAtRisk to find.
//
// A writer runs concurrently throughout, so the accumulator is being added to
// across every read. Whatever the interleaving, the invariant holds: every byte
// is either settled or still in the accumulator, and never both, and never
// neither.
func TestSettleJobBytes_LosesNothingToAConcurrentWrite(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	// High enough that noteJobBytes never crosses it and blocks on the kick
	// channel; the accumulator arithmetic is what is under test.
	application.checkpointBytes = 1 << 40

	const writes = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range writes {
			application.noteJobBytes("job-a", 1)
		}
	}()

	settle := func() int64 {
		n := application.pendingBytesFor("job-a")
		application.settleJobBytes("job-a", n)
		return n
	}

	var settled int64
	for {
		settled += settle()
		select {
		case <-done:
			settled += settle()
			left := application.pendingBytesFor("job-a")
			if settled != writes || left != 0 {
				t.Fatalf("settled %d and left %d, want %d and 0 — a byte added between the "+
					"read and the settle was counted toward neither window", settled, left, writes)
			}
			return
		default:
		}
	}
}

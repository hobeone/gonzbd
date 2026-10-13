package directunpack

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// waitReturns runs du.Wait and fails the test if it has not returned within
// d.
func waitReturns(t *testing.T, du *DirectUnpacker, d time.Duration) {
	t.Helper()
	returned := make(chan struct{})
	go func() {
		du.Wait()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(d):
		t.Fatalf("Wait did not return within %v (status %+v)", d, du.Status())
	}
}

// A set whose first volume arrives after the previous set has settled and run
// has gone idle is still extracted, and Wait does not return before it is.
func TestDirectUnpack_SetAddedAfterTheQueueEmptiedIsExtracted(t *testing.T) {
	srcDir := testdataDir(t)
	workDir := t.TempDir()
	names := []string{"single_rar5.rar"}
	for i := 1; i <= 10; i++ {
		names = append(names, fmt.Sprintf("multi_new.part%02d.rar", i))
	}
	for _, name := range names {
		copyRAR(t, srcDir, workDir, name)
	}

	du := New(testLogger(t), "test-job", workDir, t.TempDir(), Options{})
	du.SetAllFilenames(names)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	t.Cleanup(du.Abort)

	du.Add(ctx, names[0], filepath.Join(workDir, names[0]))
	// The first set settles and nothing else is queued: run is idle, or (before
	// the fix) has exited. Status reports neither as active.
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, settled := du.Results()["single_rar5"]
		if settled && !du.Status().Active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("single_rar5 never settled (status %+v)", du.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, name := range names[1:] {
		du.Add(ctx, name, filepath.Join(workDir, name))
	}
	waitReturns(t, du, 10*time.Second)

	if _, ok := du.Results()["multi_new"]; !ok {
		t.Errorf("multi_new has no success: results %v, failures %v, skipped %v",
			du.Results(), du.Failures(), du.Skipped())
	}
	du.mu.Lock()
	queued := du.nextSets
	du.mu.Unlock()
	if len(queued) != 0 {
		t.Errorf("nextSets = %q after Wait returned, want empty: a queued set got no outcome", queued)
	}
}

// awaitIdle waits until run has settled the set named settled and is parked
// in awaitNextSet with nothing queued.
func awaitIdle(t *testing.T, du *DirectUnpacker, settled string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		du.mu.Lock()
		_, ok := du.successSets[settled]
		idle := du.idle
		du.mu.Unlock()
		if ok && idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never went idle after %s (status %+v)", settled, du.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startedIdle returns an unpacker that has extracted single_rar5 and is parked
// in awaitNextSet, with the multi_new volumes copied into its work dir.
func startedIdle(t *testing.T) (du *DirectUnpacker, workDir string) {
	t.Helper()
	srcDir := testdataDir(t)
	workDir = t.TempDir()
	names := []string{"single_rar5.rar"}
	for i := 1; i <= 10; i++ {
		names = append(names, fmt.Sprintf("multi_new.part%02d.rar", i))
	}
	for _, name := range names {
		copyRAR(t, srcDir, workDir, name)
	}
	du = New(testLogger(t), "test-job", workDir, t.TempDir(), Options{})
	du.SetAllFilenames(names)
	du.Add(t.Context(), names[0], filepath.Join(workDir, names[0]))
	awaitIdle(t, du, "single_rar5")
	return du, workDir
}

// A set queued after run started, whose volume 1 never arrives, is settled
// when Wait ends the input: run has popped it and waits for volume 1, which
// will not come.
func TestDirectUnpack_WaitSettlesALateSetWithoutVolume1(t *testing.T) {
	du, workDir := startedIdle(t)
	t.Cleanup(du.Abort)
	name := "multi_new.part02.rar"
	du.Add(t.Context(), name, filepath.Join(workDir, name))
	deadline := time.Now().Add(10 * time.Second)
	for du.Status().CurrentSet != "multi_new" {
		if time.Now().After(deadline) {
			t.Fatalf("run never took multi_new (status %+v)", du.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}

	waitReturns(t, du, 5*time.Second)

	if f, ok := du.Failures()["multi_new"]; !ok || f.Reason != errFirstVolumeMissing.Error() {
		t.Errorf("Failures()[multi_new] = %+v, %v; want %q", f, ok, errFirstVolumeMissing)
	}
}

// Each wake-up of a run parked in awaitNextSet with an empty queue: a set
// Add queues, Wait ending the input, and Abort.
func TestDirectUnpack_ParkedRunWakes(t *testing.T) {
	t.Run("Add queues a set", func(t *testing.T) {
		du, workDir := startedIdle(t)
		t.Cleanup(du.Abort)
		for i := 1; i <= 10; i++ {
			name := fmt.Sprintf("multi_new.part%02d.rar", i)
			du.Add(t.Context(), name, filepath.Join(workDir, name))
		}
		// Not Wait: its own wake-up would stand in for Add's.
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, ok := du.Results()["multi_new"]; ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the parked run never took multi_new (status %+v)", du.Status())
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	t.Run("Wait ends the input", func(t *testing.T) {
		du, _ := startedIdle(t)
		t.Cleanup(du.Abort)
		waitReturns(t, du, 5*time.Second)
	})
	t.Run("Abort", func(t *testing.T) {
		du, _ := startedIdle(t)
		returned := make(chan struct{})
		go func() {
			du.Abort()
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("Abort did not return: the parked run never woke")
		}
	})
}

// A volume added after a started run has exited is refused, as one added
// after Wait on an unpacker that never started is.
func TestDirectUnpack_AddAfterAStartedRunFinishedIsRefused(t *testing.T) {
	du, workDir := startedIdle(t)
	waitReturns(t, du, 5*time.Second)

	name := "multi_new.part01.rar"
	du.Add(t.Context(), name, filepath.Join(workDir, name))

	du.mu.Lock()
	defer du.mu.Unlock()
	if len(du.nextSets) != 0 || du.curSetname != "" || len(du.completedVols["multi_new"]) != 0 {
		t.Errorf("Add after a finished run admitted the volume: nextSets %q, current %q, volumes %v",
			du.nextSets, du.curSetname, du.completedVols["multi_new"])
	}
}

// Wait on an unpacker whose run never started settles each queued set: no
// set's first volume arrived, and none will.
func TestDirectUnpack_WaitSettlesSetsWhoseFirstVolumeNeverArrived(t *testing.T) {
	srcDir := testdataDir(t)
	workDir := t.TempDir()
	name := "multi_new.part02.rar"
	path := copyRAR(t, srcDir, workDir, name)

	du := New(testLogger(t), "test-job", workDir, t.TempDir(), Options{})
	du.SetAllFilenames([]string{"multi_new.part01.rar", name})
	du.Add(t.Context(), name, path)

	waitReturns(t, du, 2*time.Second)

	if f, ok := du.Failures()["multi_new"]; !ok || f.Reason != errFirstVolumeMissing.Error() {
		t.Errorf("Failures()[multi_new] = %+v, %v; want %q", f, ok, errFirstVolumeMissing)
	}
	select {
	case <-du.Done():
	default:
		t.Error("Done is open after Wait returned")
	}
}

// A volume added after Wait has returned is refused: no run is left to
// extract its set, so queueing it would leave the set with no outcome.
func TestDirectUnpack_AddAfterWaitIsRefused(t *testing.T) {
	du := New(testLogger(t), "test-job", t.TempDir(), t.TempDir(), Options{})
	waitReturns(t, du, time.Second)

	du.Add(t.Context(), "movie.part01.rar", "/nonexistent/movie.part01.rar")

	du.mu.Lock()
	defer du.mu.Unlock()
	if du.started || len(du.nextSets) != 0 || du.curSetname != "" {
		t.Errorf("Add after Wait admitted the set: started %v, nextSets %q, current %q",
			du.started, du.nextSets, du.curSetname)
	}
}

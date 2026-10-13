package directunpack

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
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

	if f, ok := du.Failures()["multi_new"]; !ok || f.Reason != volumeMissing("multi_new", 1).Error() {
		t.Errorf("Failures()[multi_new] = %+v, %v; want %q", f, ok, volumeMissing("multi_new", 1))
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

	if f, ok := du.Failures()["multi_new"]; !ok || f.Reason != volumeMissing("multi_new", 1).Error() {
		t.Errorf("Failures()[multi_new] = %+v, %v; want %q", f, ok, volumeMissing("multi_new", 1))
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

// A volume added after Wait has been called, while run is still extracting a
// set, is refused: Wait declared that no more volumes are coming.
func TestDirectUnpack_AddWhileWaitIsExtractingIsRefused(t *testing.T) {
	srcDir := testdataDir(t)
	workDir := t.TempDir()
	const held = "single_rar5.rar"
	copyRAR(t, srcDir, workDir, held)
	late := "multi_new.part01.rar"
	copyRAR(t, srcDir, workDir, late)

	release := make(chan struct{})
	var hold sync.Once
	du := New(testLogger(t), "test-job", workDir, t.TempDir(), Options{
		OnLine: func(string) { hold.Do(func() { <-release }) },
	})
	du.SetAllFilenames([]string{held, late})
	t.Cleanup(du.Abort)
	var releaseOnce sync.Once
	releaseRun := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRun) // before du.Abort, which waits for run
	du.Add(t.Context(), held, filepath.Join(workDir, held))

	waited := make(chan struct{})
	go func() {
		du.Wait()
		close(waited)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		du.mu.Lock()
		ended := du.inputEnded
		du.mu.Unlock()
		if ended {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Wait never ended the input")
		}
		time.Sleep(5 * time.Millisecond)
	}

	du.Add(t.Context(), late, filepath.Join(workDir, late))

	du.mu.Lock()
	queued, vols := du.nextSets, len(du.completedVols["multi_new"])
	du.mu.Unlock()
	if len(queued) != 0 || vols != 0 {
		t.Errorf("Add after Wait admitted the volume: nextSets %q, multi_new volumes %d", queued, vols)
	}
	releaseRun()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after the extraction was released")
	}
	if _, ok := du.Results()["single_rar5"]; !ok {
		t.Errorf("single_rar5 was not extracted: failures %v", du.Failures())
	}
}

// A volume added after run exited because its context was cancelled, with no
// Wait or Abort, is refused: no run is left to extract its set.
func TestDirectUnpack_AddAfterRunExitedOnCancellationIsRefused(t *testing.T) {
	srcDir := testdataDir(t)
	workDir := t.TempDir()
	const first = "single_rar5.rar"
	copyRAR(t, srcDir, workDir, first)
	late := "multi_new.part01.rar"
	copyRAR(t, srcDir, workDir, late)

	du := New(testLogger(t), "test-job", workDir, t.TempDir(), Options{})
	du.SetAllFilenames([]string{first, late})
	t.Cleanup(du.Abort)
	ctx, cancel := context.WithCancel(t.Context())
	du.Add(ctx, first, filepath.Join(workDir, first))
	awaitIdle(t, du, "single_rar5")
	cancel()
	select {
	case <-du.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit after its context was cancelled")
	}

	du.Add(t.Context(), late, filepath.Join(workDir, late))

	du.mu.Lock()
	defer du.mu.Unlock()
	if len(du.nextSets) != 0 || len(du.completedVols["multi_new"]) != 0 {
		t.Errorf("Add after run exited admitted the volume: nextSets %q, volumes %v",
			du.nextSets, du.completedVols["multi_new"])
	}
}

// A set whose final volume is absent from the job's filenames is not known
// to be missing it: the feed ends after the highest listed volume, and the
// set still fails, from the RAR reader, rather than being reported extracted.
func TestDirectUnpack_WaitFailsASetMissingAnUnlistedFinalVolume(t *testing.T) {
	srcDir := testdataDir(t)
	workDir := t.TempDir()
	var names []string
	for i := 1; i <= 9; i++ {
		name := fmt.Sprintf("multi_new.part%02d.rar", i)
		copyRAR(t, srcDir, workDir, name)
		names = append(names, name)
	}
	du := New(testLogger(t), "test-job", workDir, t.TempDir(), Options{})
	du.SetAllFilenames(names)
	t.Cleanup(du.Abort)
	for _, name := range names {
		du.Add(t.Context(), name, filepath.Join(workDir, name))
	}

	waitReturns(t, du, 5*time.Second)

	if r, ok := du.Results()["multi_new"]; ok {
		t.Errorf("multi_new reported extracted without volume 10: %+v", r)
	}
	if _, ok := du.Failures()["multi_new"]; !ok {
		t.Errorf("multi_new has no failure: failures %v", du.Failures())
	}
}

// A set whose volume 1 arrived but a later volume never does is settled when
// Wait ends the input, rather than holding Wait until Abort: the feeder is
// waiting for that volume, and none is coming. The volume is either absent
// from the job's files (an incomplete post: the total is read from the
// highest listed volume) or listed but never fed to Add.
func TestDirectUnpack_WaitSettlesASetMissingALaterVolume(t *testing.T) {
	cases := []struct {
		name   string
		listed bool // part05 is among the job's filenames
	}{
		{name: "absent from the job's files", listed: false},
		{name: "listed but never added", listed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srcDir := testdataDir(t)
			workDir := t.TempDir()
			var listed, added []string
			for i := 1; i <= 10; i++ {
				name := fmt.Sprintf("multi_new.part%02d.rar", i)
				if i == 5 && !tc.listed {
					continue
				}
				listed = append(listed, name)
				if i == 5 {
					continue
				}
				copyRAR(t, srcDir, workDir, name)
				added = append(added, name)
			}

			du := New(testLogger(t), "test-job", workDir, t.TempDir(), Options{})
			du.SetAllFilenames(listed)
			t.Cleanup(du.Abort)
			for _, name := range added {
				du.Add(t.Context(), name, filepath.Join(workDir, name))
			}

			waitReturns(t, du, 5*time.Second)

			if r, ok := du.Results()["multi_new"]; ok {
				t.Errorf("multi_new reported extracted without volume 5: %+v", r)
			}
			const want = "volume 5 of set multi_new never arrived"
			if f, ok := du.Failures()["multi_new"]; !ok || f.Reason != want {
				t.Errorf("Failures()[multi_new] = %+v, %v; want reason %q", f, ok, want)
			}
		})
	}
}

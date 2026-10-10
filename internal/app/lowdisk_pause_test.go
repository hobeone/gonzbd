package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
)

type fakeDiskProbe struct {
	mu            sync.Mutex
	free          int64
	err           error
	missingSubdir string
	lastDir       string
	calls         int
	probedCh      chan struct{}
}

func newFakeDiskProbe(free int64) *fakeDiskProbe {
	return &fakeDiskProbe{
		free:     free,
		probedCh: make(chan struct{}, 256),
	}
}

func (p *fakeDiskProbe) FreeBytes(ctx context.Context, dir string) (int64, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	p.mu.Lock()
	p.lastDir = dir
	p.calls++
	missing := p.missingSubdir != "" && dir == p.missingSubdir
	free, err := p.free, p.err
	p.mu.Unlock()

	select {
	case p.probedCh <- struct{}{}:
	default:
	}

	if missing {
		return 0, os.ErrNotExist
	}
	return free, err
}

func (p *fakeDiskProbe) set(free int64, err error) {
	p.mu.Lock()
	p.free = free
	p.err = err
	p.mu.Unlock()
	for {
		select {
		case <-p.probedCh:
		default:
			return
		}
	}
}

func (p *fakeDiskProbe) waitForProbes(t *testing.T, n int) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for range n {
		select {
		case <-p.probedCh:
		case <-timer.C:
			t.Fatalf("timed out waiting for %d DiskProbe call(s)", n)
		}
	}
}

type syncRecordingEmitter struct {
	mu     sync.Mutex
	events []Event
}

func (e *syncRecordingEmitter) Broadcast(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *syncRecordingEmitter) countQueueUpdated() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	var n int
	for _, ev := range e.events {
		if ev.Type == "queue_updated" {
			n++
		}
	}
	return n
}

func waitUntilCondition(timeout time.Duration, fn func() bool) bool {
	if fn() {
		return true
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ticker.C:
			if fn() {
				return true
			}
		case <-timer.C:
			return fn()
		}
	}
}

// TestLowDiskPause_UnifiesQueuePauseAndAutoResumes pins issue #766:
//  1. handleLowDisk pauses both the dispatcher queue (so /api?mode=queue and
//     /api?mode=status report paused:true) and the downloader, and broadcasts
//     queue_updated.
//  2. While probed free space stays below MinFreeSpace (or returns a probe
//     error), the queue and downloader remain paused.
//  3. Once probed free space recovers to >= MinFreeSpace, the recurring
//     free-space check auto-resumes both the dispatcher and the downloader and
//     broadcasts queue_updated.
//  4. A second low-disk episode after auto-resume starts a fresh watcher and
//     auto-resumes again once free space recovers.
func TestLowDiskPause_UnifiesQueuePauseAndAutoResumes(t *testing.T) {
	t.Parallel()

	dlDir := t.TempDir()
	compDir := t.TempDir()
	adminDir := t.TempDir()
	cfg := testConfig(dlDir, compDir, adminDir)
	cfg.With(func(c *config.Config) {
		c.Downloads.MinFreeSpace = config.ByteSize(1000)
	})

	probe := newFakeDiskProbe(500)
	fd := newFakeDownloader()
	rec := &syncRecordingEmitter{}

	application, err := New(cfg, nil,
		WithDownloader(fd),
		WithDiskProbe(probe),
		WithLowDiskRecheckInterval(5*time.Millisecond),
		WithEventEmitter(rec),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := application.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := application.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	beforeEvents := rec.countQueueUpdated()

	// 1. Trigger low-disk pause.
	application.handleLowDisk(dlDir, 500)

	// 2. Both the dispatcher queue (read by /api?mode=queue and /api?mode=status)
	// and the downloader must be paused immediately, and queue_updated broadcast.
	if !application.Dispatcher().Paused() {
		t.Fatalf("Dispatcher().Paused() = false after handleLowDisk, want true")
	}
	fd.mu.Lock()
	dlPaused := fd.paused
	fd.mu.Unlock()
	if !dlPaused {
		t.Fatalf("downloader paused = false after handleLowDisk, want true")
	}
	afterLowDiskEvents := rec.countQueueUpdated()
	if afterLowDiskEvents <= beforeEvents {
		t.Fatalf("queue_updated events after handleLowDisk = %d, want > %d", afterLowDiskEvents, beforeEvents)
	}

	// Probe error and below-threshold (999 < 1000) must NOT auto-resume across real probe ticks.
	probe.set(2000, errors.New("statfs transient failure"))
	probe.waitForProbes(t, 2)
	if !application.Dispatcher().Paused() {
		t.Fatalf("Dispatcher().Paused() = false after probe error, want true")
	}

	probe.set(999, nil)
	probe.waitForProbes(t, 2)
	if !application.Dispatcher().Paused() {
		t.Fatalf("Dispatcher().Paused() = false at 999 bytes (< 1000 threshold), want true")
	}

	// 3. Raise probed free space to the exact threshold (1000 == MinFreeSpace).
	probe.set(1000, nil)

	// 4. Wait for auto-resume to unpause both dispatcher and downloader and emit queue_updated.
	if !waitUntilCondition(400*time.Millisecond, func() bool {
		fd.mu.Lock()
		p := fd.paused
		fd.mu.Unlock()
		return !application.Dispatcher().Paused() && !p && rec.countQueueUpdated() > afterLowDiskEvents
	}) {
		fd.mu.Lock()
		p := fd.paused
		fd.mu.Unlock()
		t.Fatalf("auto-resume did not unpause after free space recovered: Dispatcher().Paused()=%v, downloader.paused=%v, queue_updated=%d (want > %d)",
			application.Dispatcher().Paused(), p, rec.countQueueUpdated(), afterLowDiskEvents)
	}

	// 5. Trigger a second low-disk episode after auto-resume to prove
	// tryAutoResumeLowDisk cleared lowDiskCancel so the second episode starts a
	// new watcher and auto-resumes.
	probe.set(500, nil)
	application.handleLowDisk(dlDir, 500)
	if !application.Dispatcher().Paused() {
		t.Fatalf("Dispatcher().Paused() = false on second handleLowDisk, want true")
	}
	probe.set(1000, nil)
	if !waitUntilCondition(400*time.Millisecond, func() bool {
		fd.mu.Lock()
		p := fd.paused
		fd.mu.Unlock()
		return !application.Dispatcher().Paused() && !p
	}) {
		t.Fatalf("second low-disk episode did not auto-resume after free space recovered")
	}
}

// TestLowDiskPause_UserPauseOverridesAutoResume pins that a user pause wins
// over low-disk auto-resume whether the user paused before handleLowDisk fired
// or while already low-disk paused.
func TestLowDiskPause_UserPauseOverridesAutoResume(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		pauseBeforeLow bool
	}{
		{name: "user_pauses_before_low_disk", pauseBeforeLow: true},
		{name: "user_pauses_during_low_disk", pauseBeforeLow: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dlDir := t.TempDir()
			compDir := t.TempDir()
			adminDir := t.TempDir()
			cfg := testConfig(dlDir, compDir, adminDir)
			cfg.With(func(c *config.Config) {
				c.Downloads.MinFreeSpace = config.ByteSize(1000)
			})

			probe := newFakeDiskProbe(500)
			fd := newFakeDownloader()

			application, err := New(cfg, nil,
				WithDownloader(fd),
				WithDiskProbe(probe),
				WithLowDiskRecheckInterval(5*time.Millisecond),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := application.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() {
				if err := application.Shutdown(); err != nil {
					t.Errorf("Shutdown: %v", err)
				}
			})

			if tc.pauseBeforeLow {
				application.PauseDownloads()
				if !application.Dispatcher().Paused() {
					t.Fatalf("Dispatcher().Paused() = false after PauseDownloads, want true")
				}
				application.handleLowDisk(dlDir, 500)
			} else {
				application.handleLowDisk(dlDir, 500)
				if !application.Dispatcher().Paused() {
					t.Fatalf("Dispatcher().Paused() = false after handleLowDisk, want true")
				}
				application.PauseDownloads()
			}

			// Recover disk space well above threshold and verify tryAutoResumeLowDisk
			// does not unpause a user-paused daemon.
			probe.set(5000, nil)
			_ = application.tryAutoResumeLowDisk(t.Context(), dlDir)

			if !application.Dispatcher().Paused() {
				t.Fatalf("Dispatcher().Paused() = false after disk recovery, want true (user pause must stick)")
			}
			fd.mu.Lock()
			dlPaused := fd.paused
			fd.mu.Unlock()
			if !dlPaused {
				t.Fatalf("downloader.paused = false after disk recovery, want true (user pause must stick)")
			}

			// Explicit user resume lifts the pause and clears pauseReasonUser so a
			// subsequent low-disk event can claim pauseReasonLowDisk again.
			application.ResumeDownloads()
			if application.Dispatcher().Paused() {
				t.Fatalf("Dispatcher().Paused() = true after ResumeDownloads, want false")
			}
			fd.mu.Lock()
			dlPaused = fd.paused
			fd.mu.Unlock()
			if dlPaused {
				t.Fatalf("downloader.paused = true after ResumeDownloads, want false")
			}

			// Trigger low-disk again after ResumeDownloads to prove ResumeDownloads
			// cleared pauseReasonUser back to pauseReasonNone.
			probe.set(500, nil)
			application.handleLowDisk(dlDir, 500)
			probe.set(5000, nil)
			if !waitUntilCondition(400*time.Millisecond, func() bool {
				return !application.Dispatcher().Paused()
			}) {
				t.Fatalf("Dispatcher().Paused() stayed true on second low-disk cycle after ResumeDownloads")
			}
		})
	}
}

// TestTryAutoResumeLowDisk_DirectBranches exercises tryAutoResumeLowDisk,
// watchLowDisk, stopLowDiskWatchLocked, and stopLowDiskWatch directly.
func TestTryAutoResumeLowDisk_DirectBranches(t *testing.T) {
	t.Parallel()

	dlDir := t.TempDir()
	compDir := t.TempDir()
	adminDir := t.TempDir()
	cfg := testConfig(dlDir, compDir, adminDir)
	cfg.With(func(c *config.Config) {
		c.Downloads.MinFreeSpace = config.ByteSize(1000)
	})

	probe := newFakeDiskProbe(500)
	fd := newFakeDownloader()
	rec := &syncRecordingEmitter{}

	application, err := New(cfg, nil,
		WithDownloader(fd),
		WithDiskProbe(probe),
		WithLowDiskRecheckInterval(time.Hour),
		WithEventEmitter(rec),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	appCtx, appCancel := context.WithCancel(t.Context())
	application.ctx = appCtx
	t.Cleanup(func() {
		appCancel()
		application.stopLowDiskWatch()
	})

	// 1. Calling handleLowDisk twice in a row must start at most one watchLowDisk
	// goroutine (guarded by lowDiskCancel == nil), and PauseDownloads must cancel
	// it immediately via stopLowDiskWatchLocked so lowDiskWg.Wait returns.
	application.handleLowDisk(dlDir, 500)
	application.handleLowDisk(dlDir, 500)
	application.PauseDownloads()
	waitDone := make(chan struct{})
	go func() {
		application.lowDiskWg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(100 * time.Millisecond):
		appCancel()
		t.Fatalf("PauseDownloads did not cancel active low-disk watch (or duplicate watcher leaked)")
	}

	// 2. While pauseReason is pauseReasonUser, tryAutoResumeLowDisk with
	// recovered disk space returns true (nothing left to watch) without resuming.
	probe.set(2000, nil)
	if !application.tryAutoResumeLowDisk(t.Context(), dlDir) {
		t.Fatalf("tryAutoResumeLowDisk on user pause = false, want true")
	}
	if !application.Dispatcher().Paused() {
		t.Fatalf("tryAutoResumeLowDisk unpaused a user-paused dispatcher")
	}
	fd.mu.Lock()
	dlPaused := fd.paused
	fd.mu.Unlock()
	if !dlPaused {
		t.Fatalf("tryAutoResumeLowDisk unpaused a user-paused downloader")
	}

	// 3. ResumeDownloads must also cancel an active low-disk watch immediately.
	application.ResumeDownloads()
	probe.set(500, nil)
	application.handleLowDisk(dlDir, 500)
	application.ResumeDownloads()
	waitDone2 := make(chan struct{})
	go func() {
		application.lowDiskWg.Wait()
		close(waitDone2)
	}()
	select {
	case <-waitDone2:
	case <-time.After(100 * time.Millisecond):
		appCancel()
		t.Fatalf("ResumeDownloads did not cancel active low-disk watch")
	}

	// 4. Direct watchLowDisk cancellation via stopLowDiskWatchLocked.
	watchCtx, cancel := context.WithCancel(t.Context())
	application.mu.Lock()
	application.lowDiskCancel = cancel
	application.stopLowDiskWatchLocked()
	application.mu.Unlock()
	application.watchLowDisk(watchCtx, dlDir, time.Hour)

	// 5. When the per-job subdirectory passed to handleLowDisk was deleted to
	// free disk space (FreeBytes returns os.ErrNotExist), tryAutoResumeLowDisk
	// falls back to probing downloadDir and auto-resumes.
	deletedJobDir := filepath.Join(dlDir, "deleted-job")
	probe.mu.Lock()
	probe.missingSubdir = deletedJobDir
	probe.free = 2000
	probe.err = nil
	probe.mu.Unlock()
	application.handleLowDisk(deletedJobDir, 500)
	if !application.Dispatcher().Paused() {
		t.Fatalf("Dispatcher().Paused() = false after handleLowDisk on deletedJobDir, want true")
	}
	fd.mu.Lock()
	dlPausedOnDeletedDir := fd.paused
	fd.mu.Unlock()
	if !dlPausedOnDeletedDir {
		t.Fatalf("downloader.paused = false after handleLowDisk on deletedJobDir, want true")
	}
	if !application.tryAutoResumeLowDisk(t.Context(), deletedJobDir) {
		t.Fatalf("tryAutoResumeLowDisk(deletedJobDir) = false, want true via downloadDir fallback")
	}
	if application.Dispatcher().Paused() {
		t.Fatalf("Dispatcher().Paused() = true after tryAutoResumeLowDisk fallback, want false")
	}
	fd.mu.Lock()
	dlPausedAfterFallback := fd.paused
	fd.mu.Unlock()
	if dlPausedAfterFallback {
		t.Fatalf("downloader.paused = true after tryAutoResumeLowDisk fallback, want false")
	}
	application.mu.Lock()
	leakedCancel := application.lowDiskCancel != nil
	application.stopLowDiskWatchLocked()
	application.mu.Unlock()
	if leakedCancel {
		t.Fatalf("tryAutoResumeLowDisk did not clear lowDiskCancel")
	}
	probe.mu.Lock()
	probe.missingSubdir = ""
	probe.mu.Unlock()

	// 6. stopLowDiskWatch waits on lowDiskWg.Wait() before returning.
	watchCtx2, cancel2 := context.WithCancel(t.Context())
	application.mu.Lock()
	application.pauseReason = pauseReasonLowDisk
	application.lowDiskCancel = cancel2
	var workerExited atomic.Bool
	application.lowDiskWg.Go(func() {
		<-watchCtx2.Done()
		for range 64 {
			runtime.Gosched()
		}
		workerExited.Store(true)
	})
	application.mu.Unlock()
	application.stopLowDiskWatch()
	if !workerExited.Load() {
		t.Fatalf("stopLowDiskWatch returned before lowDiskWg worker exited")
	}

	// 7. handleLowDisk does not spawn a watcher while stopping or stopped, and
	// tryAutoResumeLowDisk does not unpause while stopping or stopped.
	application.stopping.Store(true)
	application.handleLowDisk(dlDir, 500)
	application.mu.Lock()
	hasCancelWhileStopping := application.lowDiskCancel != nil
	application.mu.Unlock()
	if hasCancelWhileStopping {
		application.stopLowDiskWatch()
		t.Fatalf("handleLowDisk spawned a watcher while application.stopping is true")
	}
	if !application.tryAutoResumeLowDisk(t.Context(), dlDir) {
		t.Fatalf("tryAutoResumeLowDisk while stopping = false, want true")
	}
	if !application.Dispatcher().Paused() {
		t.Fatalf("tryAutoResumeLowDisk unpaused dispatcher while application.stopping is true")
	}
	application.stopping.Store(false)
	application.ResumeDownloads()

	application.stopped.Store(true)
	application.handleLowDisk(dlDir, 500)
	application.mu.Lock()
	hasCancelWhileStopped := application.lowDiskCancel != nil
	application.mu.Unlock()
	if hasCancelWhileStopped {
		application.stopLowDiskWatch()
		t.Fatalf("handleLowDisk spawned a watcher while application.stopped is true")
	}
	if !application.tryAutoResumeLowDisk(t.Context(), dlDir) {
		t.Fatalf("tryAutoResumeLowDisk while stopped = false, want true")
	}
	if !application.Dispatcher().Paused() {
		t.Fatalf("tryAutoResumeLowDisk unpaused dispatcher while application.stopped is true")
	}
	application.stopped.Store(false)
	application.ResumeDownloads()

	// 8. stopWorkers stops any active low-disk watch and clears pauseReasonLowDisk.
	application.handleLowDisk(dlDir, 500)
	application.stopWorkers(time.Second, nil)
	application.mu.Lock()
	cancelAfterStopWorkers := application.lowDiskCancel
	reasonAfterStopWorkers := application.pauseReason
	application.mu.Unlock()
	if cancelAfterStopWorkers != nil || reasonAfterStopWorkers != pauseReasonNone {
		t.Fatalf("after stopWorkers: lowDiskCancel=%v, pauseReason=%q, want nil and empty",
			cancelAfterStopWorkers, reasonAfterStopWorkers)
	}

	// 9. handleLowDisk and tryAutoResumeLowDisk call downloader.Pause/Resume
	// under app.mu, so a concurrent ResumeDownloads or PauseDownloads can never
	// interleave between dispatcher.Pause/Resume and downloader.Pause/Resume
	// and leave the two pause flags diverged.
	application.ResumeDownloads()
	resumeDone := make(chan struct{})
	fd.mu.Lock()
	fd.onBeforePause = func() {
		fd.mu.Lock()
		fd.onBeforePause = nil
		fd.mu.Unlock()
		if application.mu.TryLock() {
			application.mu.Unlock()
			application.ResumeDownloads()
			close(resumeDone)
			return
		}
		go func() {
			application.ResumeDownloads()
			close(resumeDone)
		}()
	}
	fd.mu.Unlock()
	application.handleLowDisk(dlDir, 500)
	select {
	case <-resumeDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("handleLowDisk did not invoke downloader.Pause()")
	}
	fd.mu.Lock()
	pausedAfterRace1 := fd.paused
	fd.mu.Unlock()
	if pausedAfterRace1 || application.Dispatcher().Paused() {
		t.Fatalf("handleLowDisk vs ResumeDownloads diverged: downloader.paused=%v, Dispatcher().Paused()=%v, want both false",
			pausedAfterRace1, application.Dispatcher().Paused())
	}

	application.handleLowDisk(dlDir, 500)
	probe.set(2000, nil)
	pauseDone := make(chan struct{})
	fd.mu.Lock()
	fd.onBeforeResume = func() {
		fd.mu.Lock()
		fd.onBeforeResume = nil
		fd.mu.Unlock()
		if application.mu.TryLock() {
			application.mu.Unlock()
			application.PauseDownloads()
			close(pauseDone)
			return
		}
		go func() {
			application.PauseDownloads()
			close(pauseDone)
		}()
	}
	fd.mu.Unlock()
	if !application.tryAutoResumeLowDisk(t.Context(), dlDir) {
		t.Fatalf("tryAutoResumeLowDisk = false, want true")
	}
	select {
	case <-pauseDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("tryAutoResumeLowDisk did not invoke downloader.Resume()")
	}
	fd.mu.Lock()
	pausedAfterRace2 := fd.paused
	fd.mu.Unlock()
	if !pausedAfterRace2 || !application.Dispatcher().Paused() {
		t.Fatalf("tryAutoResumeLowDisk vs PauseDownloads diverged: downloader.paused=%v, Dispatcher().Paused()=%v, want both true",
			pausedAfterRace2, application.Dispatcher().Paused())
	}
}

// TestLowDiskPause_AssemblerCallbackAutoResumesRealDownloader exercises the
// full assembler -> OnLowDisk (handleLowDisk) -> watchLowDisk ->
// tryAutoResumeLowDisk -> real downloader dispatch path, and pins that the
// low-disk pause holds a job interrupted mid-download at Fetching and gates a
// job queued during it (Reason=GlobalPause) until auto-resume lifts the pause.
func TestLowDiskPause_AssemblerCallbackAutoResumesRealDownloader(t *testing.T) {
	t.Parallel()

	dlDir := t.TempDir()
	compDir := t.TempDir()
	adminDir := t.TempDir()
	srv := nntptest.New(t)
	const msg2 = "lowdisk-real-after@test"
	srv.AddArticle(msg2, []byte("=ybegin line=128 size=4 name=part2.bin\r\ntest\r\n=yend size=4\r\n"))

	// Assembler runs checkDiskSpace every 16 write requests (diskCheckInterval = 16),
	// over the directories of the files it still holds open. finalizeFile closes a
	// completed file and drops it from that set before the check runs, so the
	// 16th write must leave part1.bin open: it has 17 parts. The check on the 16th
	// write calls OnLowDisk -> handleLowDisk, which pauses the dispatcher and the
	// downloader before the assembler worker takes another write, so j1 cannot
	// leave Fetching while the pause holds.
	const numParts = 17
	articles := make([]nzb.Article, numParts)
	for i := range numParts {
		id := fmt.Sprintf("lowdisk-real-%d@test", i+1)
		begin := i*4 + 1
		end := (i + 1) * 4
		body := fmt.Sprintf("=ybegin part=%d total=%d line=128 size=%d name=part1.bin\r\n=ypart begin=%d end=%d\r\ntest\r\n=yend size=4 part=%d\r\n",
			i+1, numParts, numParts*4, begin, end, i+1)
		srv.AddArticle(id, []byte(body))
		articles[i] = nzb.Article{ID: id, Bytes: 4, Number: i + 1}
	}

	cfg := testConfig(dlDir, compDir, adminDir, srv.ServerConfig("primary", 1))
	cfg.With(func(c *config.Config) {
		// Exceeds any real filesystem's free bytes so the assembler's 16th
		// write triggers OnLowDisk -> app.handleLowDisk.
		c.Downloads.MinFreeSpace = config.ByteSize(1 << 60)
	})

	probe := newFakeDiskProbe(500)
	application, err := New(cfg, nil,
		WithDiskProbe(probe),
		WithLowDiskRecheckInterval(5*time.Millisecond),
		WithMetricsPushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := application.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = application.Shutdown()
	})

	j1, hdr1, raw1 := buildTestIngestJob(t, application, &nzb.NZB{Files: []nzb.File{{
		Subject:  `"part1.bin" yEnc (1/17)`,
		Bytes:    int64(numParts * 4),
		Articles: articles,
	}}}, "lowdisk-job1")
	if err := application.AddJob(t.Context(), j1, hdr1, raw1, true); err != nil {
		t.Fatalf("AddJob(j1): %v", err)
	}

	// Wait for the assembler's OnLowDisk callback to pause the dispatcher with j1
	// still at Fetching.
	if !waitUntilCondition(5*time.Second, func() bool {
		if !application.Dispatcher().Paused() {
			return false
		}
		row1, ok := application.Dispatcher().Row(j1.ID())
		return ok && row1.View.State == job.Fetching
	}) {
		row1, ok := application.Dispatcher().Row(j1.ID())
		t.Fatalf("timed out waiting for the low-disk pause with j1 at Fetching (Paused=%v, ok=%v, view=%+v)",
			application.Dispatcher().Paused(), ok, row1.View)
	}

	// Queue a second job while low-disk paused; it must be gated by GlobalPause
	// and not fetched across watcher probes, and j1 must stay at Fetching.
	j2, hdr2, raw2 := buildTestIngestJob(t, application, &nzb.NZB{Files: []nzb.File{{
		Subject:  `"part2.bin" yEnc (1/1)`,
		Bytes:    4,
		Articles: []nzb.Article{{ID: msg2, Bytes: 4, Number: 1}},
	}}}, "lowdisk-job2")
	if err := application.AddJob(t.Context(), j2, hdr2, raw2, true); err != nil {
		t.Fatalf("AddJob(j2): %v", err)
	}
	probe.waitForProbes(t, 2)
	if got := srv.FetchCount(msg2); got != 0 {
		t.Fatalf("srv.FetchCount(msg2) = %d while low-disk paused, want 0", got)
	}
	row2, ok := application.Dispatcher().Row(j2.ID())
	if !ok || row2.View.Running || row2.View.Reason != job.GlobalPause {
		t.Fatalf("j2 not gated by GlobalPause while low-disk paused: ok=%v view=%+v", ok, row2.View)
	}
	row1AfterProbes, ok := application.Dispatcher().Row(j1.ID())
	if !ok || row1AfterProbes.View.State != job.Fetching {
		t.Fatalf("j1 advanced while low-disk paused: ok=%v view=%+v", ok, row1AfterProbes.View)
	}

	// Recover disk threshold and probed free space; auto-resume must unpause,
	// advance j1 past Fetching, and fetch msg2.
	application.SetMinFreeSpace(1000)
	probe.set(2000, nil)

	if !waitUntilCondition(5*time.Second, func() bool {
		row, exists := application.Dispatcher().Row(j1.ID())
		j1Advanced := !exists || row.View.State != job.Fetching
		return !application.Dispatcher().Paused() && j1Advanced && srv.FetchCount(msg2) > 0
	}) {
		t.Fatalf("timed out waiting for auto-resume, j1 advance, and msg2 fetch (Paused=%v, msg2 fetches=%d)",
			application.Dispatcher().Paused(), srv.FetchCount(msg2))
	}
}

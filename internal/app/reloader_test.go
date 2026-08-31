package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// newBareQueueJob builds a real, empty-manifest *queue.Job via queue.NewJob,
// with ID/MD5 set to the caller's chosen values — the plain header fields
// are unaffected by the Manifest/Progress split, so setting them after
// construction but before Queue.Add is safe.
func newBareQueueJob(t *testing.T, id, md5 string) *queue.Job {
	t.Helper()
	job, err := queue.NewJob(&nzb.NZB{}, queue.AddOptions{Filename: id + ".nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	job.ID = id
	job.MD5 = md5
	return job
}

func TestApplication_ReloadOptions(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	fd := newFakeDownloader()
	app, err := New(cfg, nil, WithDownloader(fd))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 1. Test ReloadDownloadOptions happy path
	cfg.With(func(c *config.Config) {
		c.Downloads.MinFreeSpace = config.ByteSize(50 * 1024 * 1024)
		c.Downloads.MaxArtTries = 5
		c.Downloads.MaxArtOpt = 10
		c.Downloads.TopOnly = true
		c.Downloads.PropagationDelay = 30
	})
	app.ReloadDownloadOptions(cfg.Downloads)

	// Assert on Assembler
	if got := app.assembler.MinFreeBytes(); got != 50*1024*1024 {
		t.Errorf("expected MinFreeBytes to be 50MiB, got %d", got)
	}

	// Assert on fakeDownloader options
	fd.mu.Lock()
	if fd.maxArtTries != 5 {
		t.Errorf("expected maxArtTries to be 5, got %d", fd.maxArtTries)
	}
	if fd.maxArtOpt != 10 {
		t.Errorf("expected maxArtOpt to be 10, got %d", fd.maxArtOpt)
	}
	if !fd.topOnly {
		t.Error("expected topOnly to be true")
	}
	if fd.propagationDelay != 30*time.Minute {
		t.Errorf("expected propagationDelay to be 30m, got %v", fd.propagationDelay)
	}
	fd.mu.Unlock()

	// 2. Test ReloadGeneralOptions (global level)
	cfg.With(func(c *config.Config) {
		c.General.LogLevel = "debug"
		c.General.LogLevels = map[string]string{"downloader": "warn"}
	})
	app.ReloadGeneralOptions(cfg.General)

	if got := globalLevelVar.Level(); got != slog.LevelDebug {
		t.Errorf("expected global level to be Debug, got %v", got)
	}

	componentLevelsMu.RLock()
	lvl, ok := componentLevels["downloader"]
	componentLevelsMu.RUnlock()
	if !ok || lvl != slog.LevelWarn {
		t.Errorf("expected downloader component level to be Warn, got %v (ok=%t)", lvl, ok)
	}

	// 3. Test ReloadGeneralOptions invalid global level (should log error and NOT apply)
	cfg.With(func(c *config.Config) {
		c.General.LogLevel = "invalid-level"
	})
	app.ReloadGeneralOptions(cfg.General)
	// Global level should still be debug from previous step
	if got := globalLevelVar.Level(); got != slog.LevelDebug {
		t.Errorf("expected global level to remain Debug on invalid config, got %v", got)
	}

	// 4. Test ReloadGeneralOptions invalid component level (should log error and NOT apply)
	cfg.With(func(c *config.Config) {
		c.General.LogLevel = "debug"
		c.General.LogLevels = map[string]string{"downloader": "invalid-level"}
	})
	app.ReloadGeneralOptions(cfg.General)
	// Downloader level should still be warn from previous step
	componentLevelsMu.RLock()
	lvl, ok = componentLevels["downloader"]
	componentLevelsMu.RUnlock()
	if !ok || lvl != slog.LevelWarn {
		t.Errorf("expected downloader component level to remain Warn on invalid config, got %v", lvl)
	}

	// 5. Test ReloadGeneralOptions invalid global level but valid component level (partial apply)
	cfg.With(func(c *config.Config) {
		c.General.LogLevel = "invalid-level"
		c.General.LogLevels = map[string]string{"downloader": "info"}
	})
	app.ReloadGeneralOptions(cfg.General)
	// Global level should remain debug
	if got := globalLevelVar.Level(); got != slog.LevelDebug {
		t.Errorf("expected global level to remain Debug, got %v", got)
	}
	// Downloader level should update to Info
	componentLevelsMu.RLock()
	lvl, ok = componentLevels["downloader"]
	componentLevelsMu.RUnlock()
	if !ok || lvl != slog.LevelInfo {
		t.Errorf("expected downloader component level to update to Info, got %v (ok=%t)", lvl, ok)
	}

	// 6. Test ReloadPostProcOptions (happy path)
	cfg.With(func(c *config.Config) {
		c.PostProc.EnableQuickCheck = true
		c.PostProc.EnableParCleanup = true
		c.PostProc.EnableRarCleanup = true
		c.PostProc.OverwriteFiles = true
		c.PostProc.FlatUnpack = true
		c.PostProc.Permissions = "0755"
		c.PostProc.FolderRename = true
		c.PostProc.ScriptCanFail = true
		c.PostProc.UseGoRAR = true
		c.PostProc.UseGo7z = true
		c.PostProc.UseGoPar2 = true
		c.PostProc.GoRarFallback = true
		c.PostProc.Go7zFallback = true
		c.PostProc.GoPar2Fallback = true
		c.PostProc.Nice = "nice -n 19"
		c.PostProc.Ionice = "ionice -c 3"
		c.PostProc.Par2Command = "par2"
		c.PostProc.UnrarCommand = "unrar"
		c.PostProc.SevenzCommand = "7z"
		c.PostProc.ExtraUnrarParams = "-ppassword"
		c.PostProc.ExtraPar2Params = "-t+"
		c.PostProc.CleanupExtensions = []string{".nfo"}
		c.PostProc.DeobfuscateFilenames = true
		c.PostProc.IgnoreSamples = true
		c.General.ScriptDir = "/scripts"
		c.PostProc.EnableUnrar = true
		c.PostProc.Enable7zip = true
		c.PostProc.PasswordFile = "pass.txt"
		c.PostProc.EnableFileJoin = true
		c.PostProc.EnableRecursive = true
		c.PostProc.DirectUnpack = true
		c.PostProc.DirectUnpackThreads = 4
		c.PostProc.Par2Turbo = true
		c.PostProc.IgnoreUnrarDates = true
	})
	app.ReloadPostProcOptions(cfg.PostProc, cfg.General.ScriptDir)

	// 7. Test ReloadPostProcOptions with parsing errors in extra params (triggers error paths)
	cfg.With(func(c *config.Config) {
		c.PostProc.ExtraUnrarParams = "'unclosed quote"
		c.PostProc.ExtraPar2Params = "'unclosed quote"
	})
	app.ReloadPostProcOptions(cfg.PostProc, cfg.General.ScriptDir)
}

// TestApplication_ReloadPostProcOptions_NoDeadlockUnderReadLock guards against
// a regression of the self-deadlock previously present in
// internal/api/config.go's modeSetConfig handler, which used to call:
//
//	pp := s.config.GetPostProc(); s.app.ReloadPostProcOptions(pp, ...)
//
// ReloadPostProcOptions calls several Set* methods (SetEnableFileJoin,
// SetEnableRecursive, SetDirectUnpack, etc.) that internally call
// app.config.With(...), acquiring the *write* lock on config.Config. Calling
// ReloadPostProcOptions itself while still holding that config's *read* lock
// self-deadlocked the goroutine (sync.RWMutex is not reentrant) — and because
// Go's RWMutex is writer-preferring, every other request that subsequently
// tried to RLock the config queued up behind it too, wedging the whole HTTP
// server (this is what caused the 504s / unresponsive shutdown reported
// against production).
//
// ReloadPostProcOptions's signature now takes a value snapshot
// (config.PostProcConfig) instead of *config.Config, which makes it
// impossible to pass a still-locked config through to it. This test exercises
// the correct pattern — value getter snapshot, then call — and
// guards the snapshot's release-before-call ordering with a timeout in case a
// future change reintroduces the lock-holding call.
func TestApplication_ReloadPostProcOptions_NoDeadlockUnderReadLock(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan struct{})
	go func() {
		pp := cfg.GetPostProc()
		scriptDir := cfg.GetGeneral().ScriptDir
		app.ReloadPostProcOptions(pp, scriptDir)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock: ReloadPostProcOptions never returned")
	}
}

func TestApplication_RunMetricsPush(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	emitter := &eventCounter{onBroadcast: cancel}
	app, err := New(cfg, nil, WithEventEmitter(emitter), WithMetricsPushInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	app.runMetricsPush(ctx)

	if emitter.count == 0 {
		t.Error("expected metrics push to broadcast events")
	}
}

type eventCounter struct {
	count       int
	onBroadcast func()
}

func (e *eventCounter) Broadcast(ev Event) {
	e.count++
	if e.onBroadcast != nil {
		e.onBroadcast()
	}
}

// TestApplication_ReloadPostProcOptions_AppliesStrictSandboxToRunningStage
// guards against a regression where ReloadPostProcOptions stopped applying
// pp.StrictSandbox to the running UnpackStage after Application.SetStrictSandbox
// was removed (config.Validate() now enforces the Linux-only platform
// constraint, but nothing was left to propagate an accepted value to the
// already-running stage on live reload).
func TestApplication_ReloadPostProcOptions_AppliesStrictSandboxToRunningStage(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	cfg.With(func(c *config.Config) {
		c.PostProc.StrictSandbox = false
	})
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	if got := app.StageStrictSandbox(); got {
		t.Fatalf("precondition: StageStrictSandbox() = %v, want false", got)
	}

	app.ReloadPostProcOptions(config.PostProcConfig{StrictSandbox: true}, "")

	if got := app.StageStrictSandbox(); !got {
		t.Errorf("after ReloadPostProcOptions(StrictSandbox: true): StageStrictSandbox() = %v, want true", got)
	}
}

// TestDetectDuplicateNZB covers detectDuplicateNZB directly (extracted from
// AddJob in OPT-9): the MD5-in-queue branch, and the force/!force asymmetry
// (Status is only set by the caller when !force, but Warning differs either
// way — detectDuplicateNZB itself only returns the warning text, so this
// test asserts that contract).
func TestDetectDuplicateNZB(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	a, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Shutdown()

	nzbDir := t.TempDir()

	t.Run("not a duplicate", func(t *testing.T) {
		job := &queue.Job{MD5: "no-match-md5"}
		isDup, warning := a.detectDuplicateNZB(t.Context(), job, false, nzbDir)
		if isDup {
			t.Errorf("isDuplicate = true, want false")
		}
		if warning != "" {
			t.Errorf("warning = %q, want empty", warning)
		}
	})

	t.Run("duplicate via MD5 already in queue, not forced", func(t *testing.T) {
		existing := newBareQueueJob(t, "existing-not-forced", "dup-md5-queue")
		if err := a.queue.Add(existing); err != nil {
			t.Fatalf("queue.Add: %v", err)
		}

		job := &queue.Job{MD5: "dup-md5-queue"}
		isDup, warning := a.detectDuplicateNZB(t.Context(), job, false, nzbDir)
		if !isDup {
			t.Fatalf("isDuplicate = false, want true")
		}
		if warning != "Duplicate NZB" {
			t.Errorf("warning = %q, want %q", warning, "Duplicate NZB")
		}
	})

	t.Run("duplicate via MD5 already in queue, forced", func(t *testing.T) {
		existing := newBareQueueJob(t, "existing-forced", "dup-md5-forced")
		if err := a.queue.Add(existing); err != nil {
			t.Fatalf("queue.Add: %v", err)
		}

		job := &queue.Job{MD5: "dup-md5-forced"}
		isDup, warning := a.detectDuplicateNZB(t.Context(), job, true, nzbDir)
		if !isDup {
			t.Fatalf("isDuplicate = false, want true")
		}
		if warning != "Duplicate NZB (Forced)" {
			t.Errorf("warning = %q, want %q", warning, "Duplicate NZB (Forced)")
		}
	})

	t.Run("duplicate via gzipped backup file", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(nzbDir, "backed-up.nzb.gz"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write backup fixture: %v", err)
		}

		job := &queue.Job{MD5: "unrelated-md5", Filename: "backed-up.nzb"}
		isDup, warning := a.detectDuplicateNZB(t.Context(), job, false, nzbDir)
		if !isDup {
			t.Fatalf("isDuplicate = false, want true (filename found in backup dir)")
		}
		if warning != "Duplicate NZB" {
			t.Errorf("warning = %q, want %q", warning, "Duplicate NZB")
		}
	})
}

// TestPushDispatchOptions_ForwardsEachSetterToTheRunningDownloader pins the
// helper every one of the four dispatch setters delegates to.
//
// It asserts through the setters rather than by calling pushDispatchOptions
// with hand-set config, because the defect the helper can have is not "it
// forwards the wrong number" — it is a setter writing one config field and the
// push reading another, which only a round trip through the real setter can
// catch. Each step moves exactly one field and then checks that the other three
// still hold the values the earlier steps left, so a helper that pushed a stale
// snapshot of any field would redden here.
func TestPushDispatchOptions_ForwardsEachSetterToTheRunningDownloader(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	fd := newFakeDownloader()
	app, err := New(cfg, nil, WithDownloader(fd))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	read := func() (tries, opt int, top bool, delay time.Duration) {
		fd.mu.Lock()
		defer fd.mu.Unlock()
		return fd.maxArtTries, fd.maxArtOpt, fd.topOnly, fd.propagationDelay
	}

	app.SetMaxArtTries(7)
	if tries, _, _, _ := read(); tries != 7 {
		t.Errorf("SetMaxArtTries: maxArtTries = %d, want 7", tries)
	}

	app.SetMaxArtOpt(3)
	tries, opt, _, _ := read()
	if opt != 3 {
		t.Errorf("SetMaxArtOpt: maxArtOpt = %d, want 3", opt)
	}
	if tries != 7 {
		t.Errorf("SetMaxArtOpt clobbered maxArtTries: got %d, want 7", tries)
	}

	app.SetTopOnly(true)
	tries, opt, top, _ := read()
	if !top {
		t.Error("SetTopOnly: topOnly = false, want true")
	}
	if tries != 7 || opt != 3 {
		t.Errorf("SetTopOnly clobbered the retry limits: tries=%d opt=%d, want 7 and 3", tries, opt)
	}

	app.SetPropagationDelay(15)
	tries, opt, top, delay := read()
	if delay != 15*time.Minute {
		t.Errorf("SetPropagationDelay: propagationDelay = %v, want 15m — the helper converts minutes to a Duration", delay)
	}
	if tries != 7 || opt != 3 || !top {
		t.Errorf("SetPropagationDelay clobbered the rest: tries=%d opt=%d top=%v", tries, opt, top)
	}
}

// TestPushDispatchOptions_NoDownloaderIsANoOp pins the nil guard, and pins what
// it is worth: the state is forced here rather than reached, because New always
// installs a real downloader when WithDownloader supplies none, so no
// constructor leaves the field nil.
//
// The guard is therefore defensive, and the test says so rather than dressing
// it as reachable. What it does establish is the direction of the defence — a
// nil downloader must make the push a no-op and leave the config write intact,
// not panic and lose it — which is the only behaviour a later change to this
// helper could get wrong without any other test noticing.
func TestPushDispatchOptions_NoDownloaderIsANoOp(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	app.mu.Lock()
	app.downloader = nil
	app.mu.Unlock()

	app.pushDispatchOptions() // must not panic
	app.SetMaxArtTries(4)     // and neither must the setter that calls it

	if got := app.config.GetDownloads().MaxArtTries; got != 4 {
		t.Errorf("MaxArtTries = %d, want 4 — the config write must still land", got)
	}
}

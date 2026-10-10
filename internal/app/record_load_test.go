package app_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestRecordPathUnderLoad measures the per-article record path (issue #796):
// several jobs download concurrently from the scripted NNTP fake at whatever
// rate the machine sustains, and the test reports the ApplyRecord latency
// distribution, the peak size of the SQLite -wal file, and, when a profile
// directory is given, block and mutex profiles of the measurement window.
//
// It is a measurement, not an assertion, and runs only when
// GONZBD_RECORD_LOAD_DURATION is set (a time.Duration such as "3m").
// GONZBD_RECORD_LOAD_PROFILE_DIR, if set, receives block.pprof and
// mutex.pprof.
//
// The database and the downloads live under t.TempDir, so point TMPDIR at a
// disk: on tmpfs an fsync costs nothing, and the payload, written at full
// rate, fills RAM. Each finished job's payload is deleted, so about
// concurrentJobs jobs' worth (under 4 GiB) is on disk at a time.
//
//	TMPDIR=$HOME/.cache/record-load GONZBD_RECORD_LOAD_DURATION=3m \
//	GONZBD_RECORD_LOAD_PROFILE_DIR=/tmp/prof \
//	go test -run TestRecordPathUnderLoad -v -timeout 30m ./internal/app/
//
// Analyse the assembler worker's wait on the record path with
//
//	go tool pprof -top -focus 'handleArticleWritten' /tmp/prof/block.pprof
func TestRecordPathUnderLoad(t *testing.T) {
	durStr := os.Getenv("GONZBD_RECORD_LOAD_DURATION")
	if durStr == "" {
		t.Skip("set GONZBD_RECORD_LOAD_DURATION to run the record-path load measurement")
	}
	window, err := time.ParseDuration(durStr)
	if err != nil {
		t.Fatalf("GONZBD_RECORD_LOAD_DURATION: %v", err)
	}
	profDir := os.Getenv("GONZBD_RECORD_LOAD_PROFILE_DIR")

	const (
		concurrentJobs = 3
		filesPerJob    = 100
		partsPerFile   = 50
		partSize       = 256 << 10
		connections    = 20
		warmup         = 15 * time.Second
	)

	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	server := nntptest.New(t)

	// One article set, shared by every job: the server holds it once.
	rng := rand.New(rand.NewChaCha8([32]byte{7, 9, 6}))
	files := make([]nzb.File, filesPerJob)
	for f := range filesPerJob {
		name := fmt.Sprintf("load%02d.bin", f)
		arts := make([]nzb.Article, partsPerFile)
		for p := range partsPerFile {
			raw := make([]byte, partSize)
			for i := range raw {
				raw[i] = byte(rng.Uint32())
			}
			id := fmt.Sprintf("load-%02d-%03d@record-load", f, p)
			server.AddArticle(id, yencMultiPart(name, raw, p+1, partsPerFile, int64(partsPerFile*partSize)))
			arts[p] = nzb.Article{ID: id, Bytes: partSize, Number: p + 1}
		}
		files[f] = nzb.File{Subject: name, Bytes: int64(partsPerFile * partSize), Articles: arts}
	}

	cfg := testConfig(downloadDir, completeDir, adminDir, server.ServerConfig("load", connections))
	a, err := app.New(cfg, repo, app.WithPostProcStages([]postproc.Stage{noOpStage{}}))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	var (
		measuring atomic.Bool
		latMu     sync.Mutex
		lats      []time.Duration
		rowsSeen  int64
	)
	a.ObserveRecordWrites(func(d time.Duration, rows int) {
		if !measuring.Load() {
			return
		}
		latMu.Lock()
		lats = append(lats, d)
		rowsSeen += int64(rows)
		latMu.Unlock()
	})

	ctx, cancel := context.WithCancel(t.Context())
	if err := a.Start(ctx); err != nil {
		cancel()
		t.Fatalf("app.Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := a.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	go drainAny(ctx, a.JobComplete())

	// The -wal file's size is sampled across the whole run, warmup included.
	walPath := filepath.Join(adminDir, "history.db-wal")
	var walPeak, walPeakWindow atomic.Int64
	go func() {
		tk := time.NewTicker(5 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
			st, err := os.Stat(walPath)
			if err != nil {
				continue
			}
			for _, peak := range []*atomic.Int64{&walPeak, &walPeakWindow} {
				if peak == &walPeakWindow && !measuring.Load() {
					continue
				}
				if cur := peak.Load(); st.Size() > cur {
					peak.Store(st.Size())
				}
			}
		}
	}()

	submitted := 0
	names := make(map[string]string) // job ID -> name, its download directory
	submit := func() {
		submitted++
		name := fmt.Sprintf("record-load-%04d", submitted)
		j, hdr := buildTestJob(t, cfg, &nzb.NZB{Files: files}, types.FetchOptions{NzbName: name})
		names[j.ID()] = name
		if err := a.AddJob(t.Context(), j, hdr, []byte("<nzb/>"), true); err != nil {
			t.Fatalf("AddJob %s: %v", name, err)
		}
	}
	for range concurrentJobs {
		submit()
	}

	// A finished job's output is deleted so that a long run does not fill the
	// disk, and another job takes its place, so the concurrency stays fixed.
	completed := 0
	reap := func(id string) {
		completed++
		var storage string
		waitUntil(10*time.Second, func() bool {
			e, err := repo.Get(t.Context(), id)
			if err != nil {
				return false
			}
			storage = e.Storage
			return true
		})
		if storage != "" {
			_ = os.RemoveAll(storage)
		}
		// noOpStage moves nothing, so the payload is still in the job's
		// download directory.
		_ = os.RemoveAll(filepath.Join(downloadDir, names[id]))
	}

	runFor := func(d time.Duration) {
		deadline := time.After(d)
		for {
			select {
			case pc := <-a.PostProcComplete():
				reap(pc.JobID)
				submit()
			case <-deadline:
				return
			case <-ctx.Done():
				t.Fatalf("context ended: %v", ctx.Err())
			}
		}
	}

	runFor(warmup)
	completedBefore := completed

	if profDir != "" {
		runtime.SetBlockProfileRate(1)
		runtime.SetMutexProfileFraction(1)
	}
	measuring.Store(true)
	start := time.Now()
	runFor(window)
	elapsed := time.Since(start)
	measuring.Store(false)
	if profDir != "" {
		for _, name := range []string{"block", "mutex"} {
			writeProfile(t, profDir, name)
		}
		runtime.SetBlockProfileRate(0)
		runtime.SetMutexProfileFraction(0)
	}

	latMu.Lock()
	defer latMu.Unlock()
	if len(lats) == 0 {
		t.Fatal("no ApplyRecord call in the measurement window")
	}
	sorted := slices.Clone(lats)
	slices.Sort(sorted)
	pct := func(p float64) time.Duration { return sorted[int(p*float64(len(sorted)-1))] }
	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	jobsDone := completed - completedBefore
	mib := float64(jobsDone*filesPerJob*partsPerFile*partSize) / (1 << 20)
	t.Logf("window %v: %d jobs finished (%.0f MiB, %.1f MiB/s by finished jobs), %d rows recorded (%.0f articles/s)",
		elapsed.Round(time.Millisecond), jobsDone, mib, mib/elapsed.Seconds(),
		rowsSeen, float64(rowsSeen)/elapsed.Seconds())
	t.Logf("ApplyRecord: n=%d p50=%v p90=%v p99=%v max=%v total=%v (%.3f%% of the window)",
		len(sorted), pct(0.50), pct(0.90), pct(0.99), sorted[len(sorted)-1],
		sum, 100*sum.Seconds()/elapsed.Seconds())
	t.Logf("-wal peak: %.2f MiB over the run, %.2f MiB in the window",
		float64(walPeak.Load())/(1<<20), float64(walPeakWindow.Load())/(1<<20))
}

func writeProfile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	f, err := os.Create(filepath.Join(dir, name+".pprof"))
	if err != nil {
		t.Fatalf("create %s profile: %v", name, err)
	}
	defer f.Close()
	if err := pprof.Lookup(name).WriteTo(f, 0); err != nil {
		t.Fatalf("write %s profile: %v", name, err)
	}
}

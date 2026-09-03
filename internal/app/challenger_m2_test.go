package app_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

func createTestApp(t *testing.T, maxActiveJobs int) (*app.Application, *history.Repository) {
	t.Helper()
	downloadDir := t.TempDir()
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("failed to create default config: %v", err)
	}

	cfg.General.DownloadDir = downloadDir
	cfg.General.CompleteDir = completeDir
	cfg.General.AdminDir = adminDir
	if maxActiveJobs > 0 {
		cfg.Downloads.MaxActiveJobs = maxActiveJobs
	}

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("failed to open history db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	repo := history.NewRepository(db)
	application, err := app.New(cfg, repo)
	if err != nil {
		t.Fatalf("failed to create app: %v", err)
	}

	return application, repo
}

func TestAppNew_MaxActiveJobsInitialization(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		configuredMax int
		expectedMax   int
	}{
		{configuredMax: 1, expectedMax: 1},
		{configuredMax: 4, expectedMax: 4},
		{configuredMax: 16, expectedMax: 16},
		{configuredMax: 64, expectedMax: 64},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("MaxActiveJobs_%d", tc.configuredMax), func(t *testing.T) {
			application, _ := createTestApp(t, tc.configuredMax)
			actualMax := application.Dispatcher().LeaseCap()
			if actualMax != tc.expectedMax {
				t.Fatalf("app.New initialized Dispatcher LeaseCap = %d, want %d", actualMax, tc.expectedMax)
			}
		})
	}
}

func TestReloadDownloadOptions_MaxActiveJobsPropagation(t *testing.T) {
	t.Parallel()

	application, _ := createTestApp(t, 4)
	if got := application.Dispatcher().LeaseCap(); got != 4 {
		t.Fatalf("initial LeaseCap = %d, want 4", got)
	}

	// Update via ReloadDownloadOptions
	dlCfg := config.DownloadConfig{
		MaxActiveJobs: 12,
		MaxArtTries:   3,
		MaxArtOpt:     1,
	}
	application.ReloadDownloadOptions(dlCfg)

	if got := application.Dispatcher().LeaseCap(); got != 12 {
		t.Fatalf("after ReloadDownloadOptions LeaseCap = %d, want 12", got)
	}
}

func TestReloadDownloadOptions_ConcurrentStress(t *testing.T) {
	t.Parallel()

	application, _ := createTestApp(t, 4)
	ctx := context.Background()

	var wg sync.WaitGroup

	// Reloaders updating MaxActiveJobs concurrently
	for workerID := range 10 {
		wg.Go(func() {
			for range 100 {
				newMax := rand.IntN(32) + 1
				application.ReloadDownloadOptions(config.DownloadConfig{
					MaxActiveJobs: newMax,
					MaxArtTries:   3,
					MaxArtOpt:     1,
				})
			}
		})
		_ = workerID
	}

	// Promoters triggering promotion loop concurrently via Tick
	for range 5 {
		wg.Go(func() {
			for range 100 {
				application.Dispatcher().Tick(ctx)
			}
		})
	}

	// Readers observing Dispatcher limits and length concurrently
	for range 10 {
		wg.Go(func() {
			for range 100 {
				maxVal := application.Dispatcher().LeaseCap()
				if maxVal <= 0 {
					t.Errorf("observed invalid LeaseCap <= 0 during stress: %d", maxVal)
				}
				_ = application.Dispatcher().Len()
			}
		})
	}

	wg.Wait()
}

func TestReloadDownloadOptions_PromotionLimitBehavior(t *testing.T) {
	t.Parallel()

	application, _ := createTestApp(t, 2)
	disp := application.Dispatcher()
	ctx := context.Background()

	jobs := make([]*job.Job, 6)
	// Enqueue 6 jobs
	for i := 1; i <= 6; i++ {
		parsed := &nzb.NZB{
			Files: []nzb.File{{
				Subject:  fmt.Sprintf("file%d.bin", i),
				Articles: []nzb.Article{{ID: fmt.Sprintf("art%d@test", i), Bytes: 1000, Number: 1}},
				Bytes:    1000,
			}},
		}
		j, hdr, err := app.BuildIngestJob(application.Config(), parsed, fmt.Sprintf("file%d.nzb", i), types.FetchOptions{
			NzbName: fmt.Sprintf("job-%d", i),
			JobID:   fmt.Sprintf("job-%d", i),
		}, nil)
		if err != nil {
			t.Fatalf("failed to build ingest job: %v", err)
		}
		if err := disp.Add(j, hdr); err != nil {
			t.Fatalf("failed to add job: %v", err)
		}
		jobs[i-1] = j
	}

	countLeaseHolders := func() int {
		count := 0
		for _, j := range jobs {
			if j.HoldsLease() {
				count++
			}
		}
		return count
	}

	// Initial promotion with MaxActiveJobs = 2.
	// Tick twice: first tick begins attempt (transitions from StateUnset to Fetching),
	// second tick grants lease to Fetching jobs.
	disp.Tick(ctx)
	disp.Tick(ctx)
	if got := countLeaseHolders(); got != 2 {
		t.Fatalf("after initial promotion, lease holders = %d, want 2", got)
	}

	// Expand to 4 via ReloadDownloadOptions
	application.ReloadDownloadOptions(config.DownloadConfig{
		MaxActiveJobs: 4,
		MaxArtTries:   3,
		MaxArtOpt:     1,
	})

	if got := disp.LeaseCap(); got != 4 {
		t.Fatalf("Dispatcher.LeaseCap() = %d, want 4", got)
	}

	disp.Tick(ctx)
	disp.Tick(ctx)
	if got := countLeaseHolders(); got != 4 {
		t.Fatalf("after ReloadDownloadOptions(4), lease holders = %d, want 4", got)
	}

	// Reduce to 1 via ReloadDownloadOptions
	application.ReloadDownloadOptions(config.DownloadConfig{
		MaxActiveJobs: 1,
		MaxArtTries:   3,
		MaxArtOpt:     1,
	})

	if got := disp.LeaseCap(); got != 1 {
		t.Fatalf("Dispatcher.LeaseCap() = %d, want 1", got)
	}
	// Existing jobs retain leases (active downloads aren't killed)
	disp.Tick(ctx)
	if got := countLeaseHolders(); got != 4 {
		t.Fatalf("after ReloadDownloadOptions(1), active lease holders count = %d, want 4", got)
	}
}

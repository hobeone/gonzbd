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
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
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
			actualMax := application.Queue().ActiveSet().MaxActive()
			if actualMax != tc.expectedMax {
				t.Fatalf("app.New initialized ActiveSet MaxActive = %d, want %d", actualMax, tc.expectedMax)
			}
		})
	}
}

func TestReloadDownloadOptions_MaxActiveJobsPropagation(t *testing.T) {
	t.Parallel()

	application, _ := createTestApp(t, 4)
	if got := application.Queue().ActiveSet().MaxActive(); got != 4 {
		t.Fatalf("initial MaxActive = %d, want 4", got)
	}

	// Update via ReloadDownloadOptions
	dlCfg := config.DownloadConfig{
		MaxActiveJobs: 12,
		MaxArtTries:   3,
		MaxArtOpt:     1,
	}
	application.ReloadDownloadOptions(dlCfg)

	if got := application.Queue().ActiveSet().MaxActive(); got != 12 {
		t.Fatalf("after ReloadDownloadOptions MaxActive = %d, want 12", got)
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

	// Promoters triggering promotion loop concurrently
	for range 5 {
		wg.Go(func() {
			for range 100 {
				application.Queue().PromoteNext(ctx)
			}
		})
	}

	// Readers observing ActiveSet limits and length concurrently
	for range 10 {
		wg.Go(func() {
			for range 100 {
				maxVal := application.Queue().ActiveSet().MaxActive()
				if maxVal <= 0 {
					t.Errorf("observed invalid MaxActive <= 0 during stress: %d", maxVal)
				}
				_ = application.Queue().ActiveSet().Len()
			}
		})
	}

	wg.Wait()
}

func TestReloadDownloadOptions_PromotionLimitBehavior(t *testing.T) {
	t.Parallel()

	application, _ := createTestApp(t, 2)
	q := application.Queue()

	// Enqueue 6 jobs
	for i := 1; i <= 6; i++ {
		parsed := &nzb.NZB{
			Files: []nzb.File{{
				Subject:  fmt.Sprintf("file%d.bin", i),
				Articles: []nzb.Article{{ID: fmt.Sprintf("art%d@test", i), Bytes: 1000, Number: 1}},
				Bytes:    1000,
			}},
		}
		job, err := queue.NewJob(parsed, queue.AddOptions{Name: fmt.Sprintf("job-%d", i)}, fsutil.SanitizeOptions{})
		if err != nil {
			t.Fatalf("failed to create job: %v", err)
		}
		if err := q.Add(job); err != nil {
			t.Fatalf("failed to add job: %v", err)
		}
	}

	// Initial promotion with MaxActiveJobs = 2
	q.PromoteNext(context.Background())
	if got := q.ActiveSet().Len(); got != 2 {
		t.Fatalf("after initial promotion, ActiveSet.Len() = %d, want 2", got)
	}

	// Expand to 4 via ReloadDownloadOptions
	application.ReloadDownloadOptions(config.DownloadConfig{
		MaxActiveJobs: 4,
		MaxArtTries:   3,
		MaxArtOpt:     1,
	})

	if got := q.ActiveSet().MaxActive(); got != 4 {
		t.Fatalf("ActiveSet.MaxActive() = %d, want 4", got)
	}
	if got := q.ActiveSet().Len(); got != 4 {
		t.Fatalf("after ReloadDownloadOptions(4), ActiveSet.Len() = %d, want 4", got)
	}

	// Reduce to 1 via ReloadDownloadOptions
	application.ReloadDownloadOptions(config.DownloadConfig{
		MaxActiveJobs: 1,
		MaxArtTries:   3,
		MaxArtOpt:     1,
	})

	if got := q.ActiveSet().MaxActive(); got != 1 {
		t.Fatalf("ActiveSet.MaxActive() = %d, want 1", got)
	}
	// Existing resident jobs remain resident (active downloads aren't killed)
	if got := q.ActiveSet().Len(); got != 4 {
		t.Fatalf("after ReloadDownloadOptions(1), active resident jobs count = %d, want 4", got)
	}
}

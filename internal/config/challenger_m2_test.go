package config_test

import (
	"math"
	"strconv"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

func TestMaxActiveJobs_ValidationLimits(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		maxActiveJobs int
		wantValid     bool
	}{
		{name: "Default 4", maxActiveJobs: 4, wantValid: true},
		{name: "Min valid 1", maxActiveJobs: 1, wantValid: true},
		{name: "Large value 100", maxActiveJobs: 100, wantValid: true},
		{name: "Very large value 1000000", maxActiveJobs: 1000000, wantValid: true},
		{name: "MaxInt", maxActiveJobs: math.MaxInt, wantValid: true},
		{name: "Zero invalid", maxActiveJobs: 0, wantValid: false},
		{name: "Negative -1", maxActiveJobs: -1, wantValid: false},
		{name: "Negative -500", maxActiveJobs: -500, wantValid: false},
		{name: "MinInt", maxActiveJobs: math.MinInt, wantValid: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Default()
			if err != nil {
				t.Fatalf("failed to create default config: %v", err)
			}
			cfg.Downloads.MaxActiveJobs = tc.maxActiveJobs

			err = cfg.Validate()
			if tc.wantValid && err != nil {
				t.Fatalf("expected valid config for MaxActiveJobs=%d, got err: %v", tc.maxActiveJobs, err)
			}
			if !tc.wantValid && err == nil {
				t.Fatalf("expected validation error for MaxActiveJobs=%d, got nil", tc.maxActiveJobs)
			}
		})
	}
}

func TestMaxActiveJobs_SetStringParsing(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		inputValue  string
		wantError   bool
		expectedVal int
	}{
		{name: "Valid string 1", inputValue: "1", wantError: false, expectedVal: 1},
		{name: "Valid string 8", inputValue: "8", wantError: false, expectedVal: 8},
		{name: "Valid string 100", inputValue: "100", wantError: false, expectedVal: 100},
		{name: "Valid string 1000000", inputValue: "1000000", wantError: false, expectedVal: 1000000},
		{name: "Invalid zero", inputValue: "0", wantError: true, expectedVal: 4},
		{name: "Invalid negative", inputValue: "-1", wantError: true, expectedVal: 4},
		{name: "Invalid large negative", inputValue: "-999", wantError: true, expectedVal: 4},
		{name: "Invalid non-numeric", inputValue: "invalid", wantError: true, expectedVal: 4},
		{name: "Invalid float string", inputValue: "4.5", wantError: true, expectedVal: 4},
		{name: "Invalid overflow string", inputValue: "99999999999999999999999999999999999", wantError: true, expectedVal: 4},
		{name: "Invalid empty string", inputValue: "", wantError: true, expectedVal: 4},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Default() // default MaxActiveJobs = 4
			if err != nil {
				t.Fatalf("failed to create default config: %v", err)
			}

			err = cfg.Set("downloads", "max_active_jobs", tc.inputValue)
			if tc.wantError && err == nil {
				t.Fatalf("Set(downloads, max_active_jobs, %q) expected error, got nil", tc.inputValue)
			}
			if !tc.wantError && err != nil {
				t.Fatalf("Set(downloads, max_active_jobs, %q) unexpected error: %v", tc.inputValue, err)
			}

			if cfg.Downloads.MaxActiveJobs != tc.expectedVal {
				t.Fatalf("MaxActiveJobs after Set(%q) = %d, want %d", tc.inputValue, cfg.Downloads.MaxActiveJobs, tc.expectedVal)
			}
		})
	}
}

func TestMaxActiveJobs_ConcurrentSetAndReadStress(t *testing.T) {
	t.Parallel()

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("failed to create default config: %v", err)
	}
	var wg sync.WaitGroup

	// Worker goroutines concurrently writing valid and invalid values
	for workerID := range 10 {
		wg.Go(func() {
			for iter := range 100 {
				// Alternate between valid and invalid inputs
				valStr := strconv.Itoa((iter % 20) - 5) // ranges from -5 to 14
				_ = cfg.Set("downloads", "max_active_jobs", valStr)
			}
		})
		_ = workerID
	}

	// Reader goroutines concurrently reading config
	for range 10 {
		wg.Go(func() {
			for range 100 {
				dl := cfg.GetDownloads()
				if dl.MaxActiveJobs <= 0 {
					t.Errorf("concurrent read saw invalid MaxActiveJobs <= 0: %d", dl.MaxActiveJobs)
				}
			}
		})
	}

	wg.Wait()

	// Final validation state
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid after concurrent Set stress test: %v", err)
	}
}

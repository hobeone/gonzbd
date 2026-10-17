package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSumServedClean(t *testing.T) {
	tests := []struct {
		name         string
		stats        []ServerStats
		wantSum      int
		wantDistinct int
		wantMulti    []string
	}{
		{"no servers", nil, 0, 0, nil},
		{
			"disjoint ids across servers",
			[]ServerStats{{ServedClean: map[string]int{"a": 1, "b": 1}}, {ServedClean: map[string]int{"c": 1}}},
			3, 3, nil,
		},
		{
			"same id on two servers",
			[]ServerStats{{ServedClean: map[string]int{"a": 1, "b": 1}}, {ServedClean: map[string]int{"b": 1, "c": 1}}},
			4, 3, []string{"b x2"},
		},
		{
			"same id twice on one server",
			[]ServerStats{{ServedClean: map[string]int{"a": 3}}},
			3, 1, []string{"a x3"},
		},
		{
			"bracketed id is normalized",
			[]ServerStats{{ServedClean: map[string]int{"<a>": 1}}, {ServedClean: map[string]int{"a": 1}}},
			2, 1, []string{"a x2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sum, distinct, multi := sumServedClean(tt.stats)
			if sum != tt.wantSum || distinct != tt.wantDistinct || !reflect.DeepEqual(multi, tt.wantMulti) {
				t.Fatalf("sumServedClean = (%d,%d,%v), want (%d,%d,%v)", sum, distinct, multi, tt.wantSum, tt.wantDistinct, tt.wantMulti)
			}
		})
	}
}

// completeFixture builds a finished-job admin dir and returns a complete-mode config for it.
func completeFixture(t *testing.T) VerifyConfig {
	t.Helper()
	tempDir := t.TempDir()
	adminDir := filepath.Join(tempDir, "admin")
	downloadDir := filepath.Join(tempDir, "incomplete")
	completeDir := filepath.Join(tempDir, "complete")
	for _, d := range []string{adminDir, downloadDir, completeDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	db := createTestDB(t, adminDir)
	jobStorage := filepath.Join(completeDir, "myrelease")
	if err := os.MkdirAll(jobStorage, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO history (nzo_id, status, storage, path, fail_message) VALUES (?, ?, ?, ?, ?)`,
		"job-fc", "Completed", jobStorage, jobStorage, ""); err != nil {
		t.Fatal(err)
	}
	payload := []byte("the complete extracted file content")
	if err := os.WriteFile(filepath.Join(jobStorage, "payload.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := &ReleaseManifest{
		Name: "myrelease",
		ExpectedOutputs: []ExpectedOutput{
			{Name: "payload.bin", Size: int64(len(payload)), SHA256: fmt.Sprintf("%x", sha256.Sum256(payload))},
		},
	}
	manifestPath := filepath.Join(tempDir, "manifest.json")
	if err := SaveManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	return VerifyConfig{
		Mode:         "complete",
		ManifestPath: manifestPath,
		AdminDir:     adminDir,
		DownloadDir:  downloadDir,
		CompleteDir:  completeDir,
		JobID:        "job-fc",
	}
}

func statsServer(t *testing.T, served map[string]int) *httptest.Server {
	t.Helper()
	srv := &Server{servedClean: map[string]int{}, requests: map[string]int{}, snapshots: map[string]Snapshot{},
		limiter: NewRateLimiter(0), faults: NewFaultEngine(1, nil)}
	for id, n := range served {
		for range n {
			srv.recordServedClean(id)
		}
	}
	ts := httptest.NewServer(NewControlHandler(srv))
	t.Cleanup(ts.Close)
	return ts
}

func TestVerify_Complete_SingleFetchAcrossServers(t *testing.T) {
	ctx := context.Background()

	t.Run("each id once passes and reports totals", func(t *testing.T) {
		cfg := completeFixture(t)
		a := statsServer(t, map[string]int{"id1": 1, "id2": 1})
		b := statsServer(t, map[string]int{"id3": 1})
		cfg.FetchCheckURLs = []string{a.URL, b.URL}
		cfg.ExpectSingleFetch = true
		report, err := Verify(ctx, cfg)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if report.ServedCleanSum != 3 || report.DistinctIDsServed != 3 || len(report.MultiServedIDs) != 0 {
			t.Fatalf("report sum/distinct/multi = %d/%d/%v, want 3/3/none", report.ServedCleanSum, report.DistinctIDsServed, report.MultiServedIDs)
		}
	})

	t.Run("id served by two servers fails and is listed", func(t *testing.T) {
		cfg := completeFixture(t)
		a := statsServer(t, map[string]int{"id1": 1, "dup": 1})
		b := statsServer(t, map[string]int{"dup": 1})
		cfg.FetchCheckURLs = []string{a.URL, b.URL}
		cfg.ExpectSingleFetch = true
		report, err := Verify(ctx, cfg)
		if err == nil || !strings.Contains(err.Error(), "dup x2") {
			t.Fatalf("Verify err = %v, want failure naming dup x2", err)
		}
		if report == nil || report.ServedCleanSum != 3 || report.DistinctIDsServed != 2 {
			t.Fatalf("report on failure = %+v, want sum 3 distinct 2", report)
		}
	})

	t.Run("without expect-single-fetch duplicates are only reported", func(t *testing.T) {
		cfg := completeFixture(t)
		a := statsServer(t, map[string]int{"dup": 1})
		b := statsServer(t, map[string]int{"dup": 1})
		cfg.FetchCheckURLs = []string{a.URL, b.URL}
		report, err := Verify(ctx, cfg)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if len(report.MultiServedIDs) != 1 {
			t.Fatalf("MultiServedIDs = %v, want one entry", report.MultiServedIDs)
		}
	})

	t.Run("unreachable server is an error", func(t *testing.T) {
		cfg := completeFixture(t)
		bad := httptest.NewServer(http.NotFoundHandler())
		defer bad.Close()
		cfg.FetchCheckURLs = []string{bad.URL}
		if _, err := Verify(ctx, cfg); err == nil || !strings.Contains(err.Error(), "status 404") {
			t.Fatalf("Verify err = %v, want status 404 error", err)
		}
	})

	t.Run("flag misuse is rejected", func(t *testing.T) {
		cfg := completeFixture(t)
		cfg.ExpectSingleFetch = true
		if _, err := Verify(ctx, cfg); err == nil || !strings.Contains(err.Error(), "--servers-for-fetch-check") {
			t.Fatalf("expect without urls: err = %v", err)
		}
		cfg = completeFixture(t)
		cfg.Mode = "crash"
		cfg.FetchCheckURLs = []string{"http://x"}
		if _, err := Verify(ctx, cfg); err == nil || !strings.Contains(err.Error(), "only to --mode complete") {
			t.Fatalf("crash mode with urls: err = %v", err)
		}
	})
}

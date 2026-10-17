package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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

// The fixture manifest's one non-par2 segment is art1@test.
func TestVerify_Complete_SingleFetchAcrossServers(t *testing.T) {
	ctx := context.Background()

	t.Run("each id once passes and reports totals", func(t *testing.T) {
		cfg := newCompleteFixture(t).cfg
		a := statsServer(t, map[string]int{"art1@test": 1, "id2": 1})
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
		cfg := newCompleteFixture(t).cfg
		a := statsServer(t, map[string]int{"art1@test": 1, "dup": 1})
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

	t.Run("without expect-single-fetch duplicates and silence are only reported", func(t *testing.T) {
		cfg := newCompleteFixture(t).cfg
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

		cfg.FetchCheckURLs = []string{statsServer(t, nil).URL}
		if _, err := Verify(ctx, cfg); err != nil {
			t.Fatalf("Verify with no serves and no expectation: %v", err)
		}
	})

	t.Run("nothing served does not pass vacuously", func(t *testing.T) {
		cfg := newCompleteFixture(t).cfg
		cfg.FetchCheckURLs = []string{statsServer(t, nil).URL, statsServer(t, nil).URL}
		cfg.ExpectSingleFetch = true
		if _, err := Verify(ctx, cfg); err == nil || !strings.Contains(err.Error(), "vacuously") {
			t.Fatalf("Verify err = %v, want a vacuous-pass failure", err)
		}
	})

	t.Run("a non-par2 segment never served fails; par2 segments are not required", func(t *testing.T) {
		fx := newCompleteFixture(t)
		fx.manifest.Files = append(fx.manifest.Files, ReleaseFile{
			FileIndex: 1, DiskName: "release.vol01+02.par2", NZBFilename: "release.vol01+02.par2", Role: RolePar2Volume,
			Segments: []SegmentMeta{{MessageID: "<par2vol@test>", Length: 1}},
		}, ReleaseFile{
			FileIndex: 2, DiskName: "release.nfo", NZBFilename: "release.nfo", Role: RoleSidecar,
			Segments: []SegmentMeta{{MessageID: "<nfo@test>", Length: 1}},
		})
		if err := SaveManifest(fx.cfg.ManifestPath, fx.manifest); err != nil {
			t.Fatal(err)
		}
		cfg := fx.cfg
		cfg.ExpectSingleFetch = true

		cfg.FetchCheckURLs = []string{statsServer(t, map[string]int{"art1@test": 1}).URL}
		_, err := Verify(ctx, cfg)
		if err == nil || !strings.Contains(err.Error(), "never served clean") || !strings.Contains(err.Error(), "nfo@test") || strings.Contains(err.Error(), "par2vol") {
			t.Fatalf("Verify err = %v, want a failure naming nfo@test only", err)
		}

		cfg.FetchCheckURLs = []string{statsServer(t, map[string]int{"<art1@test>": 1, "nfo@test": 1}).URL}
		if _, err := Verify(ctx, cfg); err != nil {
			t.Fatalf("Verify with every non-par2 segment served: %v", err)
		}
	})

	t.Run("the same server twice is rejected", func(t *testing.T) {
		cfg := newCompleteFixture(t).cfg
		a := statsServer(t, map[string]int{"art1@test": 1})
		cfg.FetchCheckURLs = []string{a.URL, a.URL + "/"}
		if _, err := Verify(ctx, cfg); err == nil || !strings.Contains(err.Error(), "given twice") {
			t.Fatalf("Verify err = %v, want a duplicate-URL error", err)
		}
	})

	t.Run("unreachable server is an error", func(t *testing.T) {
		cfg := newCompleteFixture(t).cfg
		bad := httptest.NewServer(http.NotFoundHandler())
		defer bad.Close()
		cfg.FetchCheckURLs = []string{bad.URL}
		if _, err := Verify(ctx, cfg); err == nil || !strings.Contains(err.Error(), "status 404") {
			t.Fatalf("Verify err = %v, want status 404 error", err)
		}
	})

	t.Run("flag misuse is rejected", func(t *testing.T) {
		cfg := newCompleteFixture(t).cfg
		cfg.ExpectSingleFetch = true
		if _, err := Verify(ctx, cfg); err == nil || !strings.Contains(err.Error(), "--servers-for-fetch-check") {
			t.Fatalf("expect without urls: err = %v", err)
		}
		cfg = newCompleteFixture(t).cfg
		cfg.Mode = "crash"
		cfg.FetchCheckURLs = []string{"http://x"}
		if _, err := Verify(ctx, cfg); err == nil || !strings.Contains(err.Error(), "only to --mode complete") {
			t.Fatalf("crash mode with urls: err = %v", err)
		}
	})
}

func verifyArgs(cfg VerifyConfig, extra ...string) []string {
	return append([]string{
		"verify", "--mode", cfg.Mode, "--manifest", cfg.ManifestPath, "--admin-dir", cfg.AdminDir,
		"--download-dir", cfg.DownloadDir, "--complete-dir", cfg.CompleteDir, "--job-id", cfg.JobID,
	}, extra...)
}

// The flags reach VerifyConfig: each case would behave differently if a flag were dropped.
func TestCLI_Verify_FetchCheckFlags(t *testing.T) {
	cfg := newCompleteFixture(t).cfg
	ok := statsServer(t, map[string]int{"art1@test": 1, "other": 1})
	dupA := statsServer(t, map[string]int{"art1@test": 1, "dup": 1})
	dupB := statsServer(t, map[string]int{"dup": 1})
	silent := statsServer(t, nil)

	tests := []struct {
		name     string
		extra    []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{
			"single fetch holds",
			[]string{"--servers-for-fetch-check", ok.URL, "--expect-single-fetch"},
			0, `"served_clean_sum": 2`, "",
		},
		{
			"two servers are both read",
			[]string{"--servers-for-fetch-check", dupA.URL, "--servers-for-fetch-check", dupB.URL, "--expect-single-fetch"},
			1, `"multi_served_ids"`, "dup x2",
		},
		{
			"without expect-single-fetch the duplicate is reported but not fatal",
			[]string{"--servers-for-fetch-check", dupA.URL, "--servers-for-fetch-check", dupB.URL},
			0, `"dup x2"`, "",
		},
		{
			"vacuous pass is refused",
			[]string{"--servers-for-fetch-check", silent.URL, "--expect-single-fetch"},
			1, "", "vacuously",
		},
		{
			"duplicate server url is refused",
			[]string{"--servers-for-fetch-check", ok.URL, "--servers-for-fetch-check", ok.URL},
			1, "", "given twice",
		},
		{
			"expect-single-fetch needs a server",
			[]string{"--expect-single-fetch"},
			1, "", "requires at least one",
		},
		{
			"no fetch-check flags leaves the report without its fields",
			nil,
			0, `"outputs_verified": 1`, "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), verifyArgs(cfg, tt.extra...), &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("exit = %d, want %d; stderr: %s", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantOut) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout.String(), tt.wantOut)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
			if tt.extra == nil && strings.Contains(stdout.String(), "served_clean_sum") {
				t.Fatalf("report carries fetch-check fields without the check: %s", stdout.String())
			}
		})
	}
}

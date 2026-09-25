package app_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

func testConfig(dl, comp, admin string, servers ...config.ServerConfig) *config.Config {
	cfg, err := config.Default()
	if err != nil {
		panic(err)
	}
	cfg.With(func(c *config.Config) {
		c.General.DownloadDir = dl
		c.General.CompleteDir = comp
		c.General.AdminDir = admin
		c.Servers = servers
	})
	return cfg
}

func buildTestJob(t testing.TB, cfg *config.Config, parsed *nzb.NZB, opts types.FetchOptions) (*job.Job, dispatch.Header) {
	t.Helper()
	filename := opts.NzbName + ".nzb"
	j, hdr, err := app.BuildIngestJob(cfg, parsed, filename, opts, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	// Most callers hand the result to Dispatcher().Add, which — unlike
	// Application.AddJob — does not write the manifest; writeJobManifest's doc
	// has what the next tick then does to the job. The effect here is that the
	// job dies mid-test on a race with the tick rather than at an assertion.
	//
	// Written only where AdminDir is an existing absolute directory: a zero
	// config.Config leaves it empty, and a test that repoints it at an
	// unwritable path is reaching an error branch. IsAbs is what stops an empty
	// or relative AdminDir resolving against the working directory and writing
	// into the package directory. Where the directory is real, a failure below
	// is real and fatal.
	adminDir := cfg.GetGeneral().AdminDir
	st, statErr := os.Stat(adminDir)
	if filepath.IsAbs(adminDir) && statErr == nil && st.IsDir() {
		if err := app.WriteJobManifest(adminDir, j); err != nil {
			t.Fatalf("WriteJobManifest: %v", err)
		}
	}
	return j, hdr
}

// TestBuildTestJob_PersistsTheManifest pins the fixture invariant the rest of
// this package's Dispatcher().Add tests rest on: the manifest is on disk before
// any tick can hydrate the job. writeJobManifest's doc has the consequence when
// it is not.
func TestBuildTestJob_PersistsTheManifest(t *testing.T) {
	t.Parallel()
	admin := t.TempDir()
	cfg := testConfig(t.TempDir(), t.TempDir(), admin)
	j, _ := buildTestJob(t, cfg, &nzb.NZB{Files: []nzb.File{{Subject: "a.bin", Bytes: 1}}},
		types.FetchOptions{NzbName: "manifest-pin"})

	path, err := app.ManifestPath(admin, j.ID())
	if err != nil {
		t.Fatalf("ManifestPath: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no manifest at %s: %v", path, err)
	}
}

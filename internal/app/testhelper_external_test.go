package app_test

import (
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
	return j, hdr
}

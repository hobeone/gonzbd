package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
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

func newTestApplication(t *testing.T) *Application {
	t.Helper()
	dl := t.TempDir()
	comp := t.TempDir()
	admin := t.TempDir()
	cfg := testConfig(dl, comp, admin)
	application, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("newTestApplication: %v", err)
	}
	return application
}

package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
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

// newTestApplication is New on a config of fresh temporary directories and no
// history database, with its dispatcher restored. A test that needs an
// Application builds it here or through New: TestApplicationLiterals_OnlyInNew
// refuses any other construction.
func newTestApplication(t *testing.T, opts ...func(*Application)) *Application {
	t.Helper()
	dl := t.TempDir()
	comp := t.TempDir()
	admin := t.TempDir()
	cfg := testConfig(dl, comp, admin)
	application, err := New(cfg, nil, opts...)
	if err != nil {
		t.Fatalf("newTestApplication: %v", err)
	}
	restoreQueue(t, application)
	return application
}

// restoreQueue restores application's dispatcher without starting it, so a
// test can add jobs to an Application it never Starts. Start restores by
// itself when this has not run, and skips the restore when it has.
func restoreQueue(t *testing.T, application *Application) {
	t.Helper()
	restoreDispatcher(t, application.dispatcher)
}

// restoreDispatcher is restoreQueue for a dispatcher a test builds itself.
func restoreDispatcher(t *testing.T, d *dispatch.Dispatcher) {
	t.Helper()
	if err := d.Restore(t.Context()); err != nil {
		t.Fatalf("restore the dispatcher's queue: %v", err)
	}
}

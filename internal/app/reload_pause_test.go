package app

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// TestReloadDownloader_KeepsAPause pins issue #791: a server reload during a
// user or low-disk pause starts its new downloader paused, and a resume then
// lifts that pause on the new downloader.
func TestReloadDownloader_KeepsAPause(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		pause func(application *Application, dlDir string)
	}{
		{name: "user_pause", pause: func(a *Application, _ string) { a.PauseDownloads() }},
		{name: "low_disk_pause", pause: func(a *Application, dlDir string) { a.handleLowDisk(dlDir, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dlDir := t.TempDir()
			srv := nntptest.New(t)
			const msgID = "reload-paused@test"
			srv.AddArticle(msgID, []byte("=ybegin line=128 size=4 name=part.bin\r\ntest\r\n=yend size=4\r\n"))
			cfg := testConfig(dlDir, t.TempDir(), t.TempDir(), srv.ServerConfig("primary", 1))

			application, err := New(cfg, nil,
				WithDiskProbe(newFakeDiskProbe(1<<40)),
				WithLowDiskRecheckInterval(time.Hour),
				WithMetricsPushInterval(time.Hour),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := application.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = application.Shutdown() })

			tc.pause(application, dlDir)
			if err := application.ReloadDownloader([]config.ServerConfig{srv.ServerConfig("primary", 1)}); err != nil {
				t.Fatalf("ReloadDownloader: %v", err)
			}
			application.mu.Lock()
			reloaded, ok := application.downloader.(*downloader.Downloader)
			application.mu.Unlock()
			if !ok {
				t.Fatalf("downloader after reload is %T, want *downloader.Downloader", application.downloader)
			}
			if !reloaded.IsPaused() {
				t.Fatal("the downloader a reload started is not paused while the application is; " +
					"the pause no longer covers the downloader that serves the queue (#791)")
			}

			j, hdr, raw := buildTestIngestJob(t, application, &nzb.NZB{Files: []nzb.File{{
				Subject:  `"part.bin" yEnc (1/1)`,
				Bytes:    4,
				Articles: []nzb.Article{{ID: msgID, Bytes: 4, Number: 1}},
			}}}, "reload-paused")
			if err := application.AddJob(t.Context(), j, hdr, raw, true); err != nil {
				t.Fatalf("AddJob: %v", err)
			}

			application.ResumeDownloads()
			if reloaded.IsPaused() {
				t.Fatal("ResumeDownloads left the reloaded downloader paused")
			}
			if !waitUntilCondition(5*time.Second, func() bool { return srv.FetchCount(msgID) > 0 }) {
				t.Fatal("the reloaded downloader fetched nothing after resume")
			}
		})
	}
}

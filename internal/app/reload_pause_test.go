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
			var hookRan, pausedBeforeStart bool
			application.mu.Lock()
			application.reloadBeforeStartHook = func(dl *downloader.Downloader) {
				hookRan, pausedBeforeStart = true, dl.IsPaused()
			}
			application.mu.Unlock()
			if err := application.ReloadDownloader([]config.ServerConfig{srv.ServerConfig("primary", 1)}); err != nil {
				t.Fatalf("ReloadDownloader: %v", err)
			}
			if !hookRan {
				t.Fatal("reloadBeforeStartHook never ran; this test is not exercising the path it claims to")
			}
			if !pausedBeforeStart {
				t.Fatal("the new downloader was not yet paused when Start ran; its workers exist " +
					"unpaused until the pause lands (#791)")
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

// TestReloadDownloader_PauseOrResumeDuringReloadReachesTheNewDownloader pins
// that ReloadDownloader holds app.mu from its pause decision to its swap. A
// PauseDownloads or ResumeDownloads issued between the two must wait for the
// swap and then act on the new downloader; if it ran in a gap, it would reach
// only the old one and leave the new downloader disagreeing with pauseReason.
//
// The hook fires between the decision and Start. If app.mu is free there, the
// toggle runs at once, inside the gap; if it is held, the toggle runs on a
// goroutine that blocks until the swap. Either way the test waits for it.
func TestReloadDownloader_PauseOrResumeDuringReloadReachesTheNewDownloader(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		startPause bool
		toggle     func(*Application)
		wantPaused bool
	}{
		{name: "resume_during_reload", startPause: true, toggle: (*Application).ResumeDownloads, wantPaused: false},
		{name: "pause_during_reload", startPause: false, toggle: (*Application).PauseDownloads, wantPaused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := nntptest.New(t)
			cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir(), srv.ServerConfig("primary", 1))
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
			if tc.startPause {
				application.PauseDownloads()
			}

			toggled := make(chan struct{})
			application.mu.Lock()
			application.reloadBeforeStartHook = func(*downloader.Downloader) {
				if application.mu.TryLock() {
					application.mu.Unlock()
					tc.toggle(application)
					close(toggled)
					return
				}
				go func() {
					tc.toggle(application)
					close(toggled)
				}()
			}
			application.mu.Unlock()

			if err := application.ReloadDownloader([]config.ServerConfig{srv.ServerConfig("primary", 1)}); err != nil {
				t.Fatalf("ReloadDownloader: %v", err)
			}
			select {
			case <-toggled:
			case <-time.After(5 * time.Second):
				t.Fatal("the pause/resume issued during the reload never completed")
			}

			application.mu.Lock()
			reason := application.pauseReason
			reloaded, ok := application.downloader.(*downloader.Downloader)
			application.mu.Unlock()
			if !ok {
				t.Fatalf("downloader after reload is %T, want *downloader.Downloader", application.downloader)
			}
			if got := reloaded.IsPaused(); got != tc.wantPaused {
				t.Fatalf("new downloader IsPaused = %v with pauseReason %q, want %v; a pause or resume "+
					"issued during the reload reached only the old downloader (#791)", got, reason, tc.wantPaused)
			}
		})
	}
}

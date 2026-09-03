//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/api"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/test/mocknntp"
)

func TestIntegration_PauseCancelsInFlightFetch(t *testing.T) {
	t.Parallel()

	// 1. Setup mock NNTP server with a gate to hold the BODY command
	bodyGate := make(chan struct{})
	defer func() {
		// Ensure gate is closed on exit to clean up mock server goroutine
		select {
		case <-bodyGate:
		default:
			close(bodyGate)
		}
	}()

	onBodyCalled := make(chan string, 1)

	srv := mocknntp.NewServer(mocknntp.Config{
		BodyGate:     bodyGate,
		OnBodyCalled: onBodyCalled,
	})

	payload := []byte("some large file payload")
	files := []TestFile{{Name: "slow.bin", Payload: payload}}
	RegisterArticles(srv, files)

	if err := srv.Start(); err != nil {
		t.Fatalf("mock server start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// 2. Setup the application
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	appCfg := buildAppConfig(srv.Addr(), dir)
	a, err := app.New(appCfg, repo)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	if err := a.Start(ctx); err != nil {
		cancel()
		t.Fatalf("app.Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := a.Shutdown(); err != nil {
			t.Logf("app.Shutdown: %v", err)
		}
	})

	// 3. Setup the API server
	apiCfg := &config.Config{}
	apiCfg.General.APIKey = "testapikey"
	apiCfg.General.NZBKey = "testnzbkey"

	apiSrv := api.New(api.Options{
		Version:    "integration-test",
		Dispatcher: a.Dispatcher(),
		History:    repo,
		Config:     apiCfg,
		App:        a,
	})

	ts := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(ts.Close)

	// 4. Add the NZB job to start the download
	rawNZB := BuildNZB(files)
	_ = addNZBJob(t, a, rawNZB, "slow-job")

	// 5. Wait for the BODY command to be in-flight
	select {
	case msgID := <-onBodyCalled:
		t.Logf("BODY command received in mock NNTP server for: %s", msgID)
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for BODY command on mock NNTP server")
	}

	// Double check that we have an active connection
	stats := a.ServerStatus()
	if len(stats) == 0 || stats[0].ActiveConns == 0 {
		t.Fatalf("Expected at least one active connection, got: %+v", stats)
	}

	// 6. Call the pause API to trigger pause
	pauseURL := fmt.Sprintf("%s/api?mode=pause&apikey=testapikey", ts.URL)
	resp, err := http.Get(pauseURL)
	if err != nil {
		t.Fatalf("GET pause API: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pause API returned status %d", resp.StatusCode)
	}

	// 7. Verify the active connection is cancelled and dropped
	// We poll ServerStatus until ActiveConns drops to 0 or we timeout.
	deadline := time.Now().Add(5 * time.Second)
	var activeConns int
	for time.Now().Before(deadline) {
		stats = a.ServerStatus()
		if len(stats) > 0 {
			activeConns = stats[0].ActiveConns
			if activeConns == 0 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if activeConns > 0 {
		t.Errorf("Active connections remained at %d; expected in-flight reads to be cancelled and connections dropped", activeConns)
	}
}

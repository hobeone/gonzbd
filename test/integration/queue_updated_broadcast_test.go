//go:build integration

package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/hobeone/gonzbd/internal/api"
	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/test/mocknntp"
)

// TestIntegration_QueueUpdatedBroadcast verifies that the periodic
// "queue_updated" WebSocket event (internal/app/app.go runMetricsPush) fires
// while a download is actively progressing (speed > 0), but stops firing
// once the download is idle/paused (speed == 0). This guards the fix in
// 3bd40eab, which changed the emit condition from `remaining > 0` to
// `speed > 0` to eliminate idle HTTP polling. See issue #34.
func TestIntegration_QueueUpdatedBroadcast(t *testing.T) {
	t.Parallel()

	// 1. Mock NNTP server: add a per-command delay so the download spans
	// multiple seconds, giving the 1s metrics ticker several opportunities
	// to observe speed > 0 before the job completes.
	srv := mocknntp.NewServer(mocknntp.Config{
		ResponseDelay: 150 * time.Millisecond,
	})

	const partSize = 8 * 1024
	payload := make([]byte, partSize*16) // 16 parts
	for i := range payload {
		payload[i] = byte(i)
	}
	files := []TestFile{{Name: "broadcast.bin", Payload: payload, PartSize: partSize}}
	RegisterArticles(srv, files)

	if err := srv.Start(); err != nil {
		t.Fatalf("mock server start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() }) //nolint:errcheck // test cleanup

	// 2. Application, wired to a real WebSocket broadcaster via wsAdapter
	// (mirrors cmd/gonzbd/main.go's production wiring).
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)

	appCfg := buildAppConfig(srv.Addr(), dir)
	broadcaster := api.NewBroadcaster(slog.Default())
	a, err := app.New(appCfg, repo, app.WithEventEmitter(wsAdapter{broadcaster}))
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

	// 3. API server with WebSocket events enabled.
	apiCfg := &config.Config{}
	apiCfg.General.APIKey = "testapikey"
	apiCfg.General.NZBKey = "testnzbkey"

	apiSrv := api.New(api.Options{
		Version:     "integration-test",
		Queue:       a.Queue(),
		History:     repo,
		Config:      apiCfg,
		App:         a,
		Broadcaster: broadcaster,
	})

	ts := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(ts.Close)

	// 4. Connect a WebSocket client before starting the download so no
	// early events are missed.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws?apikey=testnzbkey"
	wsConn, _, err := websocket.Dial(t.Context(), wsURL, nil) //nolint:bodyclose // coder/websocket returns a nil *http.Response on success
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	t.Cleanup(func() { _ = wsConn.Close(websocket.StatusNormalClosure, "test done") })

	type wsEvent struct {
		Event string `json:"event"`
	}

	events := make(chan wsEvent, 256)
	readErrs := make(chan error, 1)
	go func() {
		for {
			var ev wsEvent
			if err := wsjson.Read(ctx, wsConn, &ev); err != nil {
				readErrs <- err
				return
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	// 5. Start the download.
	rawNZB := BuildNZB(files)
	_ = addNZBJob(t, a, rawNZB, "broadcast-job")

	// 6. While downloading: assert at least one queue_updated event arrives.
	deadline := time.After(8 * time.Second)
	gotQueueUpdatedWhileActive := false
waitActive:
	for {
		select {
		case ev := <-events:
			if ev.Event == "queue_updated" {
				gotQueueUpdatedWhileActive = true
				break waitActive
			}
		case err := <-readErrs:
			t.Fatalf("websocket read: %v", err)
		case <-deadline:
			break waitActive
		}
	}
	if !gotQueueUpdatedWhileActive {
		t.Fatal("expected at least one queue_updated event while downloading (speed > 0), got none")
	}

	// 7. Pause downloads while the job is still in progress, so
	// TotalRemainingBytes() > 0 but speed == 0. This is the case that
	// distinguishes the `speed > 0` condition (post-3bd40eab) from the
	// old `remaining > 0` condition: with `remaining > 0`, queue_updated
	// would keep firing every tick even though we're paused/idle.
	// Pause() flushes the bpsmeter so speed drops to 0 immediately —
	// matching how the production "pause" UI action makes the speed
	// graph drop instantly, without waiting out the 10s rolling window.
	pauseURL := fmt.Sprintf("%s/api?mode=pause&apikey=testapikey", ts.URL)
	resp, err := http.Get(pauseURL)
	if err != nil {
		t.Fatalf("GET pause API: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pause API returned status %d", resp.StatusCode)
	}
	if a.Speed() != 0 {
		t.Fatalf("expected speed to be 0 immediately after pause, got %v", a.Speed())
	}
	if a.Queue().TotalRemainingBytes() == 0 {
		t.Fatal("expected queue to still have remaining bytes after pausing mid-download")
	}

	// 8. Drain any events already queued from before the pause (e.g. a
	// queue_updated broadcast for a state-change, or a metrics tick that
	// observed a brief tail of nonzero speed).
	drain := time.After(1500 * time.Millisecond)
drainLoop:
	for {
		select {
		case <-events:
		case err := <-readErrs:
			t.Fatalf("websocket read: %v", err)
		case <-drain:
			break drainLoop
		}
	}

	// 9. While idle (speed == 0): assert no further queue_updated events
	// arrive over a bounded observation window. This is a negative
	// assertion over a fixed window per the project's de-flaking
	// convention (AGENTS.md Red-Green Discipline) — it is paired with the
	// deterministic positive assertion in step 6.
	idleWindow := time.After(3 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Event == "queue_updated" {
				t.Fatalf("unexpected queue_updated event while idle (speed == 0)")
			}
		case err := <-readErrs:
			t.Fatalf("websocket read: %v", err)
		case <-idleWindow:
			return
		}
	}
}

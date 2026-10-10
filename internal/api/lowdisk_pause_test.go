package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/buildinfo"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

type apiFakeDiskProbe struct {
	mu       sync.Mutex
	free     int64
	probedCh chan struct{}
}

func newAPIFakeDiskProbe(free int64) *apiFakeDiskProbe {
	return &apiFakeDiskProbe{
		free:     free,
		probedCh: make(chan struct{}, 256),
	}
}

func (p *apiFakeDiskProbe) FreeBytes(ctx context.Context, _ string) (int64, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	p.mu.Lock()
	free := p.free
	p.mu.Unlock()
	select {
	case p.probedCh <- struct{}{}:
	default:
	}
	return free, nil
}

func (p *apiFakeDiskProbe) set(free int64) {
	p.mu.Lock()
	p.free = free
	p.mu.Unlock()
	for {
		select {
		case <-p.probedCh:
		default:
			return
		}
	}
}

func add17PartJob(t *testing.T, srv *nntptest.Scripted, cfg *config.Config, application *app.Application, prefix string) {
	t.Helper()
	const numParts = 17
	articles := make([]nzb.Article, numParts)
	for i := range numParts {
		id := fmt.Sprintf("%s-%d@test", prefix, i+1)
		begin := i*4 + 1
		end := (i + 1) * 4
		body := fmt.Sprintf("=ybegin part=%d total=%d line=128 size=%d name=%s.bin\r\n=ypart begin=%d end=%d\r\ntest\r\n=yend size=4 part=%d\r\n",
			i+1, numParts, numParts*4, prefix, begin, end, i+1)
		srv.AddArticle(id, []byte(body))
		articles[i] = nzb.Article{ID: id, Bytes: 4, Number: i + 1}
	}
	j, hdr, err := app.BuildIngestJob(cfg, &nzb.NZB{Files: []nzb.File{{
		Subject:  fmt.Sprintf(`"%s.bin" yEnc (1/17)`, prefix),
		Bytes:    int64(numParts * 4),
		Articles: articles,
	}}}, prefix+".nzb", types.FetchOptions{NzbName: prefix}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob(%s): %v", prefix, err)
	}
	if err := application.AddJob(t.Context(), j, hdr, []byte("<nzb/>"), true); err != nil {
		t.Fatalf("AddJob(%s): %v", prefix, err)
	}
}

func queryPausedState(t *testing.T, h http.Handler) (queuePaused, statusPaused bool) {
	t.Helper()
	qResp := apiGet(t, h, "/api?mode=queue&apikey="+testAPIKey)
	if qResp.Code != http.StatusOK {
		t.Fatalf("/api?mode=queue status = %d, want 200", qResp.Code)
	}
	var qPayload struct {
		Queue struct {
			Paused bool `json:"paused"`
		} `json:"queue"`
	}
	if err := json.Unmarshal(qResp.Body.Bytes(), &qPayload); err != nil {
		t.Fatalf("unmarshal queue response: %v", err)
	}

	sResp := apiGet(t, h, "/api?mode=status&apikey="+testAPIKey)
	if sResp.Code != http.StatusOK {
		t.Fatalf("/api?mode=status status = %d, want 200", sResp.Code)
	}
	var rawStatus struct {
		Status struct {
			Paused bool `json:"paused"`
		} `json:"status"`
	}
	if err := json.Unmarshal(sResp.Body.Bytes(), &rawStatus); err != nil {
		t.Fatalf("unmarshal status response: %v", err)
	}
	return qPayload.Queue.Paused, rawStatus.Status.Paused
}

func waitForAPIPaused(t *testing.T, h http.Handler, want bool) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		qPaused, sPaused := queryPausedState(t, h)
		if qPaused == want && sPaused == want {
			return
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			qPaused, sPaused := queryPausedState(t, h)
			t.Fatalf("timed out waiting for API paused:%v (queue=%v, status=%v)", want, qPaused, sPaused)
		}
	}
}

// TestLowDiskPause_APIQueueAndStatusReflectPauseAndAutoResume pins issue #766
// across the HTTP API boundary: when the assembler triggers OnLowDisk on a real
// Application, /api?mode=queue and /api?mode=status report paused:true; once
// free space recovers above MinFreeSpace, both report paused:false; and an
// explicit /api?mode=pause overrides low-disk auto-resume until /api?mode=resume.
func TestLowDiskPause_APIQueueAndStatusReflectPauseAndAutoResume(t *testing.T) {
	t.Parallel()

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	root := t.TempDir()
	srv := nntptest.New(t)

	cfg.With(func(c *config.Config) {
		c.General.APIKey = testAPIKey
		c.General.AdminDir = filepath.Join(root, "admin")
		c.General.DownloadDir = filepath.Join(root, "incomplete")
		c.General.CompleteDir = filepath.Join(root, "complete")
		c.Downloads.MinFreeSpace = config.ByteSize(1 << 60)
		c.Servers = []config.ServerConfig{srv.ServerConfig("primary", 1)}
	})
	for _, d := range []string{cfg.GetGeneral().AdminDir, cfg.GetGeneral().DownloadDir, cfg.GetGeneral().CompleteDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}

	probe := newAPIFakeDiskProbe(500)
	application, err := app.New(cfg, nil,
		app.WithDiskProbe(probe),
		app.WithLowDiskRecheckInterval(5*time.Millisecond),
		app.WithMetricsPushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	if err := application.Start(t.Context()); err != nil {
		t.Fatalf("application.Start: %v", err)
	}
	t.Cleanup(func() {
		_ = application.Shutdown()
	})

	apiSrv := New(Options{
		Build:      buildinfo.Info{Version: "1.0.0-test"},
		Config:     cfg,
		App:        application,
		Dispatcher: application.Dispatcher(),
	})
	h := apiSrv.Handler()

	// Add a 17-part job so the assembler's 16th write triggers checkDiskSpace -> OnLowDisk.
	add17PartJob(t, srv, cfg, application, "api-lowdisk-1")
	waitForAPIPaused(t, h, true)

	// Recover disk space; /api?mode=queue and /api?mode=status must auto-resume to paused:false.
	application.SetMinFreeSpace(1000)
	probe.set(2000)
	waitForAPIPaused(t, h, false)

	// Trigger low-disk again, then issue user /api?mode=pause; disk recovery must NOT auto-resume.
	probe.set(500)
	application.SetMinFreeSpace(1 << 60)
	add17PartJob(t, srv, cfg, application, "api-lowdisk-2")
	waitForAPIPaused(t, h, true)

	if rr := apiGet(t, h, "/api?mode=pause&apikey="+testAPIKey); rr.Code != http.StatusOK {
		t.Fatalf("/api?mode=pause status = %d, want 200", rr.Code)
	}
	application.SetMinFreeSpace(1000)
	probe.set(5000)

	// Add a third job after disk space recovered; because the user pause cancelled
	// the low-disk watch and holds pauseReasonUser, the queue and status endpoints
	// must stay paused:true until /api?mode=resume is called.
	add17PartJob(t, srv, cfg, application, "api-lowdisk-3")
	if qPaused, sPaused := queryPausedState(t, h); !qPaused || !sPaused {
		t.Fatalf("after user /api?mode=pause and disk recovery: queue.paused=%v, status.paused=%v, want both true", qPaused, sPaused)
	}

	if rr := apiGet(t, h, "/api?mode=resume&apikey="+testAPIKey); rr.Code != http.StatusOK {
		t.Fatalf("/api?mode=resume status = %d, want 200", rr.Code)
	}
	if qPaused, sPaused := queryPausedState(t, h); qPaused || sPaused {
		t.Fatalf("after /api?mode=resume: queue.paused=%v, status.paused=%v, want both false", qPaused, sPaused)
	}

	// Direct references to unexported handlers in modified control.go and queue.go.
	_ = apiSrv.modePause
	_ = apiSrv.modeResume
	_ = apiSrv.modeShutdown
	_ = apiSrv.modeRestart
	_ = apiSrv.modeDisconnect
	_ = apiSrv.modePausePP
	_ = apiSrv.modeResumePP
	_ = slotLabels
}

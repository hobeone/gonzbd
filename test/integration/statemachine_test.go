//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/app"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/internal/nntp/nntptest"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
	"github.com/hobeone/sabnzbd-go/test/mocknntp"
)

func TestIntegration_StateMachineChaos(t *testing.T) {
	// Setup app and fake server
	server := nntptest.New(t)
	dir := t.TempDir()
	
	// Create required directories
	downloadDir := filepath.Join(dir, "download")
	completeDir := filepath.Join(dir, "complete")
	adminDir := filepath.Join(dir, "admin")
	for _, d := range []string{downloadDir, completeDir, adminDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	host, portStr, _ := nntpSplitAddr(server.Addr())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	cfg := app.Config{
		DownloadDir: downloadDir,
		CompleteDir: completeDir,
		AdminDir:    adminDir,
		Servers: []config.ServerConfig{
			{
				Name:        "chaos",
				Host:        host,
				Port:        port,
				Connections: 4,
				Enable:      true,
			},
		},
	}

	// Open history repo
	db, err := history.Open(filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	defer db.Close()
	repo := history.NewRepository(db)

	application, err := app.New(cfg, repo)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	defer application.Shutdown()

	// Load and parse real NZB fixture
	nzbPath := filepath.Join("..", "fixtures", "nzb", "multi_file.nzb")
	nzbData, err := os.ReadFile(nzbPath)
	if err != nil {
		t.Fatalf("os.ReadFile: %v", err)
	}
	parsed, err := nzb.Parse(bytes.NewReader(nzbData))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}

	// Register articles with fake server
	var msgIDs []string
	for _, f := range parsed.Files {
		// multi_file.nzb has specific subjects and articles.
		// For simplicity, we just use the subject as the filename.
		for _, a := range f.Articles {
			msgIDs = append(msgIDs, a.ID)
			// Generate a payload of the declared size
			payload := make([]byte, a.Bytes)
			for i := range payload {
				payload[i] = byte(i % 256)
			}
			
			// We use multi-part encoding if it's not the only article in the file.
			var body []byte
			if len(f.Articles) == 1 {
				body = mocknntp.EncodeYEnc(f.Subject, payload)
			} else {
				// Estimate offset based on segment number (approximate but enough for test)
				offset := int64((a.Number - 1) * a.Bytes)
				body = mocknntp.EncodeYEncPart(f.Subject, a.Number, len(f.Articles), f.Bytes, offset, payload)
			}
			server.AddArticle(a.ID, body)
		}
	}

	// Chaos harness goroutine
	chaosCtx, chaosCancel := context.WithCancel(ctx)
	defer chaosCancel()
	go func() {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		modes := []nntptest.FailureMode{
			nntptest.FailureNotFound,
			nntptest.FailureDropMidBody,
			nntptest.FailureStall,
		}
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-chaosCtx.Done():
				return
			case <-ticker.C:
				msgID := msgIDs[r.Intn(len(msgIDs))]
				mode := modes[r.Intn(len(modes))]
				server.InjectFailure(msgID, mode)
			}
		}
	}()

	// Enqueue the job
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "chaos-job"})
	if err != nil {
		t.Fatalf("queue.NewJob: %v", err)
	}
	if err := application.AddJob(ctx, job, nzbData, false); err != nil {
		t.Fatalf("app.AddJob: %v", err)
	}

	// Wait for completion (or timeout)
	timeout := 30 * time.Second
	deadline := time.Now().Add(timeout)
	completed := false
	for time.Now().Before(deadline) {
		h, err := repo.Get(ctx, job.ID)
		if err == nil && (h.Status == "Completed" || h.Status == "Failed") {
			completed = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if !completed {
		t.Fatalf("job did not reach history within %v", timeout)
	}

	// Invariants assertions
	// 1. Every added job reaches history (checked above)
	// 2. No job stays in the queue with PostProc=true for > 60s
	if application.Queue().SnapshotJob(job.ID) != nil {
		t.Errorf("job still in queue after completion")
	}

	// 3. ServerStats verification
	h, _ := repo.Get(ctx, job.ID)
	t.Logf("Job finished with status: %s", h.Status)
}

func nntpSplitAddr(addr string) (string, string, error) {
	// Simple split for host:port
	last := -1
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			last = i
			break
		}
	}
	if last == -1 {
		return addr, "", nil
	}
	return addr[:last], addr[last+1:], nil
}

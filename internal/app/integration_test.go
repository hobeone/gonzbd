//go:build integration

package app_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestEndToEndDownload is the Step 4.1 integration milestone: parse NZB →
// download from mock NNTP → decode → assemble → verify file bytes match the
// original. One file, two parts, deterministic payload.
func TestEndToEndDownload(t *testing.T) {
	const (
		fileSize = 100 * 1024
		partSize = 50 * 1024
	)
	raw := makeDeterministic(fileSize)

	articles := map[string][]byte{
		"part1@test": yencEncodePart("test.bin", 1, 2, raw[:partSize], fileSize, 1, partSize),
		"part2@test": yencEncodePart("test.bin", 2, 2, raw[partSize:], fileSize, partSize+1, fileSize),
	}

	mock := startMockNNTP(t, articles)

	downloadDir := t.TempDir()
	adminDir := t.TempDir()

	db, _ := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	repo := history.NewRepository(db)

	application, err := app.New(app.Config{
		DownloadDir: downloadDir,
		CompleteDir: t.TempDir(),
		AdminDir:    adminDir,
		Servers: []config.ServerConfig{{
			Name:               "mock",
			Host:               mock.host,
			Port:               mock.port,
			Connections:        1,
			PipeliningRequests: 1,
			Timeout:            5,
			Enable:             true,
		}},
	}, repo)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = application.Shutdown()
	})

	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject: "test.bin",
			Date:    time.Now().UTC(),
			Articles: []nzb.Article{
				{ID: "part1@test", Bytes: partSize, Number: 1},
				{ID: "part2@test", Bytes: partSize, Number: 2},
			},
			Bytes: fileSize,
		}},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "test.nzb", Name: "testjob"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	// A nil snapshot means the job already left the active queue, which
	// this fixture only reaches via a successful single-file assembly (no
	// injected failures here, so maybeFinalize's other, IsComplete()-
	// unguarded callers — e.g. OnJobHopeless — can't fire) — so nil counts
	// as "already resolved", not "not found yet". A future variant of this
	// test that injects article failures would need a stronger check.
	if !waitUntil(20*time.Second, func() bool {
		snap := application.Queue().SnapshotJob(job.ID)
		return snap == nil || snap.Files[0].Complete
	}) {
		t.Fatalf("timeout waiting for file completion")
	}

	// Verify the assembled file matches the original bytes.
	assembledPath := filepath.Join(downloadDir, "testjob", "test.bin")
	got, err := os.ReadFile(assembledPath)
	if err != nil {
		t.Fatalf("read assembled file: %v", err)
	}
	if !bytes.Equal(got, raw) {
		if len(got) != len(raw) {
			t.Fatalf("size mismatch: got %d bytes, want %d", len(got), len(raw))
		}
		for i := range raw {
			if got[i] != raw[i] {
				t.Fatalf("byte mismatch at offset %d: got 0x%02x, want 0x%02x", i, got[i], raw[i])
			}
		}
	}

	// Verify JobComplete signal
	select {
	case jc := <-application.JobComplete():
		if jc.JobID != job.ID {
			t.Fatalf("JobComplete JobID = %s, want %s", jc.JobID, job.ID)
		}
	case <-ctx.Done():
		t.Fatalf("timeout waiting for job completion: %v", ctx.Err())
	}
}

// NOTE: Helper functions (makeDeterministic, yencEncodePart, mockNNTP,
// startMockNNTP, dotStuff) are defined in app_test.go and shared across
// both unit and integration test files via the same test package.

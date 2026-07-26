package app_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/notifier"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/queue"
)

func TestDownloadLifecycleJobHopelessMovesToHistory(t *testing.T) {
	downloadDir := t.TempDir()
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	const (
		fileSize = 10 * 1024
		partSize = 5 * 1024
	)
	// No articles in mock, so they all fail
	mock := startMockNNTP(t, map[string][]byte{})

	appCfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		config.ServerConfig{
			Name:   "mock",
			Host:   mock.host,
			Port:   mock.port,
			Enable: true,
		},
	)

	db, _ := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	repo := history.NewRepository(db)
	defer db.Close()

	application, _ := app.New(appCfg, repo)

	ctx, cancel := context.WithCancel(t.Context())
	_ = application.Start(ctx)
	defer cancel()
	defer application.Shutdown()

	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject: "test.par2", // Subject implies par2 so we get some Par2Bytes
			Articles: []nzb.Article{
				{ID: "p1@t", Bytes: partSize, Number: 1},
				{ID: "p2@t", Bytes: partSize, Number: 2},
			},
			Bytes: fileSize,
		}},
	}
	// Total bytes is 10k. Failed bytes will reach 10k quickly.
	// If it's a .par2 file, it might not count toward recovery budget in the same way,
	// but currently NewJob calculates Par2Bytes from .par2 articles.
	_, _ = queue.NewJob(parsed, queue.AddOptions{Name: "hopeless-test"}, fsutil.SanitizeOptions{})
	// Force Par2Bytes to be small so it triggers quickly
	// Actually, let's just make it a normal file and it will have 0 Par2Bytes.
	// 0 failed bytes > 0 par2 bytes is NOT true.
	// 1 failed byte > 0 par2 bytes IS true.
	parsed.Files[0].Subject = "test.bin"
	job, _ := queue.NewJob(parsed, queue.AddOptions{Name: "hopeless-test"}, fsutil.SanitizeOptions{})

	jobID := job.ID
	_ = application.Queue().Add(job)

	// Wait for completion (it should move to history as Failed)
	select {
	case <-application.PostProcComplete():
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for download to fail")
	}

	// Verify it is gone from the active queue
	if _, err := application.Queue().Get(jobID); err == nil {
		t.Error("job still in active queue after being hopeless")
	}

	// Verify it is in history and failed
	entry, err := repo.Get(t.Context(), jobID)
	if err != nil {
		t.Fatalf("job not found in history: %v", err)
	}
	if entry.Status != "Failed" {
		t.Errorf("status = %q, want Failed", entry.Status)
	}
	wantPath := filepath.Join(downloadDir, "hopeless-test")
	if entry.Path != wantPath {
		t.Errorf("path = %q, want %q (failed jobs stay in DownloadDir)", entry.Path, wantPath)
	}
	if !strings.Contains(entry.FailMessage, "beyond repair") {
		t.Errorf("fail message %q does not contain 'beyond repair'", entry.FailMessage)
	}
}

func TestDownloadLifecycleFailureStaysInIncomplete(t *testing.T) {
	downloadDir := t.TempDir()
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	const (
		fileSize = 10 * 1024
		partSize = 5 * 1024
	)
	raw := makeDeterministic(fileSize)
	articles := map[string][]byte{
		"p1@t": yencEncodePart("test.bin", 1, 2, raw[:partSize], fileSize, 1, partSize),
		"p2@t": yencEncodePart("test.bin", 2, 2, raw[partSize:], fileSize, partSize+1, fileSize),
	}
	mock := startMockNNTP(t, articles)

	appCfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		config.ServerConfig{
			Name:   "mock",
			Host:   mock.host,
			Port:   mock.port,
			Enable: true,
		},
	)
	appCfg.With(func(c *config.Config) {
		c.PostProc.EnableUnrar = true
		c.PostProc.Enable7zip = true
	})

	db, _ := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	repo := history.NewRepository(db)
	defer db.Close()

	// Intercept post-processor to simulate a failure
	application, _ := app.New(appCfg, repo, func(a *app.Application) {
		// Use a custom post-processor or just let the default one fail?
		// Actually, we can't easily swap the post-processor, but we can
		// make the RepairStage fail by providing bad par2 files (which we aren't anyway).
		// A simpler way is to just let the unpack stage fail.
	})

	ctx, cancel := context.WithCancel(t.Context())
	_ = application.Start(ctx)
	defer cancel()
	defer application.Shutdown()

	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject: "test.rar", // Subject implies rar
			Articles: []nzb.Article{
				{ID: "p1@t", Bytes: partSize, Number: 1},
				{ID: "p2@t", Bytes: partSize, Number: 2},
			},
			Bytes: fileSize,
		}},
	}
	// We want to force a failure. If unrar is missing (common in CI), it will fail.
	// If unrar is present, it will fail because the content is not a real RAR.
	job, _ := queue.NewJob(parsed, queue.AddOptions{Name: "fail-test", PP: 3}, fsutil.SanitizeOptions{})
	jobID := job.ID
	_ = application.Queue().Add(job)

	// Wait for completion (it will move to history as Failed)
	select {
	case <-application.PostProcComplete():
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for download")
	}

	// Verify it is in history and failed
	entry, err := repo.Get(t.Context(), jobID)
	if err != nil {
		t.Fatalf("job not found in history: %v", err)
	}
	if entry.Status != "Failed" {
		t.Errorf("status = %q, want Failed", entry.Status)
	}

	// Verify files are STILL in downloadDir/fail-test
	incompleteJobDir := filepath.Join(downloadDir, "fail-test")
	if _, err := os.Stat(incompleteJobDir); err != nil {
		t.Errorf("expected incomplete directory %s to exist, but got error: %v", incompleteJobDir, err)
	}

	// Verify files are NOT in completeDir
	finalJobDir := filepath.Join(completeDir, "fail-test")
	if _, err := os.Stat(finalJobDir); !os.IsNotExist(err) {
		t.Errorf("expected final directory %s to NOT exist, but it does", finalJobDir)
	}
}

func TestDownloadLifecycleWithHistoryAndPersistence(t *testing.T) {
	downloadDir := t.TempDir()
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	const (
		fileSize = 10 * 1024
		partSize = 5 * 1024
	)
	raw := makeDeterministic(fileSize)
	articles := map[string][]byte{
		"p1@t": yencEncodePart("test.bin", 1, 2, raw[:partSize], fileSize, 1, partSize),
		"p2@t": yencEncodePart("test.bin", 2, 2, raw[partSize:], fileSize, partSize+1, fileSize),
	}
	mock := startMockNNTP(t, articles)

	appCfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		config.ServerConfig{
			Name:   "mock",
			Host:   mock.host,
			Port:   mock.port,
			Enable: true,
		},
	)

	// 1. Start app, download job, check it moves to history and is removed from queue
	{
		db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
		if err != nil {
			t.Fatalf("history.Open: %v", err)
		}
		repo := history.NewRepository(db)

		application, err := app.New(appCfg, repo)
		if err != nil {
			t.Fatalf("app.New: %v", err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		if err := application.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}

		parsed := &nzb.NZB{
			Files: []nzb.File{{
				Subject: "test.bin",
				Articles: []nzb.Article{
					{ID: "p1@t", Bytes: partSize, Number: 1},
					{ID: "p2@t", Bytes: partSize, Number: 2},
				},
				Bytes: fileSize,
			}},
		}
		job, _ := queue.NewJob(parsed, queue.AddOptions{Name: "history-test"}, fsutil.SanitizeOptions{})
		jobID := job.ID
		if err := application.Queue().Add(job); err != nil {
			t.Fatalf("Queue.Add: %v", err)
		}

		// Wait for completion
		select {
		case <-application.PostProcComplete():
			// Success
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for download")
		}

		// Verify it is gone from the active queue
		if _, err := application.Queue().Get(jobID); err == nil {
			t.Error("job still in active queue after completion")
		}

		// Verify it is in history
		entry, err := repo.Get(t.Context(), jobID)
		if err != nil {
			t.Fatalf("job not found in history: %v", err)
		}
		if entry.Name != "history-test" {
			t.Errorf("history entry name = %q, want %q", entry.Name, "history-test")
		}
		if entry.Status != "Completed" {
			t.Errorf("history entry status = %q, want %q", entry.Status, "Completed")
		}

		// Verify job state file exists for retry
		jobPath := filepath.Join(adminDir, "history", "jobs", jobID+".json.gz")
		if _, err := os.Stat(jobPath); err != nil {
			t.Errorf("expected job state file at %s, but got error: %v", jobPath, err)
		}

		cancel()
		if err := application.Shutdown(); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		_ = db.Close()
	}

	// 2. Restart app, verify queue remains empty and history still has the job
	{
		db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
		if err != nil {
			t.Fatalf("history.Open restart: %v", err)
		}
		repo := history.NewRepository(db)
		defer db.Close()

		application, err := app.New(appCfg, repo)
		if err != nil {
			t.Fatalf("app.New restart: %v", err)
		}

		if application.Queue().Len() != 0 {
			t.Errorf("Queue length after restart = %d, want 0", application.Queue().Len())
		}

		entries, err := repo.Search(t.Context(), history.SearchOptions{})
		if err != nil {
			t.Fatalf("history search: %v", err)
		}
		if len(entries) != 1 {
			t.Errorf("history entries = %d, want 1", len(entries))
		}
	}
}

type mockEmitter struct {
	mu     sync.Mutex
	events []app.Event
}

func (m *mockEmitter) Broadcast(event app.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
}

func (m *mockEmitter) Events() []app.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]app.Event, len(m.events))
	copy(res, m.events)
	return res
}

func TestRetryHistoryJob(t *testing.T) {
	downloadDir := t.TempDir()
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	rawPayload := makeDeterministic(1024)
	mock := startMockNNTP(t, map[string][]byte{
		"a@t": yencEncodePart("file.bin", 1, 1, rawPayload, 1024, 1, 1024),
	})

	appCfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		config.ServerConfig{
			Name:   "mock",
			Host:   mock.host,
			Port:   mock.port,
			Enable: true,
		},
	)

	db, _ := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	repo := history.NewRepository(db)
	defer db.Close()

	emitter := &mockEmitter{}
	application, _ := app.New(appCfg, repo, app.WithEventEmitter(emitter))

	ctx, cancel := context.WithCancel(t.Context())
	_ = application.Start(ctx)
	defer cancel()
	defer application.Shutdown()

	// 1. Manually create a "failed" history entry and its job file
	jobID := "deadbeef12345678"
	entry := history.Entry{
		NzoID:  jobID,
		Name:   "retry-test",
		Status: "Failed",
	}
	_ = repo.Add(ctx, entry)

	parsedRetry := &nzb.NZB{Files: []nzb.File{
		{Subject: "file.bin", Bytes: 1024, Articles: []nzb.Article{{ID: "a@t", Bytes: 1024, Number: 1}}},
	}}
	job, err := queue.NewJob(parsedRetry, queue.AddOptions{Filename: "retry-test.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	job.ID = jobID
	job.Name = "retry-test"
	job.Status = constants.StatusFailed
	retryQ := queue.New()
	if err := retryQ.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// file.bin stays incomplete (Complete=false); it is re-completed after
	// the retried article resolves successfully.
	if _, err := retryQ.MarkArticlesFailed(job.ID, []string{"a@t"}); err != nil {
		t.Fatalf("MarkArticlesFailed: %v", err)
	}
	jobsDir := filepath.Join(adminDir, "history", "jobs")
	_ = os.MkdirAll(jobsDir, 0o750)

	// Save the job directly to the history jobs directory (where
	// OnJobDone now writes).
	if err := queue.SaveJob(filepath.Join(jobsDir, jobID+".json.gz"), job); err != nil {
		t.Fatalf("queue.SaveJob: %v", err)
	}

	// Pause downloads so the retried job sits at Queued long enough to
	// inspect without racing the downloader/post-processor pipeline. This
	// freezes the job strictly earlier than a post-processing pause would
	// (before MarkJobStarted/RecordDownload ever run), so the status is
	// deterministically Queued rather than "maybe further along."
	application.PauseDownloads()
	application.Queue().PauseAll()
	defer application.ResumeDownloads()

	// 2. Trigger Retry
	if err := application.RetryHistoryJob(ctx, jobID); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	// 3. Verify it's back in the queue
	if application.Queue().Len() != 1 {
		t.Errorf("Queue length = %d, want 1", application.Queue().Len())
	}
	status, _ := application.Queue().GetJobStatus(jobID)
	if status != constants.StatusQueued {
		t.Errorf("Status = %q, want Queued", status)
	}

	// 4. Verify history entry is gone
	if _, err := repo.Get(ctx, jobID); err == nil {
		t.Error("history entry still exists after retry")
	}

	// Verify WebSocket update events were broadcast
	hasQueueUpdated := false
	hasHistoryUpdated := false
	for _, ev := range emitter.Events() {
		if ev.Type == "queue_updated" {
			hasQueueUpdated = true
		}
		if ev.Type == "history_updated" {
			hasHistoryUpdated = true
		}
	}
	if !hasQueueUpdated {
		t.Error("expected queue_updated event, got none")
	}
	if !hasHistoryUpdated {
		t.Error("expected history_updated event, got none")
	}

	// 5. Resume downloads first, then unpause queue to trigger promotion and dispatch
	application.ResumeDownloads()
	application.Queue().ResumeAll(context.Background())
	select {
	case <-application.PostProcComplete():
	case <-time.After(5 * time.Second):
		t.Error("timeout waiting for post-proc to complete after resume")
	}
}

func TestQueuePersistenceAcrossRestart(t *testing.T) {
	downloadDir := t.TempDir()
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	mock := startMockNNTP(t, map[string][]byte{})

	appCfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		config.ServerConfig{
			Name:   "mock",
			Host:   mock.host,
			Port:   mock.port,
			Enable: true,
		},
	)

	// 1. Start app, add a job, and stop app (triggering save)
	{
		application, err := app.New(appCfg, nil)
		if err != nil {
			t.Fatalf("app.New (1): %v", err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		if err := application.Start(ctx); err != nil {
			t.Fatalf("Start (1): %v", err)
		}

		parsed := &nzb.NZB{
			Files: []nzb.File{{
				Subject:  "test.bin",
				Articles: []nzb.Article{{ID: "a@t", Bytes: 100}},
				Bytes:    100,
			}},
		}
		job, _ := queue.NewJob(parsed, queue.AddOptions{Name: "persist-test"}, fsutil.SanitizeOptions{})
		if err := application.Queue().Add(job); err != nil {
			t.Fatalf("Queue.Add: %v", err)
		}

		if application.Queue().Len() != 1 {
			t.Fatalf("Queue length before stop = %d, want 1", application.Queue().Len())
		}

		cancel()
		if err := application.Shutdown(); err != nil {
			t.Fatalf("application.Shutdown: %v", err)
		}
	}

	// 2. Start new app instance and check if job is still there
	{
		application, err := app.New(appCfg, nil)
		if err != nil {
			t.Fatalf("app.New (2): %v", err)
		}

		// IF IT WAS PERSISTED, IT SHOULD BE LOADED NOW
		if application.Queue().Len() != 1 {
			t.Errorf("Queue length after restart = %d, want 1", application.Queue().Len())
		} else {
			jobs := application.Queue().List()
			if jobs[0].Name != "persist-test" {
				t.Errorf("Job name = %q, want %q", jobs[0].Name, "persist-test")
			}
		}
	}
}

func TestFullDownloadLifecycle(t *testing.T) {
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
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	cfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		config.ServerConfig{
			Name:               "mock",
			Host:               mock.host,
			Port:               mock.port,
			Connections:        1,
			PipeliningRequests: 1,
			Timeout:            5,
			Enable:             true,
		},
	)
	cfg.With(func(c *config.Config) {
		c.Categories = []config.CategoryConfig{
			{Name: "Default", Dir: ""},
			{Name: "movies", Dir: "Movies"},
		}
	})
	application, err := app.New(cfg, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
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
	job, err := queue.NewJob(parsed, queue.AddOptions{
		Filename: "test.nzb",
		Name:     "testjob",
		Category: "movies",
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	// Wait for Post-Processing (PostProcComplete)
	select {
	case <-application.PostProcComplete():
		// Files should be moved to CompleteDir/CategoryDir/JobName
		// Our job category is 'movies' which has Dir 'Movies'
		// Note: The pipeline now uses real filenames from NZB subjects (test.bin).
		finalPath := filepath.Join(completeDir, "Movies", "testjob", "test.bin")
		if _, err := os.Stat(finalPath); err != nil {
			t.Errorf("expected final file at %s, but got error: %v", finalPath, err)
		}

		// Verify content
		got, err := os.ReadFile(finalPath)
		if err != nil {
			t.Fatalf("read final file: %v", err)
		}
		if !bytes.Equal(got, raw) {
			t.Errorf("content mismatch in final file")
		}

		// Verify incomplete dir is cleaned up
		incompleteJobDir := filepath.Join(downloadDir, "testjob")
		if _, err := os.Stat(incompleteJobDir); !os.IsNotExist(err) {
			t.Errorf("incomplete directory %s still exists after finalization", incompleteJobDir)
		}

	case <-ctx.Done():
		t.Fatalf("timeout waiting for post-proc completion: %v", ctx.Err())
	}
}

// Reuse helper functions from integration_test.go logic (re-implemented here
// to avoid build-tag exclusion issues during unit tests).

func makeDeterministic(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i * 7 % 256)
	}
	return out
}

func yencEncodePart(name string, partNum, totalParts int, data []byte, fileSize, beginOffset, endOffset int) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=ybegin part=%d total=%d line=128 size=%d name=%s\r\n",
		partNum, totalParts, fileSize, name)
	fmt.Fprintf(&buf, "=ypart begin=%d end=%d\r\n", beginOffset, endOffset)

	encoded := make([]byte, 0, len(data)+len(data)/32)
	for _, b := range data {
		enc := byte((int(b) + 42) % 256)
		if enc == 0 || enc == '\n' || enc == '\r' || enc == '=' {
			encoded = append(encoded, '=')
			enc = byte((int(enc) + 64) % 256)
		}
		encoded = append(encoded, enc)
	}
	const lineLen = 128
	for i := 0; i < len(encoded); i += lineLen {
		end := min(i+lineLen, len(encoded))
		buf.Write(encoded[i:end])
		buf.WriteString("\r\n")
	}

	checksum := crc32.ChecksumIEEE(data)
	fmt.Fprintf(&buf, "=yend size=%d part=%d pcrc32=%08x\r\n", len(data), partNum, checksum)
	return buf.Bytes()
}

type mockNNTP struct {
	host string
	port int
	ln   net.Listener

	bodies map[string][]byte
	wg     sync.WaitGroup
}

func startMockNNTP(t *testing.T, bodies map[string][]byte) *mockNNTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	m := &mockNNTP{
		host:   addr.IP.String(),
		port:   addr.Port,
		ln:     ln,
		bodies: bodies,
	}
	t.Cleanup(func() {
		_ = ln.Close()
		m.wg.Wait()
	})
	m.wg.Go(m.acceptLoop)
	return m
}

func (m *mockNNTP) acceptLoop() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			return
		}
		m.wg.Go(func() {
			defer func() { _ = c.Close() }()
			m.handleConn(c)
		})
	}
}

func (m *mockNNTP) handleConn(c net.Conn) {
	r := bufio.NewReader(c)
	write := func(s string) bool {
		_, err := c.Write([]byte(s))
		return err == nil
	}
	if !write("200 welcome\r\n") {
		return
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		switch {
		case cmd == "CAPABILITIES":
			_ = write("101 capabilities\r\nVERSION 2\r\nREADER\r\n.\r\n")
		case strings.HasPrefix(cmd, "BODY "):
			id := strings.Trim(strings.TrimPrefix(cmd, "BODY "), "<>")
			body, ok := m.bodies[id]
			if !ok {
				_ = write("430 no such article\r\n")
				continue
			}
			_ = write(fmt.Sprintf("222 0 <%s> body follows\r\n", id))
			_ = write(string(dotStuff(body)))
			_ = write("\r\n.\r\n")
		case strings.HasPrefix(cmd, "STAT "):
			id := strings.Trim(strings.TrimPrefix(cmd, "STAT "), "<>")
			if _, ok := m.bodies[id]; !ok {
				_ = write("430 no such article\r\n")
				continue
			}
			_ = write(fmt.Sprintf("223 0 <%s>\r\n", id))
		case cmd == "QUIT":
			_ = write("205 bye\r\n")
			return
		default:
			_ = write("500 unknown command\r\n")
		}
	}
}

func dotStuff(body []byte) []byte {
	if !bytes.Contains(body, []byte("\r\n.")) && (len(body) == 0 || body[0] != '.') {
		return body
	}
	var out bytes.Buffer
	out.Grow(len(body) + 16)
	atLineStart := true
	for _, b := range body {
		if atLineStart && b == '.' {
			out.WriteByte('.')
		}
		out.WriteByte(b)
		atLineStart = b == '\n'
	}
	return out.Bytes()
}

// ---------- M26: TestStart_DoubleStartReturnsError ----------

// TestStart_DoubleStartReturnsError verifies that calling Start twice returns
// ErrAlreadyStarted and the first start remains functional.
func TestStart_DoubleStartReturnsError(t *testing.T) {
	downloadDir := t.TempDir()
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	mock := startMockNNTP(t, nil)

	appCfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		config.ServerConfig{
			Name:   "mock",
			Host:   mock.host,
			Port:   mock.port,
			Enable: true,
		},
	)

	application, err := app.New(appCfg, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	if err := application.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer application.Shutdown()

	// Second Start should fail.
	err = application.Start(ctx)
	if !errors.Is(err, app.ErrAlreadyStarted) {
		t.Errorf("second Start = %v, want ErrAlreadyStarted", err)
	}

	// First start should still be functional — queue operations should work.
	if application.Queue() == nil {
		t.Error("Queue() is nil after double Start")
	}
}

// TestStart_FailedStartResetsStartedFlag verifies that when Start fails (e.g.
// the assembler can't start), the started flag is reset to false so that:
// (a) a subsequent Start call is allowed (not rejected with ErrAlreadyStarted)
// (b) Shutdown is a safe no-op without hanging
//
// Before the fix: started was set to true before assembler.Start; a failure
// left the application permanently stuck in a "started but not running" state.
func TestStart_FailedStartResetsStartedFlag(t *testing.T) {
	downloadDir := t.TempDir()
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	appCfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
	)

	application, err := app.New(appCfg, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	// Force the assembler into a permanently-stopped state so that Start
	// fails at the assembler.Start step.
	if err := application.ForceAssemblerStopped(); err != nil {
		t.Fatalf("ForceAssemblerStopped: %v", err)
	}

	ctx := t.Context()

	err = application.Start(ctx)
	if err == nil {
		application.Shutdown()
		t.Fatal("expected Start to fail after assembler pre-stop, got nil")
	}
	t.Logf("first Start failed as expected: %v", err)

	// (a) started must be false — a second Start must not return ErrAlreadyStarted.
	err2 := application.Start(ctx)
	if errors.Is(err2, app.ErrAlreadyStarted) {
		t.Error("started flag not reset after failed Start — ErrAlreadyStarted returned on retry")
	}
	// The retry also fails (assembler is still stopped) — that's expected.
	// We only care that it's not ErrAlreadyStarted.
	t.Logf("retry Start result: %v", err2)

	// (b) Shutdown must not hang — it should be a clean no-op when Start never
	// succeeded. Use a timeout goroutine to detect hangs.
	done := make(chan struct{})
	go func() {
		application.Shutdown()
		close(done)
	}()
	select {
	case <-done:
		// Shutdown returned cleanly.
	case <-time.After(3 * time.Second):
		t.Error("Shutdown hung after failed Start")
	}
}

// ---------- TestReloadDownloader_ServerChanges ----------

// TestReloadDownloader_ServerChanges verifies that ReloadDownloader correctly
// accepts new server configurations without error.
func TestReloadDownloader_ServerChanges(t *testing.T) {
	downloadDir := t.TempDir()
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	mock1 := startMockNNTP(t, nil)
	mock2 := startMockNNTP(t, nil)

	cfg1 := config.ServerConfig{
		Name:   "server1",
		Host:   mock1.host,
		Port:   mock1.port,
		Enable: true,
	}
	cfg2 := config.ServerConfig{
		Name:   "server2",
		Host:   mock2.host,
		Port:   mock2.port,
		Enable: true,
	}

	appCfg := testConfig(
		downloadDir,
		completeDir,
		adminDir,
		cfg1,
	)

	application, err := app.New(appCfg, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer application.Shutdown()

	// Reload with both servers.
	if err := application.ReloadDownloader([]config.ServerConfig{cfg1, cfg2}); err != nil {
		t.Errorf("ReloadDownloader(both): %v", err)
	}

	// Reload with only server 2.
	if err := application.ReloadDownloader([]config.ServerConfig{cfg2}); err != nil {
		t.Errorf("ReloadDownloader(server2 only): %v", err)
	}

	// Reload back to server 1 only.
	if err := application.ReloadDownloader([]config.ServerConfig{cfg1}); err != nil {
		t.Errorf("ReloadDownloader(server1 only): %v", err)
	}
}

// TestReloadPostProcOptions verifies that ReloadPostProcOptions successfully
func TestReloadPostProcOptions(t *testing.T) {
	tmpDir := t.TempDir()
	dlDir := filepath.Join(tmpDir, "incomplete")
	compDir := filepath.Join(tmpDir, "complete")
	if err := os.MkdirAll(dlDir, 0o750); err != nil {
		t.Fatalf("Failed to create download dir: %v", err)
	}
	if err := os.MkdirAll(compDir, 0o750); err != nil {
		t.Fatalf("Failed to create complete dir: %v", err)
	}

	appCfg := testConfig(
		dlDir,
		compDir,
		filepath.Join(tmpDir, "admin"),
	)

	application, err := app.New(appCfg, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	defer application.Shutdown()

	// Reload with the hot-reloadable stage settings enabled; assert they reach
	// the running stages. Config ownership belongs to the API/config layer, so
	// reload applies pp to the stages and does NOT write config back — this
	// asserts stage state (via the exported accessors), not config.
	application.ReloadPostProcOptions(config.PostProcConfig{
		EnableFileJoin: true,
		Par2Turbo:      true,
		UseGoRAR:       true,
	}, "")

	if !application.StageEnableFileJoin() {
		t.Error("after reload(true): StageEnableFileJoin() = false, want true")
	}
	if !application.StagePar2Turbo() {
		t.Error("after reload(true): StagePar2Turbo() = false, want true")
	}
	if !application.StageUseGoRAR() {
		t.Error("after reload(true): StageUseGoRAR() = false, want true")
	}

	// Reload again with them disabled; assert the stages track the change
	// (proving the accessors reflect successive reloads, not a latched value).
	application.ReloadPostProcOptions(config.PostProcConfig{
		EnableFileJoin: false,
		Par2Turbo:      false,
		UseGoRAR:       false,
	}, "")

	if application.StageEnableFileJoin() {
		t.Error("after reload(false): StageEnableFileJoin() = true, want false")
	}
	if application.StagePar2Turbo() {
		t.Error("after reload(false): StagePar2Turbo() = true, want false")
	}
	if application.StageUseGoRAR() {
		t.Error("after reload(false): StageUseGoRAR() = true, want false")
	}
}

func TestApplication_SettersAndOptions(t *testing.T) {
	dl := t.TempDir()
	comp := t.TempDir()
	admin := t.TempDir()
	cfg := testConfig(dl, comp, admin)

	// Test options
	application, err := app.New(cfg, nil,
		app.WithLogger(slog.Default()),
		app.WithVersion("1.2.3"),
		app.WithCheckpointInterval(time.Second),
	)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	// Test the setters that survived the #109 config-fan-out refactor. The
	// stage-delegating and config-only setters (SetDirectUnpack, SetEnableUnrar,
	// SetPar2Turbo, SetSanitizeOptions, …) were removed: those config fields are
	// now written by the API layer and applied to the stages via the reload
	// translators (see ReloadPostProcOptions), not by per-field Application
	// methods.
	application.SetMinFreeSpace(2048)
	application.SetMaxArtTries(5)
	application.SetMaxArtOpt(2)
	application.SetTopOnly(true)
	application.SetPropagationDelay(10)
	application.SetDownloadDir(t.TempDir())
	application.SetCompleteDir(t.TempDir())

	// Enable direct-unpack via config directly for the maybeDirectUnpack
	// coverage below (formerly done via the removed SetDirectUnpack setters).
	application.GetConfig().With(func(cfg *config.Config) {
		cfg.PostProc.DirectUnpack = true
		cfg.PostProc.DirectUnpackThreads = 5
	})

	// Verify the surviving setters wrote config.
	c := application.GetConfig()
	c.WithRead(func(cfg *config.Config) {
		if cfg.Downloads.MinFreeSpace != 2048 {
			t.Errorf("MinFreeSpace = %d, want 2048", cfg.Downloads.MinFreeSpace)
		}
		if cfg.Downloads.MaxArtTries != 5 {
			t.Errorf("MaxArtTries = %d, want 5", cfg.Downloads.MaxArtTries)
		}
		if cfg.Downloads.MaxArtOpt != 2 {
			t.Errorf("MaxArtOpt = %d, want 2", cfg.Downloads.MaxArtOpt)
		}
		if !cfg.Downloads.TopOnly {
			t.Error("TopOnly not set")
		}
		if cfg.Downloads.PropagationDelay != 10 {
			t.Errorf("PropagationDelay = %d, want 10", cfg.Downloads.PropagationDelay)
		}
	})

	// Test maybeDirectUnpack and buildDirectUnpackOpts coverage
	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject: "test.part01.rar",
			Bytes:   100,
		}},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "du-test", Category: "movies", PP: 3}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	_ = application.Queue().Add(job)

	// Trigger buildDirectUnpackOpts
	_ = application.TriggerBuildDirectUnpackOpts()

	// Trigger maybeDirectUnpack with nonexistent job (nil snap check)
	application.TriggerMaybeDirectUnpack(app.FileComplete{JobID: "nonexistent", FileIdx: 0})

	// Trigger maybeDirectUnpack with valid job but bad index
	application.TriggerMaybeDirectUnpack(app.FileComplete{JobID: job.ID, FileIdx: -1})

	// Trigger maybeDirectUnpack with valid job but non-rar filename
	parsed2 := &nzb.NZB{
		Files: []nzb.File{{
			Subject: "test.txt",
			Bytes:   100,
		}},
	}
	job2, _ := queue.NewJob(parsed2, queue.AddOptions{Name: "du-test-txt", Category: "movies", PP: 3}, fsutil.SanitizeOptions{})
	_ = application.Queue().Add(job2)
	application.TriggerMaybeDirectUnpack(app.FileComplete{JobID: job2.ID, FileIdx: 0})

	// Trigger maybeDirectUnpack with valid job and valid RAR filename (resolves but fails resolveFileInfo)
	application.TriggerMaybeDirectUnpack(app.FileComplete{JobID: job.ID, FileIdx: 0})
}

func TestApplication_EdgeCases(t *testing.T) {
	dl := t.TempDir()
	comp := t.TempDir()
	admin := t.TempDir()
	cfg := testConfig(dl, comp, admin)

	db, _ := history.Open(t.Context(), filepath.Join(admin, "history.db"))
	repo := history.NewRepository(db)
	defer db.Close()

	// 1. Test duplicate checks in AddJob (History DB and backup files)
	application, _ := app.New(cfg, repo)

	// Create a job, add it, wait for it to complete/fail to get in history
	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject: "test.bin",
			Articles: []nzb.Article{
				{ID: "art1", Bytes: 10, Number: 1},
			},
			Bytes: 10,
		}},
	}
	job, _ := queue.NewJob(parsed, queue.AddOptions{Name: "dup-test"}, fsutil.SanitizeOptions{})
	// Set MD5 sum
	job.MD5 = "abcdef0123456789abcdef0123456789"
	job.Filename = "dup-test.nzb"

	// Add it to queue
	_ = application.Queue().Add(job)

	// Try adding the SAME job (Duplicate check: active queue MD5 check)
	job2, _ := queue.NewJob(parsed, queue.AddOptions{Name: "dup-test-2"}, fsutil.SanitizeOptions{})
	job2.MD5 = job.MD5
	job2.Filename = "dup-test.nzb"
	err := application.AddJob(t.Context(), job2, []byte("nzb content"), false)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if job2.Status != constants.StatusPaused || job2.Warning != "Duplicate NZB" {
		t.Errorf("job2.Status = %v, Warning = %v; want Paused, Duplicate NZB", job2.Status, job2.Warning)
	}

	// Add forced duplicate
	job3, _ := queue.NewJob(parsed, queue.AddOptions{Name: "dup-test-3"}, fsutil.SanitizeOptions{})
	job3.MD5 = job.MD5
	err = application.AddJob(t.Context(), job3, []byte("nzb content"), true)
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if job3.Warning != "Duplicate NZB (Forced)" {
		t.Errorf("job3.Warning = %v; want Duplicate NZB (Forced)", job3.Warning)
	}

	// Test unique name collision in downloadDir
	if err := os.MkdirAll(filepath.Join(dl, "collision-name"), 0755); err != nil {
		t.Fatal(err)
	}
	jobCollision, _ := queue.NewJob(parsed, queue.AddOptions{Name: "collision-name"}, fsutil.SanitizeOptions{})
	if err := application.AddJob(t.Context(), jobCollision, []byte("nzb content"), false); err != nil {
		t.Fatalf("AddJob collision: %v", err)
	}
	if jobCollision.Name == "collision-name" {
		t.Errorf("expected unique name suffix for collision, got %q", jobCollision.Name)
	}

	// Test backup duplicate check
	nzbDir := filepath.Join(admin, "nzb")
	if err := os.MkdirAll(nzbDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nzbDir, "backup-test.nzb.gz"), []byte("backup"), 0644); err != nil {
		t.Fatal(err)
	}
	jobBackup, _ := queue.NewJob(parsed, queue.AddOptions{Name: "backup-test"}, fsutil.SanitizeOptions{})
	jobBackup.Filename = "backup-test.nzb"
	_ = application.AddJob(t.Context(), jobBackup, []byte("nzb content"), false)
	if jobBackup.Status != constants.StatusPaused {
		t.Error("expected jobBackup to be paused due to backup duplicate")
	}

	// 2. Test RemoveJob not found error
	err = application.RemoveJob(t.Context(), "nonexistent", true)
	if err == nil {
		t.Error("expected RemoveJob to fail for nonexistent job")
	}

	// 3. Test buildDownloaderOptions and OnJobHopeless callback (triggers maybeFinalize and enqueuePostProc)
	downloaderOpts := application.TriggerBuildDownloaderOptions()

	// Nonexistent job ID (nil snap return)
	downloaderOpts.OnJobHopeless("nonexistent")

	// Valid job ID (triggers maybeFinalize and enqueuePostProc)
	downloaderOpts.OnJobHopeless(job.ID)

	// Add category with flat layout to config
	cfg.With(func(c *config.Config) {
		c.Categories = append(c.Categories, config.CategoryConfig{
			Name: "flatcat",
			Dir:  "flat*",
		})
	})
	jobFlat, _ := queue.NewJob(parsed, queue.AddOptions{Name: "flat-test", Category: "flatcat", PP: 3}, fsutil.SanitizeOptions{})
	_ = application.Queue().Add(jobFlat)

	// Inject a dummy direct unpacker to test enqueuePostProc direct unpack handoff
	duEdge := directunpack.New(slog.Default(), jobFlat.ID, t.TempDir(), t.TempDir(), directunpack.Options{})
	application.InjectDirectUnpacker(jobFlat.ID, duEdge)

	// Trigger hopeless on flat job
	_ = application.Queue().SetStatus(jobFlat.ID, constants.StatusDownloading)
	downloaderOpts.OnJobHopeless(jobFlat.ID)

	// 4. Test handleFileComplete branches
	// Nonexistent job (early return)
	application.TriggerHandleFileComplete(t.Context(), app.FileComplete{JobID: "nonexistent", FileIdx: 0})

	// Cancelled context (early return)
	cancelledCtx, cancelCtx := context.WithCancel(t.Context())
	cancelCtx()
	application.TriggerHandleFileComplete(cancelledCtx, app.FileComplete{JobID: job.ID, FileIdx: 0})

	// Job already in post-processing (early return)
	jobPostProc, _ := queue.NewJob(parsed, queue.AddOptions{Name: "postproc-already"}, fsutil.SanitizeOptions{})
	_, _ = application.Queue().SetPostProcStarted(jobPostProc.ID)
	_ = application.Queue().Add(jobPostProc)
	application.TriggerHandleFileComplete(t.Context(), app.FileComplete{JobID: jobPostProc.ID, FileIdx: 0})

	// 5. Test drainCompletions case branch (channel has entry)
	application.SendFileComplete(app.FileComplete{JobID: "nonexistent", FileIdx: 0})
	application.TriggerDrainCompletions(t.Context())

	// 6. Test maybeFinalize double start branch (returns early when already started)
	application.TriggerMaybeFinalize(job.ID, "")

	// 7. Test maybeFinalize queue save error branch (triggers warning log)
	_ = application.Queue().SetStatus(jobCollision.ID, constants.StatusDownloading)
	cfg.With(func(c *config.Config) {
		c.General.AdminDir = "/nonexistent-permission-denied"
	})
	application.TriggerMaybeFinalize(jobCollision.ID, "")

	// 8. Test maybeDirectUnpack branches
	// Create a job with RAR volumes
	parsedRar := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "test.part1.rar"},
			{Subject: "test.part2.rar"},
		},
	}
	jobRar, _ := queue.NewJob(parsedRar, queue.AddOptions{Name: "rar-job", PP: 3}, fsutil.SanitizeOptions{})
	_ = application.Queue().Add(jobRar)

	// Low PP (returns early)
	jobNoPP, _ := queue.NewJob(parsedRar, queue.AddOptions{Name: "no-pp-job", PP: 1}, fsutil.SanitizeOptions{})
	_ = application.Queue().Add(jobNoPP)
	application.TriggerMaybeDirectUnpack(app.FileComplete{JobID: jobNoPP.ID, FileIdx: 0})

	// Password set (returns early)
	jobPwd, _ := queue.NewJob(parsedRar, queue.AddOptions{Name: "pwd-job", PP: 3, Password: "foo"}, fsutil.SanitizeOptions{})
	_ = application.Queue().Add(jobPwd)
	application.TriggerMaybeDirectUnpack(app.FileComplete{JobID: jobPwd.ID, FileIdx: 0})

	// Invalid file index (returns early)
	application.TriggerMaybeDirectUnpack(app.FileComplete{JobID: jobRar.ID, FileIdx: -1})
	application.TriggerMaybeDirectUnpack(app.FileComplete{JobID: jobRar.ID, FileIdx: 99})

	// Not a RAR volume (returns early)
	application.TriggerMaybeDirectUnpack(app.FileComplete{JobID: job.ID, FileIdx: 0})

	// resolveFileInfo fails (returns early)
	application.TriggerMaybeDirectUnpack(app.FileComplete{JobID: jobRar.ID, FileIdx: 0})

	// Concurrency limit reached (returns early)
	application.InjectPipelineFileInfo(jobRar.ID, 0, filepath.Join(dl, "test.part1.rar"))
	cfg.With(func(c *config.Config) {
		c.PostProc.DirectUnpackThreads = 1
		c.PostProc.DirectUnpack = true
		c.PostProc.EnableUnrar = true
	})
	application.SetActiveDU(1)
	application.TriggerMaybeDirectUnpack(app.FileComplete{JobID: jobRar.ID, FileIdx: 0})

	// Success path (creates new direct unpacker and calls Add)
	application.SetActiveDU(0)
	application.InjectCtx(t.Context())
	application.TriggerMaybeDirectUnpack(app.FileComplete{JobID: jobRar.ID, FileIdx: 0})
}

func TestApplication_PersistAndCommitError(t *testing.T) {
	dl := t.TempDir()
	comp := t.TempDir()
	admin := t.TempDir()
	cfg := testConfig(dl, comp, admin)

	db, err := history.Open(t.Context(), filepath.Join(admin, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)

	application, err := app.New(cfg, repo)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject: "test.bin",
			Bytes:   10,
		}},
	}
	qJob, err := queue.NewJob(parsed, queue.AddOptions{Name: "persist-err-test"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	_ = application.Queue().Add(qJob)

	// Construct a postproc.Job wrapping the queue.Job
	ppJob := &postproc.Job{
		Queue: qJob,
	}

	// Close the DB database connection so history.Add fails
	db.Close()

	entry := history.Entry{
		NzoID: qJob.ID,
		Name:  qJob.Name,
	}

	// Trigger persistAndCommit — it should fail because DB is closed
	err = application.TriggerPersistAndCommit(slog.Default(), entry, ppJob)
	if err == nil {
		t.Error("expected persistAndCommit to fail when DB is closed")
	}

	// Verify the gz payload was deleted/cleaned up
	histJobsDir := filepath.Join(admin, "history", "jobs")
	jobPath := filepath.Join(histJobsDir, qJob.ID+".json.gz")
	if _, err := os.Stat(jobPath); err == nil || !os.IsNotExist(err) {
		t.Errorf("expected gz file %q to be deleted, got err: %v", jobPath, err)
	}
}

func TestApplication_ShutdownWithOptions(t *testing.T) {
	dl := t.TempDir()
	comp := t.TempDir()
	admin := t.TempDir()
	cfg := testConfig(dl, comp, admin)

	application, err := app.New(cfg, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	// 1. Test Shutdown on unstarted application (returns nil)
	if err := application.Shutdown(); err != nil {
		t.Errorf("expected no error shutting down unstarted app, got %v", err)
	}

	// 2. Start application
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Inject a dummy direct unpacker
	du := directunpack.New(slog.Default(), "job1", t.TempDir(), t.TempDir(), directunpack.Options{})
	application.InjectDirectUnpacker("job1", du)

	// Verify DirectUnpackStatus
	_, ok := application.DirectUnpackStatus("job1")
	if !ok {
		t.Error("DirectUnpackStatus: expected ok=true, got false")
	}

	_, ok = application.DirectUnpackStatus("nonexistent")
	if ok {
		t.Error("DirectUnpackStatus: expected ok=false for nonexistent job, got true")
	}

	// Call Shutdown on started application (verifies the abort loop and normal cleanup)
	if err := application.Shutdown(); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestDirectUnpackStatuses_ReturnsAllActiveJobs proves DirectUnpackStatuses
// snapshots every active direct-unpacker's status in a single call (OPT-12),
// instead of requiring one DirectUnpackStatus(jobID) call — and therefore one
// app.mu acquisition — per job.
func TestDirectUnpackStatuses_ReturnsAllActiveJobs(t *testing.T) {
	dl := t.TempDir()
	comp := t.TempDir()
	admin := t.TempDir()
	cfg := testConfig(dl, comp, admin)

	application, err := app.New(cfg, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	du1 := directunpack.New(slog.Default(), "job-1", t.TempDir(), t.TempDir(), directunpack.Options{})
	du2 := directunpack.New(slog.Default(), "job-2", t.TempDir(), t.TempDir(), directunpack.Options{})
	application.InjectDirectUnpacker("job-1", du1)
	application.InjectDirectUnpacker("job-2", du2)

	statuses := application.DirectUnpackStatuses()
	if len(statuses) != 2 {
		t.Fatalf("len(statuses) = %d; want 2", len(statuses))
	}
	if _, ok := statuses["job-1"]; !ok {
		t.Errorf("statuses missing job-1")
	}
	if _, ok := statuses["job-2"]; !ok {
		t.Errorf("statuses missing job-2")
	}
}

func TestApp_EventLoopStarvation(t *testing.T) {
	dl := t.TempDir()
	comp := t.TempDir()
	admin := t.TempDir()
	cfg := testConfig(dl, comp, admin)

	application, err := app.New(cfg, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer application.Shutdown()

	// Read test RAR archive from unpack testdata to simulate DirectUnpack blocking
	rarData, err := os.ReadFile("../unpack/testdata/multi_new.part01.rar")
	if err != nil {
		t.Fatalf("failed to read test archive: %v", err)
	}
	rarPath := filepath.Join(dl, "multi_new.part01.rar")
	if err := os.WriteFile(rarPath, rarData, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// 1. Add job1 (will use DirectUnpack and block on vol 2)
	parsed1 := nzb.NZB{Files: []nzb.File{{Subject: "multi_new.part01.rar", Bytes: 1024}}}
	job1, _ := queue.NewJob(&parsed1, queue.AddOptions{Name: "job1-du", PP: 3}, fsutil.SanitizeOptions{})
	if err := application.Queue().Add(job1); err != nil {
		t.Fatalf("add job1: %v", err)
	}
	_ = application.Queue().SetStatus(job1.ID, constants.StatusDownloading)

	du := directunpack.New(slog.Default(), job1.ID, dl, comp, directunpack.Options{})
	du.SetAllFilenames([]string{"multi_new.part01.rar", "multi_new.part02.rar"})
	du.Add(ctx, "multi_new.part01.rar", rarPath)
	application.InjectDirectUnpacker(job1.ID, du)
	defer du.Abort()

	// 2. Add job2 (normal job without DirectUnpack)
	parsed2 := nzb.NZB{Files: []nzb.File{{Subject: "normal.txt", Bytes: 100}}}
	job2, _ := queue.NewJob(&parsed2, queue.AddOptions{Name: "job2-normal", PP: 3}, fsutil.SanitizeOptions{})
	if err := application.Queue().Add(job2); err != nil {
		t.Fatalf("add job2: %v", err)
	}
	_ = application.Queue().SetStatus(job2.ID, constants.StatusDownloading)

	// Send completion events: job1 first, then job2 immediately after.
	// In unpatched code, watchCompletions blocks synchronously on job1's du.Wait(),
	// preventing it from ever processing job2's completion event.
	application.SendFileComplete(app.FileComplete{JobID: job1.ID, FileIdx: 0})
	application.SendFileComplete(app.FileComplete{JobID: job2.ID, FileIdx: 0})

	// Assert that job2 completes without being starved by job1's DirectUnpack.
	select {
	case jc := <-application.JobComplete():
		if jc.JobID == job1.ID {
			t.Fatalf("job1 completed prematurely; expected du.Wait() to block on vol 2")
		}
		if jc.JobID != job2.ID {
			t.Fatalf("expected job2 to complete, got job %q", jc.JobID)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("completion event loop starved: job2 did not complete because watchCompletions blocked on job1's du.Wait()")
	}
}

type contextCheckingNotifier struct {
	mu          sync.Mutex
	receivedErr error
}

func (n *contextCheckingNotifier) Name() string                    { return "context-checker" }
func (n *contextCheckingNotifier) Accepts(notifier.EventType) bool { return true }
func (n *contextCheckingNotifier) Send(ctx context.Context, _ notifier.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.receivedErr = ctx.Err()
	return n.receivedErr
}

func TestApp_ShutdownContextInheritance(t *testing.T) {
	dl := t.TempDir()
	comp := t.TempDir()
	admin := t.TempDir()
	cfg := testConfig(dl, comp, admin)

	db, err := history.Open(t.Context(), filepath.Join(admin, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	defer db.Close()
	repo := history.NewRepository(db)

	application, err := app.New(cfg, repo)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	checker := &contextCheckingNotifier{}
	dispatcher := notifier.NewDispatcher(slog.Default())
	dispatcher.Register(checker)
	application.SetNotifier(dispatcher)

	// Create and immediately cancel the application lifecycle context
	ctx, cancel := context.WithCancel(t.Context())
	application.InjectCtx(ctx)
	cancel()

	// 1. Verify fireCompletionNotification derives from canceled lifecycle context rather than orphaned context.Background()
	entry := history.Entry{
		NzoID:  "test-ctx-inherit",
		Name:   "test-job-ctx",
		Status: "Completed",
	}
	application.TriggerFireCompletionNotification(entry)

	checker.mu.Lock()
	gotErr := checker.receivedErr
	checker.mu.Unlock()
	if !errors.Is(gotErr, context.Canceled) {
		t.Errorf("expected completion notification context to inherit cancellation from parent lifecycle context, got %v", gotErr)
	}

	// 2. Verify persistAndCommit derives from canceled lifecycle context rather than orphaned context.Background()
	parsed := &nzb.NZB{
		Files: []nzb.File{{
			Subject: "test.bin",
			Bytes:   10,
		}},
	}
	qJob, err := queue.NewJob(parsed, queue.AddOptions{Name: "persist-ctx-test"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	_ = application.Queue().Add(qJob)
	ppJob := &postproc.Job{
		Queue: qJob,
	}

	err = application.TriggerPersistAndCommit(slog.Default(), entry, ppJob)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected persistAndCommit to fail with context.Canceled when parent lifecycle context is canceled, got %v", err)
	}
}

type deadlineCheckingNotifier struct {
	onSend func(ctx context.Context)
}

func (n *deadlineCheckingNotifier) Name() string                    { return "deadline-checker" }
func (n *deadlineCheckingNotifier) Accepts(notifier.EventType) bool { return true }
func (n *deadlineCheckingNotifier) Send(ctx context.Context, _ notifier.Event) error {
	if n.onSend != nil {
		n.onSend(ctx)
	}
	return nil
}

func TestApp_FireCompletionNotificationTimeout(t *testing.T) {
	dl := t.TempDir()
	comp := t.TempDir()
	admin := t.TempDir()
	cfg := testConfig(dl, comp, admin)
	application, _ := app.New(cfg, nil)

	var gotDeadline time.Time
	var hasDeadline bool
	var gotErr error

	checker := &deadlineCheckingNotifier{
		onSend: func(ctx context.Context) {
			gotDeadline, hasDeadline = ctx.Deadline()
			gotErr = ctx.Err()
		},
	}
	dispatcher := notifier.NewDispatcher(slog.Default())
	dispatcher.Register(checker)
	application.SetNotifier(dispatcher)

	application.TriggerFireCompletionNotification(history.Entry{Status: "Completed", Name: "test"})

	if !hasDeadline {
		t.Fatal("expected notification context to have a deadline")
	}
	if gotErr != nil {
		t.Fatalf("expected notification context to be active (not timed out), got err: %v", gotErr)
	}
	remaining := time.Until(gotDeadline)
	if remaining < 25*time.Second || remaining > 35*time.Second {
		t.Fatalf("expected notification timeout to be ~30s, got remaining: %v", remaining)
	}
}

type dummyDownloader struct{}

func (d *dummyDownloader) Start(ctx context.Context) error               { return nil }
func (d *dummyDownloader) Stop() error                                   { return nil }
func (d *dummyDownloader) Completions() <-chan *downloader.ArticleResult { return nil }
func (d *dummyDownloader) SetSpeedLimit(bytesPerSec int64)               {}
func (d *dummyDownloader) SetDispatchOptions(maxArtTries, maxArtOpt int, topOnly bool, propagationDelay time.Duration) {
}
func (d *dummyDownloader) UnblockServer(name string) bool { return true }
func (d *dummyDownloader) Pause()                         {}
func (d *dummyDownloader) Resume()                        {}
func (d *dummyDownloader) DisconnectAll()                 {}

type wedgedDownloader struct {
	dummyDownloader
	stopCh chan struct{}
}

func (w *wedgedDownloader) Stop() error {
	if w.stopCh != nil {
		<-w.stopCh
		return nil
	}
	select {}
}

func TestApplication_Shutdown_WedgedComponent(t *testing.T) {
	adminDir := t.TempDir()
	cfg := testConfig(t.TempDir(), t.TempDir(), adminDir)

	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	dl := &wedgedDownloader{stopCh: stopCh}

	application, err := app.New(cfg, nil, app.WithDownloader(dl))
	if err != nil {
		t.Fatalf("build app: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	if err := application.Start(ctx); err != nil {
		t.Fatalf("start app: %v", err)
	}

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- application.Shutdown()
	}()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if elapsed > 25*time.Second {
			t.Errorf("Shutdown took %v, expected budget ~15s", elapsed)
		}
		if err == nil {
			t.Error("expected shutdown error due to wedged downloader, got nil")
		}
		queuePath := filepath.Join(adminDir, "queue", "queue.json.gz")
		if _, statErr := os.Stat(queuePath); os.IsNotExist(statErr) {
			t.Errorf("expected queue file to exist at %s, but it was not created", queuePath)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown hung indefinitely on wedged downloader")
	}
}

func TestApplication_TestNNTPServer(t *testing.T) {
	t.Parallel()
	appInst := &app.Application{}
	cfg := config.ServerConfig{
		Host:    "127.0.0.1",
		Port:    1,
		Timeout: 1,
	}
	res, err := appInst.TestNNTPServer(t.Context(), cfg)
	if err == nil {
		t.Errorf("expected error connecting to port 1, got nil")
	}
	if res.ConnectionLimitExceeded {
		t.Errorf("res.ConnectionLimitExceeded = true, want false for connection refused")
	}
}

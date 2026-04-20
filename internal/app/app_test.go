package app_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/app"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/constants"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

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

	appCfg := app.Config{
		DownloadDir: downloadDir,
		CompleteDir: completeDir,
		AdminDir:    adminDir,
		CacheLimit:  1 * 1024 * 1024,
		Servers: []config.ServerConfig{{
			Name:   "mock",
			Host:   mock.host,
			Port:   mock.port,
			Enable: true,
		}},
	}

	// 1. Start app, download job, check it moves to history and is removed from queue
	{
		db, err := history.Open(filepath.Join(adminDir, "history.db"))
		if err != nil {
			t.Fatalf("history.Open: %v", err)
		}
		repo := history.NewRepository(db)

		application, err := app.New(appCfg, repo)
		if err != nil {
			t.Fatalf("app.New: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
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
		job, _ := queue.NewJob(parsed, queue.AddOptions{Name: "history-test"})
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
		entry, err := repo.Get(context.Background(), jobID)
		if err != nil {
			t.Fatalf("job not found in history: %v", err)
		}
		if entry.Name != "history-test" {
			t.Errorf("history entry name = %q, want %q", entry.Name, "history-test")
		}
		if entry.Status != "Completed" {
			t.Errorf("history entry status = %q, want %q", entry.Status, "Completed")
		}

		cancel()
		if err := application.Shutdown(); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		_ = db.Close()
	}

	// 2. Restart app, verify queue remains empty and history still has the job
	{
		db, err := history.Open(filepath.Join(adminDir, "history.db"))
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

		entries, err := repo.Search(context.Background(), history.SearchOptions{})
		if err != nil {
			t.Fatalf("history search: %v", err)
		}
		if len(entries) != 1 {
			t.Errorf("history entries = %d, want 1", len(entries))
		}
	}
}

func TestRetryHistoryJob(t *testing.T) {
	downloadDir := t.TempDir()
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	mock := startMockNNTP(t, map[string][]byte{})

	appCfg := app.Config{
		DownloadDir: downloadDir,
		CompleteDir: completeDir,
		AdminDir:    adminDir,
		CacheLimit:  1 * 1024 * 1024,
		Servers: []config.ServerConfig{{
			Name:   "mock",
			Host:   mock.host,
			Port:   mock.port,
			Enable: true,
		}},
	}

	db, _ := history.Open(filepath.Join(adminDir, "history.db"))
	repo := history.NewRepository(db)
	defer db.Close()

	application, _ := app.New(appCfg, repo)
	ctx, cancel := context.WithCancel(context.Background())
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

	job := &queue.Job{
		ID:     jobID,
		Name:   "retry-test",
		Status: constants.StatusFailed,
		Files: []queue.JobFile{{
			Subject: "file.bin",
			Complete: true, // Mark as complete so post-proc triggers immediately
			Articles: []queue.JobArticle{{ID: "a@t", Done: true}},
		}},
	}
	jobsDir := filepath.Join(adminDir, "queue", "jobs")
	_ = os.MkdirAll(jobsDir, 0o750)
	
	// We need to use the internal writeGzJSON or similar to create the file.
	// Since it's internal to queue, we'll just use a dummy for now and see if app.RetryHistoryJob works.
	// Actually, queue.Save is available. Let's use that.
	q := queue.New()
	_ = q.Add(job)
	_ = q.Save(filepath.Join(adminDir, "queue"))
	_ = q.Remove(jobID) // remove from active queue

	// 2. Trigger Retry
	if err := application.RetryHistoryJob(ctx, jobID); err != nil {
		t.Fatalf("RetryHistoryJob: %v", err)
	}

	// 3. Verify it's back in the queue
	if application.Queue().Len() != 1 {
		t.Errorf("Queue length = %d, want 1", application.Queue().Len())
	}
	got, _ := application.Queue().Get(jobID)
	if got.Status != constants.StatusQueued {
		t.Errorf("Status = %q, want %q", got.Status, constants.StatusQueued)
	}

	// 4. Verify history entry is gone
	if _, err := repo.Get(ctx, jobID); err == nil {
		t.Error("history entry still exists after retry")
	}
}

func TestQueuePersistenceAcrossRestart(t *testing.T) {
	downloadDir := t.TempDir()
	completeDir := t.TempDir()
	adminDir := t.TempDir()

	mock := startMockNNTP(t, map[string][]byte{})

	appCfg := app.Config{
		DownloadDir: downloadDir,
		CompleteDir: completeDir,
		AdminDir:    adminDir,
		CacheLimit:  1 * 1024 * 1024,
		Servers: []config.ServerConfig{{
			Name:   "mock",
			Host:   mock.host,
			Port:   mock.port,
			Enable: true,
		}},
	}

	// 1. Start app, add a job, and stop app (triggering save)
	{
		application, err := app.New(appCfg, nil)
		if err != nil {
			t.Fatalf("app.New (1): %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
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
		job, _ := queue.NewJob(parsed, queue.AddOptions{Name: "persist-test"})
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

	application, err := app.New(app.Config{
		DownloadDir: downloadDir,
		CompleteDir: completeDir,
		AdminDir:    adminDir,
		CacheLimit:  1 * 1024 * 1024,
		Servers: []config.ServerConfig{{
			Name:               "mock",
			Host:               mock.host,
			Port:               mock.port,
			Connections:        1,
			PipeliningRequests: 1,
			Timeout:            5,
			Enable:             true,
		}},
		Categories: []config.CategoryConfig{
			{Name: "Default", Dir: ""},
			{Name: "movies", Dir: "Movies"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	// 1. Wait for assembly (FileComplete)
	select {
	case <-application.FileComplete():
		// Assemblies should go into DownloadDir/JobName
		incompletePath := filepath.Join(downloadDir, "testjob", "0000.tmp")
		if _, err := os.Stat(incompletePath); err != nil {
			t.Errorf("expected incomplete file at %s, but got error: %v", incompletePath, err)
		}
	case <-ctx.Done():
		t.Fatalf("timeout waiting for file completion: %v", ctx.Err())
	}

	// 2. Wait for Post-Processing (PostProcComplete)
	select {
	case <-application.PostProcComplete():
		// Files should be moved to CompleteDir/CategoryDir/JobName
		// Our job category is 'movies' which has Dir 'Movies'
		// Note: The RepairStage renames 0000.tmp to test.bin via fallback naming.
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
		end := i + lineLen
		if end > len(encoded) {
			end = len(encoded)
		}
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
	go m.acceptLoop()
	return m
}

func (m *mockNNTP) acceptLoop() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			return
		}
		m.wg.Add(1)
		go func(c net.Conn) {
			defer m.wg.Done()
			defer func() { _ = c.Close() }()
			m.handleConn(c)
		}(c)
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

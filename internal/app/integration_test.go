//go:build integration

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
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
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

	application, err := app.New(app.Config{
		DownloadDir: downloadDir,
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
	})
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
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "test.nzb", Name: "testjob"})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := application.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	select {
	case fc := <-application.FileComplete():
		if fc.JobID != job.ID {
			t.Fatalf("FileComplete JobID = %s, want %s", fc.JobID, job.ID)
		}
		if fc.FileIdx != 0 {
			t.Fatalf("FileComplete FileIdx = %d, want 0", fc.FileIdx)
		}
	case <-ctx.Done():
		t.Fatalf("timeout waiting for file completion: %v", ctx.Err())
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
}

// makeDeterministic produces a reproducible byte sequence including every
// byte value 0x00–0xff, giving decoder coverage of escape bytes in the
// end-to-end flow.
func makeDeterministic(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i * 7 % 256)
	}
	return out
}

// yencEncodePart builds a multi-part yEnc article body (no dot-stuffing;
// the mock NNTP server applies that at the wire level).
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

// mockNNTP is a minimal RFC 3977-shaped server for integration tests.
// It speaks only the verbs the downloader issues: the greeting,
// CAPABILITIES, BODY, STAT, QUIT. No auth, no TLS.
type mockNNTP struct {
	host string
	port int
	ln   net.Listener

	bodies map[string][]byte // keyed by message-id (no angle brackets)

	wg sync.WaitGroup
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
			// 222 response then dot-stuffed body then ".\r\n".
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

// dotStuff doubles any leading '.' on a line, per RFC 3977 §3.1.1.
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

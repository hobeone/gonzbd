package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/nntp"
)

func createTestRelease(t *testing.T, releaseDir string) *ReleaseManifest {
	t.Helper()

	filesDir := filepath.Join(releaseDir, "files")
	if err := os.MkdirAll(filesDir, 0o750); err != nil {
		t.Fatalf("mkdir files dir: %v", err)
	}

	singlePayload := []byte("hello single part release payload for testing 1234567890!")
	singlePath := filepath.Join(filesDir, "single.bin")
	if err := os.WriteFile(singlePath, singlePayload, 0o600); err != nil {
		t.Fatalf("write single: %v", err)
	}
	singleSum := sha256.Sum256(singlePayload)
	singleCRC := crc32.ChecksumIEEE(singlePayload)

	multiPayload := bytes.Repeat([]byte("0123456789abcdef"), 100) // 1600 bytes
	multiPath := filepath.Join(filesDir, "multi.bin")
	if err := os.WriteFile(multiPath, multiPayload, 0o600); err != nil {
		t.Fatalf("write multi: %v", err)
	}
	multiSum := sha256.Sum256(multiPayload)

	// Multi has 2 segments of 800 bytes each
	seg1 := multiPayload[:800]
	seg2 := multiPayload[800:]

	manifest := &ReleaseManifest{
		Name:     "test-release",
		PartSize: 800,
		ExpectedOutputs: []ExpectedOutput{
			{Name: "single.bin", Size: int64(len(singlePayload)), SHA256: hex.EncodeToString(singleSum[:])},
			{Name: "multi.bin", Size: int64(len(multiPayload)), SHA256: hex.EncodeToString(multiSum[:])},
		},
		Files: []ReleaseFile{
			{
				FileIndex:   0,
				DiskName:    "single.bin",
				NZBFilename: "single.bin",
				Role:        RoleSidecar,
				TotalSize:   int64(len(singlePayload)),
				SHA256:      hex.EncodeToString(singleSum[:]),
				Segments: []SegmentMeta{
					{
						MessageID: "single-part-1@localnntp.test",
						PartNum:   1,
						Offset:    0,
						Length:    len(singlePayload),
						CRC32:     singleCRC,
					},
				},
			},
			{
				FileIndex:   1,
				DiskName:    "multi.bin",
				NZBFilename: "multi.bin",
				Role:        RoleRARVolume,
				VolumeNum:   1,
				TotalSize:   int64(len(multiPayload)),
				SHA256:      hex.EncodeToString(multiSum[:]),
				Segments: []SegmentMeta{
					{
						MessageID: "multi-part-1@localnntp.test",
						PartNum:   1,
						Offset:    0,
						Length:    800,
						CRC32:     crc32.ChecksumIEEE(seg1),
					},
					{
						MessageID: "multi-part-2@localnntp.test",
						PartNum:   2,
						Offset:    800,
						Length:    800,
						CRC32:     crc32.ChecksumIEEE(seg2),
					},
				},
			},
		},
	}

	return manifest
}

func dialNNTP(t *testing.T, srv *Server, user, pass string) *nntp.Conn {
	t.Helper()

	host, portStr, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi port: %v", err)
	}

	cfg := config.ServerConfig{
		Name:               "test-server",
		Host:               host,
		Port:               port,
		Username:           user,
		Password:           pass,
		Connections:        1,
		Enable:             true,
		Timeout:            5,
		PipeliningRequests: 1,
	}

	checked, err := cfg.Checked()
	if err != nil {
		t.Fatalf("ServerConfig.Checked: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := nntp.Dial(ctx, checked)
	if err != nil {
		t.Fatalf("nntp.Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestServer_HappyPath_SingleAndMultiPart(t *testing.T) {
	dir := t.TempDir()
	manifest := createTestRelease(t, dir)

	srv, err := NewServer(ServerConfig{
		ReleaseDir: dir,
		Manifest:   manifest,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	conn := dialNNTP(t, srv, "", "")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 1. Fetch single part
	body, err := conn.Fetch(ctx, "single-part-1@localnntp.test")
	if err != nil {
		t.Fatalf("Fetch single: %v", err)
	}
	art, err := decoder.DecodeArticle(body)
	if err != nil {
		t.Fatalf("DecodeArticle single: %v", err)
	}
	wantSingle := []byte("hello single part release payload for testing 1234567890!")
	if !bytes.Equal(art.Data, wantSingle) {
		t.Fatalf("single body mismatch: got %q, want %q", art.Data, wantSingle)
	}
	if art.CRC != manifest.Files[0].Segments[0].CRC32 {
		t.Fatalf("single CRC mismatch: got %08X, want %08X", art.CRC, manifest.Files[0].Segments[0].CRC32)
	}

	// 2. Fetch multi part 1
	body1, err := conn.Fetch(ctx, "multi-part-1@localnntp.test")
	if err != nil {
		t.Fatalf("Fetch multi part 1: %v", err)
	}
	art1, err := decoder.DecodeArticle(body1)
	if err != nil {
		t.Fatalf("DecodeArticle multi part 1: %v", err)
	}
	if !art1.HasOffset || art1.Offset != 0 {
		t.Fatalf("multi part 1 offset mismatch: offset %d (hasOffset %v)", art1.Offset, art1.HasOffset)
	}
	if len(art1.Data) != 800 {
		t.Fatalf("multi part 1 length: got %d, want 800", len(art1.Data))
	}

	// 3. Fetch multi part 2
	body2, err := conn.Fetch(ctx, "multi-part-2@localnntp.test")
	if err != nil {
		t.Fatalf("Fetch multi part 2: %v", err)
	}
	art2, err := decoder.DecodeArticle(body2)
	if err != nil {
		t.Fatalf("DecodeArticle multi part 2: %v", err)
	}
	if !art2.HasOffset || art2.Offset != 800 {
		t.Fatalf("multi part 2 offset mismatch: offset %d (hasOffset %v)", art2.Offset, art2.HasOffset)
	}
	if len(art2.Data) != 800 {
		t.Fatalf("multi part 2 length: got %d, want 800", len(art2.Data))
	}

	stats := srv.Stats()
	if stats.ServedCleanTotal != 3 {
		t.Errorf("ServedCleanTotal = %d, want 3", stats.ServedCleanTotal)
	}
	if stats.FaultsFiredTotal != 0 {
		t.Errorf("FaultsFiredTotal = %d, want 0", stats.FaultsFiredTotal)
	}
	if stats.ServedClean["single-part-1@localnntp.test"] != 1 {
		t.Errorf("single-part-1 clean count = %d, want 1", stats.ServedClean["single-part-1@localnntp.test"])
	}
}

func TestServer_NNTPProtocol(t *testing.T) {
	dir := t.TempDir()
	manifest := createTestRelease(t, dir)

	srv, err := NewServer(ServerConfig{
		ReleaseDir: dir,
		Manifest:   manifest,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	reader := bufio.NewReader(c)

	// Greeting
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if !strings.HasPrefix(line, "200 ") {
		t.Fatalf("greeting mismatch: %s", line)
	}

	// CAPABILITIES
	if _, err := c.Write([]byte("CAPABILITIES\r\n")); err != nil {
		t.Fatalf("write CAPABILITIES: %v", err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "101 ") {
		t.Fatalf("CAPABILITIES line 1: %s (err %v)", line, err)
	}
	var caps []string
	for {
		l, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read caps: %v", err)
		}
		trimmed := strings.TrimRight(l, "\r\n")
		if trimmed == "." {
			break
		}
		caps = append(caps, trimmed)
	}
	if !strings.Contains(strings.Join(caps, " "), "READER") {
		t.Errorf("caps missing READER: %v", caps)
	}

	// GROUP
	if _, err := c.Write([]byte("GROUP alt.binaries.test\r\n")); err != nil {
		t.Fatalf("write GROUP: %v", err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "211 ") {
		t.Fatalf("GROUP response: %s", line)
	}

	// STAT valid
	if _, err := c.Write([]byte("STAT <single-part-1@localnntp.test>\r\n")); err != nil {
		t.Fatalf("write STAT: %v", err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "223 ") {
		t.Fatalf("STAT valid response: %s", line)
	}

	// STAT missing
	if _, err := c.Write([]byte("STAT <nonexistent@localnntp.test>\r\n")); err != nil {
		t.Fatalf("write STAT nonexistent: %v", err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "430 ") {
		t.Fatalf("STAT missing response: %s", line)
	}

	// ARTICLE valid (verify RFC 3977 headers + blank line separator before body)
	if _, err := c.Write([]byte("ARTICLE <single-part-1@localnntp.test>\r\n")); err != nil {
		t.Fatalf("write ARTICLE: %v", err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "220 ") {
		t.Fatalf("ARTICLE valid response: %s", line)
	}
	hdrLine, err := reader.ReadString('\n')
	if err != nil || strings.TrimRight(hdrLine, "\r\n") != "Message-ID: <single-part-1@localnntp.test>" {
		t.Fatalf("ARTICLE expected Message-ID header, got %q (err %v)", hdrLine, err)
	}
	sepLine, err := reader.ReadString('\n')
	if err != nil || strings.TrimRight(sepLine, "\r\n") != "" {
		t.Fatalf("ARTICLE expected blank header/body separator line, got %q (err %v)", sepLine, err)
	}
	// read until terminator
	for {
		l, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read article body: %v", err)
		}
		if strings.TrimRight(l, "\r\n") == "." {
			break
		}
	}

	// Unknown command
	if _, err := c.Write([]byte("FOOBAR\r\n")); err != nil {
		t.Fatalf("write FOOBAR: %v", err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "500 ") {
		t.Fatalf("unknown command response: %s", line)
	}

	// QUIT
	if _, err := c.Write([]byte("QUIT\r\n")); err != nil {
		t.Fatalf("write QUIT: %v", err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "205 ") {
		t.Fatalf("QUIT response: %s", line)
	}
}

func TestServer_Auth(t *testing.T) {
	dir := t.TempDir()
	manifest := createTestRelease(t, dir)

	srv, err := NewServer(ServerConfig{
		ReleaseDir: dir,
		Manifest:   manifest,
		Users:      map[string]string{"alice": "secret123"},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	reader := bufio.NewReader(c)
	_, _ = reader.ReadString('\n') // greeting

	// STAT before auth -> 480
	if _, err := c.Write([]byte("STAT <single-part-1@localnntp.test>\r\n")); err != nil {
		t.Fatalf("write STAT: %v", err)
	}
	line, _ := reader.ReadString('\n')
	if !strings.HasPrefix(line, "480 ") {
		t.Fatalf("STAT before auth expected 480, got %s", line)
	}

	// AUTHINFO USER unknown -> 481
	if _, err := c.Write([]byte("AUTHINFO USER bob\r\n")); err != nil {
		t.Fatalf("write USER: %v", err)
	}
	line, _ = reader.ReadString('\n')
	if !strings.HasPrefix(line, "481 ") {
		t.Fatalf("AUTHINFO USER unknown expected 481, got %s", line)
	}

	// AUTHINFO USER alice -> 381
	if _, err := c.Write([]byte("AUTHINFO USER alice\r\n")); err != nil {
		t.Fatalf("write USER: %v", err)
	}
	line, _ = reader.ReadString('\n')
	if !strings.HasPrefix(line, "381 ") {
		t.Fatalf("AUTHINFO USER alice expected 381, got %s", line)
	}

	// AUTHINFO PASS wrong -> 481
	if _, err := c.Write([]byte("AUTHINFO PASS wrong\r\n")); err != nil {
		t.Fatalf("write PASS: %v", err)
	}
	line, _ = reader.ReadString('\n')
	if !strings.HasPrefix(line, "481 ") {
		t.Fatalf("AUTHINFO PASS wrong expected 481, got %s", line)
	}

	// Re-authenticate correctly
	if _, err := c.Write([]byte("AUTHINFO USER alice\r\n")); err != nil {
		t.Fatalf("write USER: %v", err)
	}
	_, _ = reader.ReadString('\n')
	if _, err := c.Write([]byte("AUTHINFO PASS secret123\r\n")); err != nil {
		t.Fatalf("write PASS: %v", err)
	}
	line, _ = reader.ReadString('\n')
	if !strings.HasPrefix(line, "281 ") {
		t.Fatalf("AUTHINFO PASS secret123 expected 281, got %s", line)
	}

	// STAT after auth -> 223
	if _, err := c.Write([]byte("STAT <single-part-1@localnntp.test>\r\n")); err != nil {
		t.Fatalf("write STAT: %v", err)
	}
	line, _ = reader.ReadString('\n')
	if !strings.HasPrefix(line, "223 ") {
		t.Fatalf("STAT after auth expected 223, got %s", line)
	}
}

func TestServer_Faults(t *testing.T) {
	dir := t.TempDir()
	manifest := createTestRelease(t, dir)

	srv, err := NewServer(ServerConfig{
		ReleaseDir: dir,
		Manifest:   manifest,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	ctx := t.Context()

	// 1. FaultDrop
	t.Run("FaultDrop", func(t *testing.T) {
		srv.Faults().ClearRules()
		srv.Faults().AddRule(FaultRule{
			Action:     FaultDrop,
			MessageIDs: []string{"single-part-1@localnntp.test"},
		})

		conn := dialNNTP(t, srv, "", "")
		_, err := conn.Fetch(ctx, "single-part-1@localnntp.test")
		if !errors.Is(err, nntp.ErrNoArticle) {
			t.Fatalf("expected ErrNoArticle for dropped article, got %v", err)
		}
		stats := srv.Stats()
		if stats.FaultsFiredTotal != 1 {
			t.Errorf("FaultsFiredTotal = %d, want 1", stats.FaultsFiredTotal)
		}
	})

	// 2. FaultCorruptPayload
	t.Run("FaultCorruptPayload", func(t *testing.T) {
		srv.Faults().ClearRules()
		srv.Faults().AddRule(FaultRule{
			Action:       FaultCorruptPayload,
			MessageIDs:   []string{"single-part-1@localnntp.test"},
			CorruptBytes: 16,
		})

		conn := dialNNTP(t, srv, "", "")
		body, err := conn.Fetch(ctx, "single-part-1@localnntp.test")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		art, err := decoder.DecodeArticle(body)
		if err != nil {
			t.Fatalf("DecodeArticle should succeed on corrupt-payload because CRC trailer matches, got %v", err)
		}
		orig := []byte("hello single part release payload for testing 1234567890!")
		if bytes.Equal(art.Data, orig) {
			t.Fatalf("expected corrupted body, but got original payload")
		}
		stats := srv.Stats()
		if stats.ServedClean["single-part-1@localnntp.test"] != 0 {
			t.Errorf("served clean should not count corrupt payload")
		}
	})

	// 3. FaultCorruptWire
	t.Run("FaultCorruptWire", func(t *testing.T) {
		srv.Faults().ClearRules()
		srv.Faults().AddRule(FaultRule{
			Action:       FaultCorruptWire,
			MessageIDs:   []string{"single-part-1@localnntp.test"},
			CorruptBytes: 8,
		})

		conn := dialNNTP(t, srv, "", "")
		body, err := conn.Fetch(ctx, "single-part-1@localnntp.test")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		_, err = decoder.DecodeArticle(body)
		if !errors.Is(err, decoder.ErrCRCMismatch) {
			t.Fatalf("expected ErrCRCMismatch, got %v", err)
		}
	})

	// 4. FaultDropMidBody
	t.Run("FaultDropMidBody", func(t *testing.T) {
		srv.Faults().ClearRules()
		srv.Faults().AddRule(FaultRule{
			Action:     FaultDropMidBody,
			MessageIDs: []string{"single-part-1@localnntp.test"},
		})

		c, err := net.Dial("tcp", srv.Addr())
		if err != nil {
			t.Fatalf("net.Dial: %v", err)
		}
		defer func() { _ = c.Close() }()

		reader := bufio.NewReader(c)
		_, _ = reader.ReadString('\n') // greeting

		if _, err := c.Write([]byte("BODY <single-part-1@localnntp.test>\r\n")); err != nil {
			t.Fatalf("write BODY: %v", err)
		}
		statusLine, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(statusLine, "222 ") {
			t.Fatalf("BODY status: %s (err %v)", statusLine, err)
		}

		// Read remainder until EOF (server should close mid-body)
		rest, _ := io.ReadAll(reader)
		if strings.HasSuffix(string(rest), "\r\n.\r\n") {
			t.Fatalf("drop mid body unexpectedly sent valid terminator")
		}
	})

	// 5. FaultStall
	t.Run("FaultStall", func(t *testing.T) {
		srv.Faults().ClearRules()
		srv.Faults().AddRule(FaultRule{
			Action:       FaultStall,
			MessageIDs:   []string{"single-part-1@localnntp.test"},
			TimeoutAfter: 50 * time.Millisecond,
		})

		c, err := net.Dial("tcp", srv.Addr())
		if err != nil {
			t.Fatalf("net.Dial: %v", err)
		}
		defer func() { _ = c.Close() }()

		reader := bufio.NewReader(c)
		_, _ = reader.ReadString('\n') // greeting

		if _, err := c.Write([]byte("BODY <single-part-1@localnntp.test>\r\n")); err != nil {
			t.Fatalf("write BODY: %v", err)
		}
		statusLine, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(statusLine, "222 ") {
			t.Fatalf("BODY status: %s (err %v)", statusLine, err)
		}

		start := time.Now()
		_, _ = io.ReadAll(reader) // should unblock when stall timer fires and closes connection
		elapsed := time.Since(start)
		if elapsed < 40*time.Millisecond {
			t.Fatalf("stall finished too quickly: %v", elapsed)
		}
	})
}

func TestServer_Snapshots(t *testing.T) {
	dir := t.TempDir()
	manifest := createTestRelease(t, dir)

	srv, err := NewServer(ServerConfig{
		ReleaseDir: dir,
		Manifest:   manifest,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	conn := dialNNTP(t, srv, "", "")
	ctx := t.Context()

	_, err = conn.Fetch(ctx, "single-part-1@localnntp.test")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	snap := srv.TakeSnapshot("snap-1", []string{"id-1", "id-2"})
	if snap.Name != "snap-1" {
		t.Errorf("snap.Name = %q, want snap-1", snap.Name)
	}
	if snap.ServedClean["single-part-1@localnntp.test"] != 1 {
		t.Errorf("snap.ServedClean count = %d, want 1", snap.ServedClean["single-part-1@localnntp.test"])
	}
	if len(snap.RecordedIDs) != 2 {
		t.Errorf("len(snap.RecordedIDs) = %d, want 2", len(snap.RecordedIDs))
	}

	got, ok := srv.GetSnapshot("snap-1")
	if !ok {
		t.Fatalf("GetSnapshot snap-1 failed")
	}
	if got.Name != "snap-1" {
		t.Errorf("got.Name = %q, want snap-1", got.Name)
	}

	_, ok = srv.GetSnapshot("non-existent")
	if ok {
		t.Errorf("expected non-existent snapshot to return ok=false")
	}
}

func TestControl_API(t *testing.T) {
	dir := t.TempDir()
	manifest := createTestRelease(t, dir)

	srv, err := NewServer(ServerConfig{
		ReleaseDir: dir,
		Manifest:   manifest,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	h := NewControlHandler(srv)
	ts := httptest.NewServer(h)
	defer ts.Close()

	client := ts.Client()

	// GET /stats
	resp, err := client.Get(ts.URL + "/stats")
	if err != nil {
		t.Fatalf("GET /stats: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /stats status: %d", resp.StatusCode)
	}
	var stats ServerStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	_ = resp.Body.Close()

	// POST /faults (spec)
	specBody := strings.NewReader(`{"spec": "drop:single.bin:1"}`)
	resp, err = client.Post(ts.URL+"/faults", "application/json", specBody)
	if err != nil {
		t.Fatalf("POST /faults spec: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /faults spec status: %d", resp.StatusCode)
	}
	var rules []FaultRule
	if err := json.NewDecoder(resp.Body).Decode(&rules); err != nil {
		t.Fatalf("decode rules: %v", err)
	}
	_ = resp.Body.Close()
	if len(rules) != 1 || rules[0].Action != FaultDrop {
		t.Fatalf("unexpected rules after spec: %v", rules)
	}

	// POST /faults (JSON rule)
	ruleBody := strings.NewReader(`{"action": "corrupt-wire", "file_match": "multi.bin", "corrupt_bytes": 10}`)
	resp, err = client.Post(ts.URL+"/faults", "application/json", ruleBody)
	if err != nil {
		t.Fatalf("POST /faults json: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /faults json status: %d", resp.StatusCode)
	}
	rules = nil
	if err := json.NewDecoder(resp.Body).Decode(&rules); err != nil {
		t.Fatalf("decode rules: %v", err)
	}
	_ = resp.Body.Close()
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}

	// POST /faults (invalid JSON / spec)
	badBody := strings.NewReader(`{"spec": "unknown-action:abc"}`)
	resp, err = client.Post(ts.URL+"/faults", "application/json", badBody)
	if err != nil {
		t.Fatalf("POST /faults bad: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad spec, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// DELETE /faults
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/faults", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("DELETE /faults: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /faults status: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if len(srv.Faults().Rules()) != 0 {
		t.Fatalf("expected 0 rules after DELETE")
	}

	// POST /rate
	rateBody := strings.NewReader(`{"rate_mbps": 50.0}`)
	resp, err = client.Post(ts.URL+"/rate", "application/json", rateBody)
	if err != nil {
		t.Fatalf("POST /rate: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /rate status: %d", resp.StatusCode)
	}
	var rateResp struct {
		RateMBps float64 `json:"rate_mbps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rateResp); err != nil {
		t.Fatalf("decode rate resp: %v", err)
	}
	_ = resp.Body.Close()
	if rateResp.RateMBps != 50.0 || srv.Limiter().RateMBps() != 50.0 {
		t.Fatalf("rate mismatch: %f", rateResp.RateMBps)
	}

	// POST /snapshot
	snapReq := strings.NewReader(`{"name": "snap-post", "recorded_ids": ["id-a"]}`)
	resp, err = client.Post(ts.URL+"/snapshot", "application/json", snapReq)
	if err != nil {
		t.Fatalf("POST /snapshot: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /snapshot status: %d", resp.StatusCode)
	}
	var snapResp Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapResp); err != nil {
		t.Fatalf("decode snap: %v", err)
	}
	_ = resp.Body.Close()
	if snapResp.Name != "snap-post" {
		t.Fatalf("snap name mismatch: %s", snapResp.Name)
	}

	// GET /snapshot?name=snap-post
	resp, err = client.Get(ts.URL + "/snapshot?name=snap-post")
	if err != nil {
		t.Fatalf("GET /snapshot: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /snapshot status: %d", resp.StatusCode)
	}
	var snapGet Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapGet); err != nil {
		t.Fatalf("decode snap get: %v", err)
	}
	_ = resp.Body.Close()
	if snapGet.Name != "snap-post" {
		t.Fatalf("snap name: %s", snapGet.Name)
	}

	// GET /snapshot?name=missing -> 404
	resp, err = client.Get(ts.URL + "/snapshot?name=missing")
	if err != nil {
		t.Fatalf("GET missing snapshot: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// GET /snapshot without name -> 400
	resp, err = client.Get(ts.URL + "/snapshot")
	if err != nil {
		t.Fatalf("GET /snapshot without name: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /snapshot with empty name -> 400
	resp, err = client.Post(ts.URL+"/snapshot", "application/json", strings.NewReader(`{"name": ""}`))
	if err != nil {
		t.Fatalf("POST /snapshot empty: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty snapshot name, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /snapshot?name=snap-bad with malformed JSON -> 400
	resp, err = client.Post(ts.URL+"/snapshot?name=snap-bad", "application/json", strings.NewReader(`{invalid`))
	if err != nil {
		t.Fatalf("POST /snapshot malformed: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed snapshot json, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if _, ok := srv.GetSnapshot("snap-bad"); ok {
		t.Fatalf("expected malformed POST /snapshot not to store snapshot")
	}

	// POST /rate with negative rate -> 400
	resp, err = client.Post(ts.URL+"/rate", "application/json", strings.NewReader(`{"rate_mbps": -1.0}`))
	if err != nil {
		t.Fatalf("POST /rate negative: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative rate, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /rate with malformed JSON -> 400
	resp, err = client.Post(ts.URL+"/rate", "application/json", strings.NewReader(`{invalid`))
	if err != nil {
		t.Fatalf("POST /rate malformed: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed json, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /faults with malformed JSON -> 400
	resp, err = client.Post(ts.URL+"/faults", "application/json", strings.NewReader(`{invalid`))
	if err != nil {
		t.Fatalf("POST /faults malformed: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed json, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /faults with empty spec -> 400
	resp, err = client.Post(ts.URL+"/faults", "application/json", strings.NewReader(`{"spec": ""}`))
	if err != nil {
		t.Fatalf("POST /faults empty spec: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty spec, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /faults (missing action -> 400)
	resp, err = client.Post(ts.URL+"/faults", "application/json", strings.NewReader(`{"file_match": "test"}`))
	if err != nil {
		t.Fatalf("POST /faults missing action: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing action, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /faults (unknown action -> 400)
	resp, err = client.Post(ts.URL+"/faults", "application/json", strings.NewReader(`{"action": "typo", "file_match": "test"}`))
	if err != nil {
		t.Fatalf("POST /faults unknown action: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown action, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /faults (no selector -> 400)
	resp, err = client.Post(ts.URL+"/faults", "application/json", strings.NewReader(`{"action": "drop"}`))
	if err != nil {
		t.Fatalf("POST /faults no selector: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for no selector, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /faults (structured JSON with string timeout_after -> 200)
	resp, err = client.Post(ts.URL+"/faults", "application/json", strings.NewReader(`{"action": "stall", "file_match": "single.bin", "timeout_after": "250ms", "max_fires": 1}`))
	if err != nil {
		t.Fatalf("POST /faults string timeout_after: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for string timeout_after, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /faults (structured JSON with integer nanoseconds timeout_after -> 200)
	resp, err = client.Post(ts.URL+"/faults", "application/json", strings.NewReader(`{"action": "stall", "file_match": "single.bin", "timeout_after": 50000000}`))
	if err != nil {
		t.Fatalf("POST /faults int timeout_after: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for int timeout_after, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// POST /faults (structured JSON with empty string and null timeout_after -> 200)
	for _, bodyStr := range []string{
		`{"action": "drop", "file_match": "single.bin", "timeout_after": ""}`,
		`{"action": "drop", "file_match": "single.bin", "timeout_after": null}`,
	} {
		resp, err = client.Post(ts.URL+"/faults", "application/json", strings.NewReader(bodyStr))
		if err != nil {
			t.Fatalf("POST /faults null/empty timeout_after: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	// POST /faults (invalid duration string and invalid duration type -> 400)
	for _, badDur := range []string{
		`{"action": "stall", "file_match": "single.bin", "timeout_after": "not-a-dur"}`,
		`{"action": "stall", "file_match": "single.bin", "timeout_after": true}`,
		`{"action": "drop", "file_match": "single.bin", "unknown_field": 1}`,
	} {
		resp, err = client.Post(ts.URL+"/faults", "application/json", strings.NewReader(badDur))
		if err != nil {
			t.Fatalf("POST /faults bad duration: %v", err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d", badDur, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestNewServer_Validation(t *testing.T) {
	_, err := NewServer(ServerConfig{ReleaseDir: ""})
	if err == nil {
		t.Errorf("expected error for nil manifest")
	}

	manifest := &ReleaseManifest{Name: "test"}
	_, err = NewServer(ServerConfig{Manifest: manifest, ReleaseDir: ""})
	if err == nil {
		t.Errorf("expected error for empty release dir")
	}
}

func TestServer_NNTPProtocol_Edges(t *testing.T) {
	dir := t.TempDir()
	manifest := createTestRelease(t, dir)

	srv, err := NewServer(ServerConfig{
		ReleaseDir: dir,
		Manifest:   manifest,
		Users:      map[string]string{"user": "pass"},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	reader := bufio.NewReader(c)
	_, _ = reader.ReadString('\n') // greeting

	// Empty line
	if _, err := c.Write([]byte("\r\n")); err != nil {
		t.Fatalf("write empty: %v", err)
	}

	// GROUP with no group arg
	if _, err := c.Write([]byte("GROUP\r\n")); err != nil {
		t.Fatalf("write GROUP: %v", err)
	}
	line, _ := reader.ReadString('\n')
	if !strings.HasPrefix(line, "211 ") {
		t.Errorf("expected 211 for GROUP, got %s", line)
	}

	// AUTHINFO short (<3 parts)
	if _, err := c.Write([]byte("AUTHINFO FOO\r\n")); err != nil {
		t.Fatalf("write short authinfo: %v", err)
	}
	line, _ = reader.ReadString('\n')
	if !strings.HasPrefix(line, "500 ") {
		t.Errorf("expected 500 for short authinfo, got %s", line)
	}

	// AUTHINFO unknown subcmd
	if _, err := c.Write([]byte("AUTHINFO UNKNOWN val\r\n")); err != nil {
		t.Fatalf("write unknown authinfo: %v", err)
	}
	line, _ = reader.ReadString('\n')
	if !strings.HasPrefix(line, "500 ") {
		t.Errorf("expected 500 for unknown authinfo, got %s", line)
	}

	// AUTHINFO PASS without user
	if _, err := c.Write([]byte("AUTHINFO PASS pass\r\n")); err != nil {
		t.Fatalf("write pass without user: %v", err)
	}
	line, _ = reader.ReadString('\n')
	if !strings.HasPrefix(line, "481 ") {
		t.Errorf("expected 481 for pass without user, got %s", line)
	}

	// Authenticate to test command length checks
	if _, err := c.Write([]byte("AUTHINFO USER user\r\n")); err != nil {
		t.Fatalf("write user: %v", err)
	}
	_, _ = reader.ReadString('\n')
	if _, err := c.Write([]byte("AUTHINFO PASS pass\r\n")); err != nil {
		t.Fatalf("write pass: %v", err)
	}
	_, _ = reader.ReadString('\n')

	// STAT with no arg
	if _, err := c.Write([]byte("STAT\r\n")); err != nil {
		t.Fatalf("write short stat: %v", err)
	}
	line, _ = reader.ReadString('\n')
	if !strings.HasPrefix(line, "500 ") {
		t.Errorf("expected 500 for short STAT, got %s", line)
	}

	// BODY with no arg
	if _, err := c.Write([]byte("BODY\r\n")); err != nil {
		t.Fatalf("write short body: %v", err)
	}
	line, _ = reader.ReadString('\n')
	if !strings.HasPrefix(line, "500 ") {
		t.Errorf("expected 500 for short BODY, got %s", line)
	}

	// ARTICLE with no arg
	if _, err := c.Write([]byte("ARTICLE\r\n")); err != nil {
		t.Fatalf("write short article: %v", err)
	}
	line, _ = reader.ReadString('\n')
	if !strings.HasPrefix(line, "500 ") {
		t.Errorf("expected 500 for short ARTICLE, got %s", line)
	}
}

type errFailingWriter struct{}

func (e *errFailingWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("simulated write error")
}

func TestServer_SendAndDotStuff_Unit(t *testing.T) {
	srv := &Server{limiter: NewRateLimiter(0)}
	errW := &errFailingWriter{}
	// Small buffer so Fprintf fails immediately and hits `return err` branch
	bwSmall := bufio.NewWriterSize(errW, 2)
	if err := srv.send(bwSmall, "test line that exceeds small buffer"); err == nil {
		t.Errorf("expected error from small buffer Fprintf")
	}

	// Large buffer so Fprintf succeeds, but Flush fails
	bwLarge := bufio.NewWriterSize(errW, 4096)
	if err := srv.send(bwLarge, "test"); err == nil {
		t.Errorf("expected error from failing writer flush")
	}

	// Immediate write failure for writeRateLimited
	if _, err := srv.writeRateLimited(context.Background(), bwSmall, []byte("body that exceeds small buffer")); err == nil {
		t.Errorf("expected error from writeRateLimited with failing writer")
	}

	// Successful FormatDotStuffedWire + writeRateLimited with leading dot and line without newline
	var buf bytes.Buffer
	goodBW := bufio.NewWriter(&buf)
	raw := []byte(".dotline\r\nsecond line\nthird-no-newline")
	wire := FormatDotStuffedWire(raw)
	if _, err := srv.writeRateLimited(context.Background(), goodBW, wire); err != nil {
		t.Fatalf("writeRateLimited: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "..dotline\r\n") {
		t.Errorf("expected dot stuffing, got: %q", got)
	}
	if !strings.HasSuffix(got, ".\r\n") {
		t.Errorf("expected trailing terminator, got: %q", got)
	}
}

func TestServer_ArticlesPack_FastPath(t *testing.T) {
	dir := t.TempDir()
	manifest := createTestRelease(t, dir)

	// Write articles.pack
	packPath := filepath.Join(dir, "articles.pack")
	if err := WriteArticlesPack(packPath, filepath.Join(dir, "files"), manifest.Files); err != nil {
		t.Fatalf("WriteArticlesPack: %v", err)
	}

	srv, err := NewServer(ServerConfig{
		ReleaseDir: dir,
		Manifest:   manifest,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.releases[0].packFile == nil {
		t.Fatalf("expected packFile to be opened")
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	conn := dialNNTP(t, srv, "", "")
	ctx := t.Context()

	// 1. Fetch single part via packfile fast path
	body, err := conn.Fetch(ctx, "single-part-1@localnntp.test")
	if err != nil {
		t.Fatalf("Fetch single fast path: %v", err)
	}
	art, err := decoder.DecodeArticle(body)
	if err != nil {
		t.Fatalf("DecodeArticle single: %v", err)
	}
	wantSingle := []byte("hello single part release payload for testing 1234567890!")
	if !bytes.Equal(art.Data, wantSingle) {
		t.Fatalf("single body mismatch: got %q, want %q", art.Data, wantSingle)
	}

	// 2. Fetch multi parts via packfile fast path
	body1, err := conn.Fetch(ctx, "multi-part-1@localnntp.test")
	if err != nil {
		t.Fatalf("Fetch multi 1 fast path: %v", err)
	}
	art1, err := decoder.DecodeArticle(body1)
	if err != nil || len(art1.Data) != 800 {
		t.Fatalf("DecodeArticle multi 1 failed: %v, len %d", err, len(art1.Data))
	}

	// 2b. Test ARTICLE command over fast path
	cDirect, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer func() { _ = cDirect.Close() }()
	rDirect := bufio.NewReader(cDirect)
	_, _ = rDirect.ReadString('\n') // greeting
	if _, err := cDirect.Write([]byte("ARTICLE <single-part-1@localnntp.test>\r\n")); err != nil {
		t.Fatalf("write ARTICLE: %v", err)
	}
	artLine, err := rDirect.ReadString('\n')
	if err != nil || !strings.HasPrefix(artLine, "220 ") {
		t.Fatalf("ARTICLE response: %s", artLine)
	}
	for {
		l, err := rDirect.ReadString('\n')
		if err != nil {
			t.Fatalf("read ARTICLE body: %v", err)
		}
		if strings.TrimRight(l, "\r\n") == "." {
			break
		}
	}

	stats := srv.Stats()
	if stats.ServedCleanTotal != 3 {
		t.Errorf("ServedCleanTotal = %d, want 3", stats.ServedCleanTotal)
	}

	// 3. Add fault: corrupt-payload overrides fast path
	srv.Faults().AddRule(FaultRule{
		Action:     FaultCorruptPayload,
		MessageIDs: []string{"single-part-1@localnntp.test"},
	})
	bodyCorrupt, err := conn.Fetch(ctx, "single-part-1@localnntp.test")
	if err != nil {
		t.Fatalf("Fetch corrupt single: %v", err)
	}
	artCorrupt, err := decoder.DecodeArticle(bodyCorrupt)
	if err != nil {
		t.Fatalf("expected valid decode with corrupt payload: %v", err)
	}
	if bytes.Equal(artCorrupt.Data, wantSingle) {
		t.Fatalf("expected corrupted payload data, but got clean")
	}

	// 4. Add fault: corrupt-wire overrides fast path
	srv.Faults().AddRule(FaultRule{
		Action:     FaultCorruptWire,
		MessageIDs: []string{"multi-part-2@localnntp.test"},
	})
	bodyWire, err := conn.Fetch(ctx, "multi-part-2@localnntp.test")
	if err != nil {
		t.Fatalf("Fetch corrupt-wire: %v", err)
	}
	_, err = decoder.DecodeArticle(bodyWire)
	if !errors.Is(err, decoder.ErrCRCMismatch) {
		t.Fatalf("expected ErrCRCMismatch for corrupt-wire over packfile, got: %v", err)
	}

	stats = srv.Stats()
	if stats.FaultsFiredTotal != 2 {
		t.Errorf("FaultsFiredTotal = %d, want 2", stats.FaultsFiredTotal)
	}

	// 5. Test missing file on disk when falling back to slow path
	manifestMissing := createTestRelease(t, dir)
	manifestMissing.Files = append(manifestMissing.Files, ReleaseFile{
		FileIndex:   2,
		DiskName:    "missing_on_disk.bin",
		NZBFilename: "missing.bin",
		TotalSize:   100,
		Segments: []SegmentMeta{
			{
				MessageID: "missing-disk@localnntp.test",
				PartNum:   1,
				Length:    100,
			},
		},
	})
	srvMissing, err := NewServer(ServerConfig{
		ReleaseDir: dir,
		Manifest:   manifestMissing,
	})
	if err != nil {
		t.Fatalf("NewServer srvMissing: %v", err)
	}
	if err := srvMissing.Start(); err != nil {
		t.Fatalf("Start srvMissing: %v", err)
	}
	defer func() { _ = srvMissing.Close() }()
	connMissing := dialNNTP(t, srvMissing, "", "")
	_, err = connMissing.Fetch(ctx, "missing-disk@localnntp.test")
	if !errors.Is(err, nntp.ErrNoArticle) {
		t.Fatalf("expected ErrNoArticle for missing disk file, got: %v", err)
	}
}

func TestServer_StatDoesNotConsumeBodyFaultAndAcceptBackoff(t *testing.T) {
	dir := t.TempDir()
	manifest := createTestRelease(t, dir)

	srv, err := NewServer(ServerConfig{
		ReleaseDir: dir,
		Manifest:   manifest,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	// Rule: max_fires=1 corrupt-payload on single-part-1.
	// STAT must NOT consume max_fires=1.
	srv.Faults().AddRule(FaultRule{
		Action:     FaultCorruptPayload,
		MessageIDs: []string{"single-part-1@localnntp.test"},
		MaxFires:   1,
	})

	conn := dialNNTP(t, srv, "", "")
	ctx := t.Context()

	if err := conn.Stat(ctx, "single-part-1@localnntp.test"); err != nil {
		t.Fatalf("Stat before BODY failed: %v", err)
	}
	if srv.Faults().Rules()[0].Fired != 0 {
		t.Fatalf("STAT should not consume body-only fault budget, Fired=%d", srv.Faults().Rules()[0].Fired)
	}

	// Now BODY should trigger the corrupt-payload fault once.
	body1, err := conn.Fetch(ctx, "single-part-1@localnntp.test")
	if err != nil {
		t.Fatalf("Fetch 1: %v", err)
	}
	art1, err := decoder.DecodeArticle(body1)
	if err != nil {
		t.Fatalf("DecodeArticle 1: %v", err)
	}
	wantSingle := []byte("hello single part release payload for testing 1234567890!")
	if bytes.Equal(art1.Data, wantSingle) {
		t.Fatalf("expected first BODY to be corrupted")
	}

	// Second BODY should be clean because max_fires=1 is now exhausted.
	body2, err := conn.Fetch(ctx, "single-part-1@localnntp.test")
	if err != nil {
		t.Fatalf("Fetch 2: %v", err)
	}
	art2, err := decoder.DecodeArticle(body2)
	if err != nil {
		t.Fatalf("DecodeArticle 2: %v", err)
	}
	if !bytes.Equal(art2.Data, wantSingle) {
		t.Fatalf("expected second BODY to be clean")
	}

	// Exercise waitOnAcceptError branches directly.
	var backoff time.Duration
	if !srv.waitOnAcceptError(errors.New("transient"), &backoff) {
		t.Fatalf("expected true on transient error")
	}
	if backoff != 5*time.Millisecond {
		t.Fatalf("expected 5ms initial backoff, got %v", backoff)
	}
	if !srv.waitOnAcceptError(errors.New("transient 2"), &backoff) {
		t.Fatalf("expected true on second transient error")
	}
	if backoff != 10*time.Millisecond {
		t.Fatalf("expected 10ms doubled backoff, got %v", backoff)
	}
	if srv.waitOnAcceptError(net.ErrClosed, &backoff) {
		t.Fatalf("expected false on net.ErrClosed")
	}

	// Test serveFastPath ReadAt error when packFile is truncated.
	truncatedPack, err := os.Create(filepath.Join(dir, "truncated.pack"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = truncatedPack.Close() }()
	srvTrunc := &Server{
		limiter:  NewRateLimiter(0),
		releases: []serverRelease{{packFile: truncatedPack}},
	}
	var outBuf bytes.Buffer
	bw := bufio.NewWriter(&outBuf)
	var wireBuf []byte
	loc := segmentLoc{WireOffset: 100, WireLength: 50}
	if !srvTrunc.serveFastPath(ctx, bw, "id@test", loc, false, &wireBuf) {
		t.Fatalf("expected serveFastPath to send 430 and return true on ReadAt EOF")
	}
	if !strings.Contains(outBuf.String(), "430 ") {
		t.Fatalf("expected 430 response on ReadAt error, got %q", outBuf.String())
	}

	_ = srv.Close()
	if srv.waitOnAcceptError(errors.New("after close"), &backoff) {
		t.Fatalf("expected false after server closed")
	}
}

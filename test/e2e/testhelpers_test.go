//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/app"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/nntp"
	"github.com/hobeone/sabnzbd-go/test/mocknntp"
)

const (
	e2eNewsgroup       = "alt.binaries.test"
	propagationTimeout = 30 * time.Second
	downloadTimeout    = 5 * time.Minute
)

type postedPart struct {
	messageID string
	size      int
}

type postedFile struct {
	name  string
	parts []postedPart
}

// File describes a file to post and download in E2E tests.
type File struct {
	Name     string
	Payload  []byte
	PartSize int
}

// loadConfig loads the NNTP server configuration for E2E tests.
// It checks E2E_CONFIG env var first, then falls back to the project's
// sabnzbd.yaml. Skips the test if no config is found or has no servers.
func loadConfig(t *testing.T) *config.Config {
	t.Helper()

	path := os.Getenv("E2E_CONFIG")
	if path == "" {
		candidates := []string{
			"../../sabnzbd.yaml",
			filepath.Join(os.Getenv("HOME"), "software", "sabnzbd-go", "sabnzbd.yaml"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil { //nolint:gosec // G703: test code; path is test-controlled
				path = c
				break
			}
		}
	}
	if path == "" {
		t.Skip("no config found: set E2E_CONFIG or place sabnzbd.yaml in project root")
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%q): %v", path, err)
	}
	if len(cfg.Servers) == 0 {
		t.Skip("no servers configured; cannot run E2E tests")
	}

	var enabled []config.ServerConfig
	for _, s := range cfg.Servers {
		if s.Enable {
			enabled = append(enabled, s)
		}
	}
	if len(enabled) == 0 {
		t.Skip("no enabled servers; cannot run E2E tests")
	}
	cfg.Servers = enabled

	return cfg
}

// newE2EApp builds and starts an app.Application using the real server
// config. Downloads go to a temp directory.
func newE2EApp(t *testing.T, cfg *config.Config) (a *app.Application, downloadDir string) {
	t.Helper()

	keepFiles := os.Getenv("E2E_KEEP_FILES") == "1"

	var err error
	downloadDir, err = os.MkdirTemp("", "sabnzbd-e2e-download-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	adminDir, err := os.MkdirTemp("", "sabnzbd-e2e-admin-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}

	if keepFiles {
		t.Logf("E2E_KEEP_FILES=1: leaving files in place:")
		t.Logf("  downloadDir: %s", downloadDir)
		t.Logf("  adminDir:    %s", adminDir)
	}

	appCfg := app.Config{
		DownloadDir: downloadDir,
		AdminDir:    adminDir,
		CacheLimit:  64 * 1024 * 1024, // 64 MB
		Servers:     cfg.Servers,
	}

	var opts []func(*app.Application)
	if os.Getenv("E2E_DEBUG") == "1" {
		debugLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		slog.SetDefault(debugLogger)
		opts = append(opts, app.WithLogger(debugLogger))
	}

	a, err = app.New(appCfg, opts...)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := a.Start(ctx); err != nil {
		cancel()
		t.Fatalf("app.Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := a.Shutdown(); err != nil {
			t.Logf("app.Shutdown: %v", err)
		}
		if !keepFiles {
			_ = os.RemoveAll(downloadDir)
			_ = os.RemoveAll(adminDir)
		}
	})

	return a, downloadDir
}


// e2eMessageID generates a unique message-ID for E2E test articles.
func e2eMessageID(filename string, partNum int) string {
	ts := time.Now().UnixNano()
	h := sha256.Sum256([]byte(fmt.Sprintf("e2e:%d:%s:%d", ts, filename, partNum)))
	return fmt.Sprintf("%x@e2e.sabnzbd-go.test", h[:16])
}

// postAndBuildNZB posts all articles for the given files to the server,
// then returns NZB XML and the first message-ID (for propagation checks).
func postAndBuildNZB(t *testing.T, cfg config.ServerConfig, files []File) (nzbXML []byte, firstMID string) {
	t.Helper()

	posted := make([]postedFile, 0, len(files))

	for _, f := range files {
		partSize := f.PartSize
		if partSize <= 0 {
			partSize = len(f.Payload)
		}
		parts := splitParts(f.Payload, partSize)
		totalParts := len(parts)
		totalSize := int64(len(f.Payload))

		pf := postedFile{name: f.Name}
		var offset int64

		for i, part := range parts {
			partNum := i + 1
			mid := e2eMessageID(f.Name, partNum)
			if firstMID == "" {
				firstMID = mid
			}

			var body []byte
			if totalParts == 1 {
				body = mocknntp.EncodeYEnc(f.Name, part)
			} else {
				body = mocknntp.EncodeYEncPart(f.Name, partNum, totalParts, totalSize, offset, part)
			}

			subject := fmt.Sprintf("[1/%d] - %q yEnc (%d/%d)", totalParts, f.Name, partNum, totalParts)

			t.Logf("posting %s part %d/%d (mid=%s, %d bytes)",
				f.Name, partNum, totalParts, mid, len(part))

			if err := postArticle(cfg, mid, e2eNewsgroup, subject, body); err != nil {
				if strings.Contains(err.Error(), "posting not allowed") {
					t.Skipf("server does not support POST: %v", err)
				}
				t.Fatalf("postArticle %s part %d: %v", f.Name, partNum, err)
			}

			pf.parts = append(pf.parts, postedPart{messageID: mid, size: len(part)})
			offset += int64(len(part))
		}
		posted = append(posted, pf)
	}

	return buildNZB(posted), firstMID
}

// buildNZB generates NZB XML from posted article metadata.
func buildNZB(files []postedFile) []byte {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	sb.WriteString(`<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">` + "\n")
	sb.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")

	now := time.Now().Unix()

	for _, f := range files {
		totalParts := len(f.parts)
		totalSize := 0
		for _, p := range f.parts {
			totalSize += p.size
		}

		subject := fmt.Sprintf("[1/%d] - &quot;%s&quot; yEnc (1/%d) %d",
			totalParts, f.name, totalParts, totalSize)
		fmt.Fprintf(&sb, "  <file poster=\"e2e-test@sabnzbd-go.test\" date=\"%d\" subject=\"%s\">\n", now, subject)
		fmt.Fprintf(&sb, "    <groups><group>%s</group></groups>\n", e2eNewsgroup)
		sb.WriteString("    <segments>\n")

		for i, p := range f.parts {
			fmt.Fprintf(&sb, "      <segment bytes=\"%d\" number=\"%d\">%s</segment>\n",
				p.size, i+1, p.messageID)
		}

		sb.WriteString("    </segments>\n")
		sb.WriteString("  </file>\n")
	}

	sb.WriteString("</nzb>\n")
	return []byte(sb.String())
}

// splitParts divides payload into chunks of at most partSize bytes.
func splitParts(payload []byte, partSize int) [][]byte {
	if partSize <= 0 || len(payload) <= partSize {
		return [][]byte{payload}
	}
	var parts [][]byte
	for off := 0; off < len(payload); off += partSize {
		end := off + partSize
		if end > len(payload) {
			end = len(payload)
		}
		parts = append(parts, payload[off:end])
	}
	return parts
}

// waitForArticle polls the server with STAT until the article is available
// or the timeout expires.
func waitForArticle(t *testing.T, cfg config.ServerConfig, messageID string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	interval := 2 * time.Second

	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := nntp.Dial(ctx, cfg)
		cancel()
		if err != nil {
			t.Logf("waitForArticle: dial failed (retrying): %v", err)
			time.Sleep(interval)
			continue
		}

		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		err = conn.Stat(ctx2, messageID)
		cancel2()
		_ = conn.Close() //nolint:errcheck // test polling; close error is irrelevant

		if err == nil {
			t.Logf("waitForArticle: %s available", messageID)
			return
		}
		t.Logf("waitForArticle: %s not yet available: %v", messageID, err)
		time.Sleep(interval)
	}

	t.Fatalf("waitForArticle: %s not available after %v", messageID, timeout)
}

// findFile walks dir and returns the first path whose base name matches filename.
func findFile(t *testing.T, dir, filename string) string {
	t.Helper()
	var result string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error { //nolint:errcheck // walk error on test tree is not actionable
		if err != nil || result != "" {
			return nil //nolint:nilerr // walk continues on error
		}
		if !info.IsDir() && filepath.Base(path) == filename {
			result = path
		}
		return nil
	})
	return result
}

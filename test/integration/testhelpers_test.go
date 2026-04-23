//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/app"
	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/history"
	"github.com/hobeone/sabnzbd-go/test/mocknntp"
)

// TestFile describes one file to be included in a synthetic NZB.
type TestFile struct {
	// Name is the filename as it will appear in the NZB.
	Name string
	// Payload is the raw file content to be encoded and served.
	Payload []byte
	// PartSize is the number of raw bytes per article part.
	// If 0, the whole payload is a single article.
	PartSize int
}

// articleID derives a deterministic message-ID from (filename, partNum) so
// the same inputs always produce the same ID.
func articleID(filename string, partNum int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", filename, partNum)))
	hex := fmt.Sprintf("%x", h)
	return hex[:32] + "@integration.test"
}

// SplitIntoParts divides payload into chunks of at most partSize bytes.
func SplitIntoParts(payload []byte, partSize int) [][]byte {
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

// BuildArticle returns a deterministic message-ID and the yEnc-encoded
// article body for the given part of a file.
//
// For single-part files (totalParts==1) it produces a plain yEnc body.
// For multi-part files it produces a yEnc multi-part article with the
// correct =ybegin/=ypart headers.
func BuildArticle(payload []byte, filename string, partNum, totalParts int, totalSize, offset int64) (messageID string, body []byte) {
	mid := articleID(filename, partNum)
	if totalParts == 1 {
		body = mocknntp.EncodeYEnc(filename, payload)
	} else {
		body = mocknntp.EncodeYEncPart(filename, partNum, totalParts, totalSize, offset, payload)
	}
	return mid, body
}

// BuildNZB produces minimal but well-formed NZB XML for the given files.
// It derives message-IDs from articleID so they are consistent with the
// articles registered with BuildArticle.
func BuildNZB(files []TestFile) []byte {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	sb.WriteString(`<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">` + "\n")
	sb.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")

	now := time.Now().Unix()

	for _, f := range files {
		partSize := f.PartSize
		if partSize <= 0 {
			partSize = len(f.Payload)
			if partSize == 0 {
				partSize = 1
			}
		}
		parts := SplitIntoParts(f.Payload, partSize)

		totalSize := int64(len(f.Payload))
		totalParts := len(parts)

		// Compute total bytes declared in NZB (sum of encoded part sizes is not
		// the same as actual payload; NZB bytes attributes are best-effort from
		// the poster). We use the raw payload length per part as the declared size.
		totalDeclared := 0
		for _, p := range parts {
			totalDeclared += len(p)
		}

		// Build the subject with XML-escaped double-quotes around the filename (NZB convention).
		// Real NZB files use &quot; to embed double-quotes inside XML attribute values.
		subject := fmt.Sprintf("[1/%d] - &quot;%s&quot; yEnc (1/%d) %d", totalParts, f.Name, totalParts, totalDeclared)
		fmt.Fprintf(&sb, "  <file poster=\"poster@integration.test\" date=\"%d\" subject=\"%s\">\n", now, subject)
		sb.WriteString("    <groups><group>alt.test</group></groups>\n")
		sb.WriteString("    <segments>\n")

		var offset int64
		for i, part := range parts {
			partNum := i + 1
			mid := articleID(f.Name, partNum)
			// Declare the raw payload bytes for each segment (not encoded size).
			fmt.Fprintf(&sb, "      <segment bytes=\"%d\" number=\"%d\">%s</segment>\n",
				len(part), partNum, mid)
			_ = offset
			offset += int64(len(part))
		}
		_ = totalSize

		sb.WriteString("    </segments>\n")
		sb.WriteString("  </file>\n")
	}

	sb.WriteString("</nzb>\n")
	return []byte(sb.String())
}

// RegisterArticles registers all articles for the given files with the mock
// NNTP server. It mirrors the encoding used by BuildNZB.
func RegisterArticles(srv *mocknntp.Server, files []TestFile) {
	for _, f := range files {
		partSize := f.PartSize
		if partSize <= 0 {
			partSize = len(f.Payload)
			if partSize == 0 {
				partSize = 1
			}
		}
		parts := SplitIntoParts(f.Payload, partSize)
		totalParts := len(parts)
		totalSize := int64(len(f.Payload))

		var offset int64
		for i, part := range parts {
			partNum := i + 1
			mid, body := BuildArticle(part, f.Name, partNum, totalParts, totalSize, offset)
			srv.AddArticle(mid, body)
			offset += int64(len(part))
		}
	}
}

// buildAppConfig creates an app.Config pointing at the given mock NNTP address
// and download directory.
func buildAppConfig(mockAddr, downloadDir string) app.Config {
	host := mockAddr
	port := 119
	if idx := strings.LastIndex(mockAddr, ":"); idx >= 0 {
		host = mockAddr[:idx]
		fmt.Sscanf(mockAddr[idx+1:], "%d", &port) //nolint:errcheck // best-effort port parse in tests
	}
	return app.Config{
		DownloadDir: downloadDir,
		CompleteDir: downloadDir,
		AdminDir:    downloadDir,
		CacheLimit:  0,
		Servers: []config.ServerConfig{
			{
				Name:        "test",
				Host:        host,
				Port:        port,
				Connections: 2,
				Enable:      true,
			},
		},
	}
}

// NewTestApp builds and starts an *app.Application pointed at the mock NNTP
// server at mockAddr. DownloadDir and AdminDir are both created under
// t.TempDir(). The app is registered for cleanup via t.Cleanup.
func NewTestApp(t *testing.T, mockAddr string) *app.Application {
	t.Helper()
	dir := t.TempDir()
	cfg := buildAppConfig(mockAddr, dir)

	db, err := history.Open(filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)

	a, err := app.New(cfg, repo)
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
		db.Close()
	})

	return a
}

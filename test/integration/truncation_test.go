//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/test/mocknntp"
)

// TestDownload_FileSizeMatchesPayload is a regression test for the
// pre-allocation truncation bug. When a file is pre-allocated using the
// NZB-declared segment sizes (which are yEnc *encoded* sizes, ~2% larger
// than the decoded payload), the assembled file must be truncated to the
// actual decoded size. Without truncation, the file has trailing zero
// bytes from the over-sized pre-allocation, causing par2 to report the
// file as "damaged" despite 100% download health.
//
// This test uses realistic NZB segment byte values (encoded sizes) to
// reproduce the bug that the standard BuildNZB helper doesn't trigger
// (because it uses decoded sizes).
func TestDownload_FileSizeMatchesPayload(t *testing.T) {
	t.Parallel()

	// Create a payload with values that exercise yEnc escaping.
	// Bytes 0x00, 0x0a, 0x0d, and 0x3d all require escaping, making the
	// encoded size ~3-6% larger than the decoded size.
	payload := make([]byte, 32*1024) // 32 KB
	for i := range payload {
		payload[i] = byte(i % 256) // every byte value appears
	}

	const partSize = 8 * 1024 // 4 parts of 8 KB each
	parts := SplitIntoParts(payload, partSize)
	totalParts := len(parts)
	totalSize := int64(len(payload))

	// Register articles with the mock server.
	srv := mocknntp.NewServer(mocknntp.Config{})
	metas := make([]encodedArticle, 0, totalParts)

	var offset int64
	for i, part := range parts {
		partNum := i + 1
		mid, body := BuildArticle(part, "truncation-test.bin", partNum, totalParts, totalSize, offset)
		srv.AddArticle(mid, body)
		metas = append(metas, encodedArticle{messageID: mid, encodedSize: len(body)})
		offset += int64(len(part))
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("mock server start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// Build NZB XML with ENCODED sizes in the <segment bytes="...">
	// attribute. This is what real NZB files contain — the encoded article
	// size, not the decoded payload size. This triggers the pre-allocation
	// oversize that causes the trailing-zeros bug.
	nzbXML := buildNZBWithEncodedSizes("truncation-test.bin", metas)

	// Verify the NZB parses and the declared sizes are larger than decoded.
	parsed, err := nzb.Parse(bytes.NewReader(nzbXML))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	if len(parsed.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(parsed.Files))
	}
	nzbDeclaredSize := parsed.Files[0].Bytes
	t.Logf("payload size:      %d bytes", len(payload))
	t.Logf("NZB declared size: %d bytes (encoded, %+.1f%%)",
		nzbDeclaredSize, float64(nzbDeclaredSize-int64(len(payload)))/float64(len(payload))*100)
	if nzbDeclaredSize <= int64(len(payload)) {
		t.Fatalf("NZB declared size (%d) should be larger than payload (%d) for this test to be meaningful",
			nzbDeclaredSize, len(payload))
	}

	// Set up the app and download.
	downloadDir := t.TempDir()
	a := newTestAppWithDir(t, srv.Addr(), downloadDir)

	job, err := queue.NewJob(parsed, queue.AddOptions{
		Filename: "truncation-test.nzb",
		Name:     "truncation-test",
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("queue.NewJob: %v", err)
	}
	if err := a.AddJob(t.Context(), job, nzbXML, false); err != nil {
		t.Fatalf("app.AddJob: %v", err)
	}

	// Wait for completion.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	select {
	case <-a.PostProcComplete():
		// success
	case <-ctx.Done():
		t.Fatalf("timeout waiting for PostProcComplete")
	}

	// Verify: the file must exist, match the payload exactly, and have
	// exactly len(payload) bytes — NOT the larger NZB-declared size.
	found := findFile(t, downloadDir, "truncation-test.bin")
	if found == "" {
		t.Fatalf("assembled file not found under %s", downloadDir)
	}

	data, err := os.ReadFile(found) //nolint:gosec // test code
	if err != nil {
		t.Fatalf("read assembled file: %v", err)
	}

	// Check file size (the core assertion for this regression test).
	if int64(len(data)) != int64(len(payload)) {
		t.Errorf("file size = %d, want %d (decoded payload size, not NZB-declared %d)",
			len(data), len(payload), nzbDeclaredSize)
		// Show how many trailing zeros there are.
		if int64(len(data)) > int64(len(payload)) {
			trailing := data[len(payload):]
			allZeros := true
			for _, b := range trailing {
				if b != 0 {
					allZeros = false
					break
				}
			}
			t.Errorf("  trailing %d bytes (all zeros: %v) — this is the pre-allocation truncation bug",
				len(trailing), allZeros)
		}
	}

	// Verify content integrity via SHA-256.
	wantSHA := sha256.Sum256(payload)
	gotSHA := sha256.Sum256(data)
	if !bytes.Equal(gotSHA[:], wantSHA[:]) {
		t.Errorf("SHA-256 mismatch:\n  got  %x\n  want %x", gotSHA, wantSHA)
	} else {
		t.Logf("verified truncation-test.bin: %d bytes, SHA-256 matches", len(data))
	}
}

// TestDownload_MultiPartFileSizeExact verifies that a multi-part download
// produces a file whose size matches the original payload exactly, not the
// pre-allocated size. This uses a more realistic scenario with varied part
// sizes to exercise the completion truncate across out-of-order delivery. The
// bound comes from durability.Barrier.FinalizeFile — the highest end offset
// over the file's durable runs — not from any high-water mark the
// assembler tracks, since it tracks none.
func TestDownload_MultiPartFileSizeExact(t *testing.T) {
	t.Parallel()

	// Create a 64 KB payload with deterministic content.
	payload := make([]byte, 64*1024)
	for i := range payload {
		// Use all byte values to maximize yEnc encoding overhead.
		payload[i] = byte((i*7 + 13) % 256)
	}

	const partSize = 10 * 1024 // 7 parts (6 full + 1 partial)
	parts := SplitIntoParts(payload, partSize)
	totalParts := len(parts)
	totalSize := int64(len(payload))

	srv := mocknntp.NewServer(mocknntp.Config{})
	metas := make([]encodedArticle, 0, totalParts)

	var offset int64
	for i, part := range parts {
		partNum := i + 1
		mid, body := BuildArticle(part, "multipart-exact.bin", partNum, totalParts, totalSize, offset)
		srv.AddArticle(mid, body)
		metas = append(metas, encodedArticle{messageID: mid, encodedSize: len(body)})
		offset += int64(len(part))
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("mock server start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	nzbXML := buildNZBWithEncodedSizes("multipart-exact.bin", metas)
	downloadDir := t.TempDir()
	a := newTestAppWithDir(t, srv.Addr(), downloadDir)

	parsed, err := nzb.Parse(bytes.NewReader(nzbXML))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{
		Filename: "multipart-exact.nzb",
		Name:     "multipart-exact",
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("queue.NewJob: %v", err)
	}
	if err := a.AddJob(t.Context(), job, nzbXML, false); err != nil {
		t.Fatalf("app.AddJob: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	select {
	case <-a.PostProcComplete():
	case <-ctx.Done():
		t.Fatalf("timeout waiting for PostProcComplete")
	}

	found := findFile(t, downloadDir, "multipart-exact.bin")
	if found == "" {
		t.Fatalf("assembled file not found under %s", downloadDir)
	}

	info, err := os.Stat(found)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != int64(len(payload)) {
		t.Errorf("file size = %d, want %d (exact decoded payload size)", info.Size(), len(payload))
	}

	data, err := os.ReadFile(found) //nolint:gosec // test code
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	wantSHA := sha256.Sum256(payload)
	gotSHA := sha256.Sum256(data)
	if !bytes.Equal(gotSHA[:], wantSHA[:]) {
		t.Errorf("SHA-256 mismatch:\n  got  %x\n  want %x", gotSHA, wantSHA)
	} else {
		t.Logf("verified multipart-exact.bin: %d bytes (%d parts), SHA-256 matches",
			len(data), totalParts)
	}
}

// encodedArticle holds the message-ID and encoded body size for an article,
// used by buildNZBWithEncodedSizes to create NZB XML with realistic
// (encoded) segment byte counts.
type encodedArticle struct {
	messageID   string
	encodedSize int
}

// buildNZBWithEncodedSizes creates NZB XML using the yEnc encoded article
// sizes in the <segment bytes="..."> attribute, mimicking real-world NZB
// files. The standard BuildNZB helper uses decoded payload sizes, which
// does not exercise the pre-allocation oversize path.
func buildNZBWithEncodedSizes(filename string, articles []encodedArticle) []byte {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	sb.WriteString(`<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">` + "\n")
	sb.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")

	now := time.Now().Unix()
	totalParts := len(articles)

	totalDeclared := 0
	for _, a := range articles {
		totalDeclared += a.encodedSize
	}

	subject := fmt.Sprintf("[1/%d] - &quot;%s&quot; yEnc (1/%d) %d",
		totalParts, filename, totalParts, totalDeclared)
	fmt.Fprintf(&sb, "  <file poster=\"poster@integration.test\" date=\"%d\" subject=\"%s\">\n", now, subject)
	sb.WriteString("    <groups><group>alt.test</group></groups>\n")
	sb.WriteString("    <segments>\n")

	for i, a := range articles {
		// Use the ENCODED size — this is the key difference from BuildNZB.
		fmt.Fprintf(&sb, "      <segment bytes=\"%d\" number=\"%d\">%s</segment>\n",
			a.encodedSize, i+1, a.messageID)
	}

	sb.WriteString("    </segments>\n")
	sb.WriteString("  </file>\n")
	sb.WriteString("</nzb>\n")
	return []byte(sb.String())
}

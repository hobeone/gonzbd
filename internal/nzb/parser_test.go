package nzb

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/md5" //nolint:gosec // test mirrors parser's MD5 usage
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture returns the path to a test fixture under test/fixtures/nzb at
// the module root. cwd during tests is .../internal/nzb; go up two.
func fixture(t *testing.T, name string) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(cwd, "..", "..", "test", "fixtures", "nzb", name)
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixture(t, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestParseSimple(t *testing.T) {
	src := loadFixture(t, "simple.nzb")
	got, err := Parse(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got := got.Meta["title"]; len(got) != 1 || got[0] != "Simple Test Post" {
		t.Errorf(`Meta["title"] = %v, want ["Simple Test Post"]`, got)
	}
	if got := got.Meta["category"]; len(got) != 1 || got[0] != "misc" {
		t.Errorf(`Meta["category"] = %v, want ["misc"]`, got)
	}

	if len(got.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(got.Files))
	}
	f := got.Files[0]
	if !strings.Contains(f.Subject, "test.bin") {
		t.Errorf("Subject = %q, want to contain test.bin", f.Subject)
	}
	if f.Date.Unix() != 1700000000 {
		t.Errorf("Date = %d, want 1700000000", f.Date.Unix())
	}
	if f.Bytes != 2*1048576 {
		t.Errorf("Bytes = %d, want %d", f.Bytes, 2*1048576)
	}
	if len(f.Articles) != 2 {
		t.Fatalf("len(Articles) = %d, want 2", len(f.Articles))
	}
	if f.Articles[0].Number != 1 || f.Articles[1].Number != 2 {
		t.Errorf("articles not sorted by part number: %v", f.Articles)
	}

	wantGroups := []string{"alt.binaries.test"}
	if !equalSlices(got.Groups, wantGroups) {
		t.Errorf("Groups = %v, want %v", got.Groups, wantGroups)
	}
	if got.DuplicateArticles != 0 || got.BadArticles != 0 || got.SkippedFiles != 0 {
		t.Errorf("counters = (%d,%d,%d), want (0,0,0)",
			got.DuplicateArticles, got.BadArticles, got.SkippedFiles)
	}
	if got.AvgAge.Unix() != 1700000000 {
		t.Errorf("AvgAge = %d, want 1700000000", got.AvgAge.Unix())
	}
}

func TestParseMultiFileAndMultiValueMeta(t *testing.T) {
	src := loadFixture(t, "multi_file.nzb")
	got, err := Parse(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Multi-value meta: two <meta type="category"> entries.
	wantCats := []string{"tv", "hd"}
	if !equalSlices(got.Meta["category"], wantCats) {
		t.Errorf(`Meta["category"] = %v, want %v`, got.Meta["category"], wantCats)
	}
	if got := got.Meta["password"]; len(got) != 1 || got[0] != "secret" {
		t.Errorf(`Meta["password"] = %v, want ["secret"]`, got)
	}

	if len(got.Files) != 2 {
		t.Fatalf("len(Files) = %d, want 2", len(got.Files))
	}

	// Groups union, deduped, first-seen order.
	wantGroups := []string{"alt.binaries.tv", "alt.binaries.misc"}
	if !equalSlices(got.Groups, wantGroups) {
		t.Errorf("Groups = %v, want %v", got.Groups, wantGroups)
	}

	// Avg age: (1700000100 + 1700000200) / 2 = 1700000150.
	if got.AvgAge.Unix() != 1700000150 {
		t.Errorf("AvgAge = %d, want 1700000150", got.AvgAge.Unix())
	}
}

// TestParseMalformedSegments exercises the three sanity rules:
// duplicate part numbers (same and different IDs), zero size, and
// over-max size. See fixture malformed.nzb for the exact segment list.
func TestParseMalformedSegments(t *testing.T) {
	src := loadFixture(t, "malformed.nzb")
	got, err := Parse(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got.DuplicateArticles != 1 {
		t.Errorf("DuplicateArticles = %d, want 1", got.DuplicateArticles)
	}
	if got.BadArticles != 3 {
		// File 1: bytes=0 (c4) + bytes=2^23 (c5). File 2: bytes=0 (skip-me).
		t.Errorf("BadArticles = %d, want 3", got.BadArticles)
	}
	if got.SkippedFiles != 1 {
		t.Errorf("SkippedFiles = %d, want 1", got.SkippedFiles)
	}
	if len(got.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(got.Files))
	}

	f := got.Files[0]
	if len(f.Articles) != 3 {
		t.Fatalf("len(Articles) = %d, want 3", len(f.Articles))
	}
	for i, a := range f.Articles {
		if a.Number != i+1 {
			t.Errorf("Articles[%d].Number = %d, want %d", i, a.Number, i+1)
		}
	}
	if f.Bytes != 300000 {
		t.Errorf("Bytes = %d, want 300000", f.Bytes)
	}

	// MD5 ordering parity: every structurally-complete article ID
	// contributes, in source order, even if later deduped or rejected.
	// File 1 segments in order: c3, c1, c2, c2-dup-same, c1, c4, c5.
	// File 2 segments in order: skip-me.
	wantOrder := []string{
		"c3@example.com",
		"c1@example.com",
		"c2@example.com",
		"c2-dup-same@example.com",
		"c1@example.com",
		"c4@example.com",
		"c5@example.com",
		"skip-me@example.com",
	}
	h := md5.New() //nolint:gosec // test only
	for _, id := range wantOrder {
		_, _ = h.Write([]byte(id))
	}
	var want [16]byte
	copy(want[:], h.Sum(nil))
	if got.MD5 != want {
		t.Errorf("MD5 = %x, want %x", got.MD5, want)
	}
}

func TestParseGzipEnvelope(t *testing.T) {
	plain := loadFixture(t, "simple.nzb")

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(plain); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	got, err := Parse(&gzBuf)
	if err != nil {
		t.Fatalf("Parse gzip: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(got.Files))
	}

	// Parity: decoded content MUST match the plain-file parse byte for byte.
	plainParsed, err := Parse(bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("Parse plain: %v", err)
	}
	if got.MD5 != plainParsed.MD5 {
		t.Errorf("gzip parse diverges from plain: MD5 %x vs %x", got.MD5, plainParsed.MD5)
	}
}

// TestParseBzip2Envelope builds a bzip2-framed NZB using an external
// `bzip2` binary if available, otherwise skips. compress/bzip2 in stdlib
// is decode-only, so we can't generate input inline.
func TestParseBzip2Envelope(t *testing.T) {
	bz := bzip2Encode(t, loadFixture(t, "simple.nzb"))
	if bz == nil {
		t.Skip("bzip2 command not available")
	}
	if len(bz) < 2 || bz[0] != 'B' || bz[1] != 'Z' {
		t.Fatalf("bzip2 output missing BZ magic: % x", bz[:min(4, len(bz))])
	}
	got, err := Parse(bytes.NewReader(bz))
	if err != nil {
		t.Fatalf("Parse bzip2: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(got.Files))
	}
}

// TestEnvelopeSelection verifies unwrapEnvelope picks the right branch
// for each magic sequence. Each case feeds a real, decodable body so the
// returned reader is usable.
func TestEnvelopeSelection(t *testing.T) {
	const body = "<?xml version=\"1.0\"?><nzb/>"

	tests := []struct {
		name       string
		build      func() []byte
		wantCloser bool
		// wantPassthrough is true when the returned reader should be
		// the caller's own bufio.Reader (no decompression).
		wantPassthrough bool
	}{
		{
			"plain XML",
			func() []byte { return []byte(body) },
			false,
			true,
		},
		{
			"gzip",
			func() []byte {
				var buf bytes.Buffer
				gw := gzip.NewWriter(&buf)
				_, _ = gw.Write([]byte(body))
				_ = gw.Close()
				return buf.Bytes()
			},
			true,
			false,
		},
		{
			"too short",
			func() []byte { return []byte{0x1f} },
			false,
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.build()
			br := bufio.NewReader(bytes.NewReader(raw))
			magic, _ := br.Peek(2)
			src, closer, err := unwrapEnvelope(br, magic)
			if err != nil {
				t.Fatalf("unwrapEnvelope: %v", err)
			}
			if (closer != nil) != tc.wantCloser {
				t.Errorf("closer presence = %v, want %v", closer != nil, tc.wantCloser)
			}
			if tc.wantPassthrough && src != br {
				t.Errorf("expected passthrough of bufio.Reader, got wrapper")
			}
			// Sanity-read a byte to prove the reader is live.
			one := make([]byte, 1)
			if _, err := src.Read(one); err != nil {
				t.Errorf("src.Read: %v", err)
			}
			if closer != nil {
				if err := closer(); err != nil {
					t.Errorf("closer: %v", err)
				}
			}
		})
	}
}

func TestParseRejectsUnterminatedXML(t *testing.T) {
	// Deliberately cut off mid-element.
	_, err := Parse(strings.NewReader("<nzb><file></nzb"))
	if err == nil {
		t.Fatal("expected error on unterminated XML")
	}
}

func TestParseEmptyInput(t *testing.T) {
	_, err := Parse(strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error on empty input")
	}
}

func TestParseMissingDateUsesWallClock(t *testing.T) {
	const doc = `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file subject="no date">
    <groups><group>g</group></groups>
    <segments><segment bytes="100" number="1">id@h</segment></segments>
  </file>
</nzb>`
	before := time.Now().Add(-1 * time.Second).Unix()
	got, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	after := time.Now().Add(1 * time.Second).Unix()

	if len(got.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(got.Files))
	}
	ts := got.Files[0].Date.Unix()
	if ts < before || ts > after {
		t.Errorf("Date %d not within [%d, %d] — missing date should fall back to now", ts, before, after)
	}
}

func TestParseMissingSubjectDefaultsToUnknown(t *testing.T) {
	const doc = `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file date="1700000000">
    <groups><group>g</group></groups>
    <segments><segment bytes="100" number="1">id@h</segment></segments>
  </file>
</nzb>`
	got, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Files[0].Subject != "unknown" {
		t.Errorf("Subject = %q, want %q", got.Files[0].Subject, "unknown")
	}
}

// bzip2Encode runs the system `bzip2` command to compress data. Returns
// nil when bzip2 is unavailable so the caller can t.Skip.
func bzip2Encode(t *testing.T, data []byte) []byte {
	t.Helper()
	cmd := exec.Command("bzip2", "-c")
	cmd.Stdin = bytes.NewReader(data)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) {
			return nil
		}
		t.Fatalf("bzip2 -c: %v", err)
	}
	return out.Bytes()
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

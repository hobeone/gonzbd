package nzb

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/md5" //nolint:gosec // test mirrors parser's MD5 usage
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
	if !slices.Equal(got.Groups, wantGroups) {
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
	if !slices.Equal(got.Meta["category"], wantCats) {
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
	if !slices.Equal(got.Groups, wantGroups) {
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

func TestParseISO88591(t *testing.T) {
	// "café" in Latin-1 is "caf\xE9"
	const doc = `<?xml version="1.0" encoding="iso-8859-1"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file subject="caf` + "\xE9" + `">
    <groups><group>g</group></groups>
    <segments><segment bytes="100" number="1">id@h</segment></segments>
  </file>
</nzb>`
	got, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Files[0].Subject != "café" {
		t.Errorf("Subject = %q, want %q", got.Files[0].Subject, "café")
	}
}

// TestParse_InputSizeLimit verifies that an NZB exceeding maxNZBSize is
// rejected rather than consuming unbounded memory (C5 – XML bomb).
func TestParse_InputSizeLimit(t *testing.T) {
	// Use a streaming reader that yields XML content exceeding maxNZBSize
	// without allocating the full buffer in memory.
	// Structure: prolog + <nzb><head><meta type="junk"> + N×'A' + </meta></head></nzb>
	prolog := []byte(`<?xml version="1.0"?><nzb><head><meta type="junk">`)
	epilog := []byte(`</meta></head></nzb>`)

	// The oversize reader streams: prolog, then maxNZBSize+1024 bytes of 'A', then epilog.
	r := io.MultiReader(
		bytes.NewReader(prolog),
		io.LimitReader(&infiniteAReader{}, maxNZBSize+1024),
		bytes.NewReader(epilog),
	)

	_, err := Parse(r)
	if err == nil {
		t.Fatal("expected error for NZB exceeding maxNZBSize")
	}
}

// infiniteAReader is an io.Reader that yields an endless stream of 'A' bytes.
type infiniteAReader struct{}

func (infiniteAReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'A'
	}
	return len(p), nil
}

// TestParse_ExcessiveFiles verifies that the parser rejects NZBs with
// more than maxFiles <file> elements (H2 – unbounded file count).
func TestParse_ExcessiveFiles(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><nzb>`)
	for i := range maxFiles + 1 {
		fmt.Fprintf(&b, `<file subject="f%d" date="1700000000">`+
			`<groups><group>g</group></groups>`+
			`<segments><segment bytes="100" number="1">id%d@h</segment></segments>`+
			`</file>`, i, i)
	}
	b.WriteString(`</nzb>`)

	_, err := Parse(strings.NewReader(b.String()))
	if err == nil {
		t.Fatal("expected error for excessive file count")
	}
	if !strings.Contains(err.Error(), "file count exceeds limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestParse_ExcessiveSegments verifies that the parser rejects NZBs with
// more than maxSegments total segments (H2 – unbounded segment count).
func TestParse_ExcessiveSegments(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><nzb>`)
	b.WriteString(`<file subject="big" date="1700000000">`)
	b.WriteString(`<groups><group>g</group></groups><segments>`)
	for i := range maxSegments + 1 {
		fmt.Fprintf(&b, `<segment bytes="100" number="%d">id%d@h</segment>`, i+1, i)
	}
	b.WriteString(`</segments></file></nzb>`)

	_, err := Parse(strings.NewReader(b.String()))
	if err == nil {
		t.Fatal("expected error for excessive segment count")
	}
	if !strings.Contains(err.Error(), "segment count exceeds limit") {
		t.Fatalf("unexpected error: %v", err)
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
		if _, ok := errors.AsType[*exec.Error](err); ok {
			return nil
		}
		t.Fatalf("bzip2 -c: %v", err)
	}
	return out.Bytes()
}

func TestCharsetReaderDirect(t *testing.T) {
	input := strings.NewReader("hello")

	// 1. Supported: UTF-8
	r, err := charsetReader("utf-8", input)
	if err != nil {
		t.Fatalf("charsetReader(utf-8) failed: %v", err)
	}
	if r != input {
		t.Error("expected UTF-8 reader to pass through input reader directly")
	}

	// 2. Supported: ISO-8859-1 / Latin-1
	r, err = charsetReader("iso-8859-1", input)
	if err != nil {
		t.Fatalf("charsetReader(iso-8859-1) failed: %v", err)
	}
	if r == input {
		t.Error("expected ISO-8859-1 reader to wrap the input reader")
	}

	// 3. Unsupported charset
	_, err = charsetReader("invalid-charset-123", input)
	if err == nil {
		t.Error("expected error for invalid-charset-123, got nil")
	}
}

func TestParserUnexportedHelpersDirect(t *testing.T) {
	t.Parallel()

	t.Run("convertFile", func(t *testing.T) {
		xf := xmlFile{
			Subject: "test-subject",
			Date:    "1700000000",
			Groups:  []string{"alt.binaries.test"},
			Segments: []xmlSegment{
				{Bytes: 100, Number: 1, ID: "msg1"},
				{Bytes: 200, Number: 2, ID: "msg2"},
				{Bytes: 100, Number: 0, ID: "bad4"},
				{Bytes: 0, Number: 3, ID: "bad1"},
				{Bytes: -10, Number: 4, ID: "bad2"},
				{Bytes: 1 << 24, Number: 5, ID: "bad3"},
				{Bytes: 100, Number: 1, ID: "msg1-dup"},
			},
		}

		digest := md5.New()
		file, ts, counters := convertFile(xf, time.Now(), digest, make(map[string]struct{}))
		if ts != 1700000000 {
			t.Errorf("ts = %d, want 1700000000", ts)
		}
		if file.Subject != "test-subject" {
			t.Errorf("Subject = %q, want 'test-subject'", file.Subject)
		}
		if len(file.Articles) != 2 {
			t.Errorf("len(Articles) = %d, want 2", len(file.Articles))
		}
		if counters.dupes != 1 {
			t.Errorf("counters.dupes = %d, want 1", counters.dupes)
		}
		if counters.bad != 3 {
			t.Errorf("counters.bad = %d, want 3", counters.bad)
		}
	})

	t.Run("partitionSegments", func(t *testing.T) {
		segs := []xmlSegment{
			{Bytes: 100, Number: 1, ID: "  msg1@foo  "},
			{Bytes: 200, Number: 2, ID: "msg2@foo"},
			{Bytes: 100, Number: 0, ID: "bad-number-0"},
			{Bytes: 100, Number: -1, ID: "bad-number-neg"},
			{Bytes: 100, Number: 3, ID: "   "},
			{Bytes: 0, Number: 4, ID: "bad-bytes-0"},
			{Bytes: -5, Number: 5, ID: "bad-bytes-neg"},
			{Bytes: 1 << 23, Number: 6, ID: "bad-bytes-max"},
			{Bytes: (1 << 23) - 1, Number: 7, ID: "msg7@foo"},
			{Bytes: 150, Number: 1, ID: "msg1-dup@foo"},
			{Bytes: 200, Number: 2, ID: "msg2@foo"},
		}

		digest := md5.New()
		byPart, counters := partitionSegments(segs, digest, make(map[string]struct{}))

		if len(byPart) != 3 {
			t.Fatalf("len(byPart) = %d, want 3", len(byPart))
		}
		if byPart[1].ID != "msg1@foo" || byPart[1].Bytes != 100 || byPart[1].Number != 1 {
			t.Errorf("byPart[1] = %+v, want msg1@foo (100 bytes)", byPart[1])
		}
		if byPart[2].ID != "msg2@foo" {
			t.Errorf("byPart[2] = %+v, want msg2@foo", byPart[2])
		}
		if byPart[7].ID != "msg7@foo" || byPart[7].Bytes != (1<<23)-1 {
			t.Errorf("byPart[7] = %+v, want msg7@foo", byPart[7])
		}

		if counters.dupes != 1 {
			t.Errorf("counters.dupes = %d, want 1", counters.dupes)
		}
		if counters.bad != 3 {
			t.Errorf("counters.bad = %d, want 3", counters.bad)
		}

		wantDigest := md5.New()
		for _, id := range []string{"msg1@foo", "msg2@foo", "bad-bytes-0", "bad-bytes-neg", "bad-bytes-max", "msg7@foo", "msg1-dup@foo", "msg2@foo"} {
			_, _ = wantDigest.Write([]byte(id))
		}
		if !slices.Equal(digest.Sum(nil), wantDigest.Sum(nil)) {
			t.Errorf("digest mismatch: got %x, want %x", digest.Sum(nil), wantDigest.Sum(nil))
		}
	})

	t.Run("normalizeFileStruct", func(t *testing.T) {
		t.Run("nil file", func(t *testing.T) {
			normalizeFileStruct(nil, map[int]Article{1: {ID: "a", Number: 1, Bytes: 10}})
		})

		t.Run("empty or nil map", func(t *testing.T) {
			file := &File{Subject: "test", Bytes: 100}
			normalizeFileStruct(file, nil)
			if len(file.Articles) != 0 || file.Articles == nil {
				t.Errorf("expected empty non-nil Articles slice, got %v", file.Articles)
			}
			if file.Bytes != 0 {
				t.Errorf("expected Bytes=0, got %d", file.Bytes)
			}
		})

		t.Run("sorting and byte sum", func(t *testing.T) {
			byPart := map[int]Article{
				10: {ID: "part10", Number: 10, Bytes: 500},
				2:  {ID: "part2", Number: 2, Bytes: 200},
				5:  {ID: "part5", Number: 5, Bytes: 300},
				1:  {ID: "part1", Number: 1, Bytes: 100},
			}
			file := &File{Subject: "test", Bytes: 9999}
			normalizeFileStruct(file, byPart)

			if len(file.Articles) != 4 {
				t.Fatalf("len(Articles) = %d, want 4", len(file.Articles))
			}
			wantNumbers := []int{1, 2, 5, 10}
			wantIDs := []string{"part1", "part2", "part5", "part10"}
			for i, a := range file.Articles {
				if a.Number != wantNumbers[i] || a.ID != wantIDs[i] {
					t.Errorf("Articles[%d] = (Number:%d, ID:%q), want (Number:%d, ID:%q)",
						i, a.Number, a.ID, wantNumbers[i], wantIDs[i])
				}
			}
			if file.Bytes != 1100 {
				t.Errorf("Bytes = %d, want 1100", file.Bytes)
			}
		})
	})

	t.Run("absorbHead and absorbFile and parseXML", func(t *testing.T) {
		const doc = `<?xml version="1.0"?>
<nzb>
  <head>
    <meta type="title">Test NZB</meta>
  </head>
  <file date="1700000000">
    <groups><group>g</group></groups>
    <segments><segment bytes="100" number="1">id@h</segment></segments>
  </file>
</nzb>`
		got, err := parseXML(strings.NewReader(doc))
		if err != nil {
			t.Fatalf("parseXML: %v", err)
		}
		if got.Meta["title"][0] != "Test NZB" {
			t.Errorf("Meta title = %q", got.Meta["title"][0])
		}
		if len(got.Files) != 1 {
			t.Fatalf("len(Files) = %d", len(got.Files))
		}

		// Direct call to absorbFile
		dec := xml.NewDecoder(strings.NewReader(`<file date="1700000000"><groups><group>g</group></groups><segments><segment bytes="100" number="1">id@h</segment></segments></file>`))
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("dec.Token: %v", err)
		}
		se := tok.(xml.StartElement)
		nzbOut := &NZB{}
		digest := md5.New()
		seenGroups := make(map[string]struct{})
		ts, segs, err := absorbFile(dec, &se, nzbOut, digest, seenGroups, make(map[string]struct{}), time.Now())
		if err != nil {
			t.Fatalf("absorbFile: %v", err)
		}
		if ts != 1700000000 {
			t.Errorf("expected ts=1700000000, got %d", ts)
		}
		if segs != 1 {
			t.Errorf("expected segs=1, got %d", segs)
		}
	})
}

func TestAbsorbHead_Direct(t *testing.T) {
	const headXMLData = `<head>
		<meta type="title">Test Direct Title</meta>
		<meta type="category">  test-category  </meta>
		<meta type="empty"></meta>
		<meta type="">no-type</meta>
	</head>`

	dec := xml.NewDecoder(strings.NewReader(headXMLData))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("dec.Token: %v", err)
	}
	se, ok := tok.(xml.StartElement)
	if !ok {
		t.Fatalf("expected xml.StartElement, got %T", tok)
	}

	nzbOut := &NZB{Meta: make(map[string][]string)}
	err = absorbHead(dec, &se, nzbOut)
	if err != nil {
		t.Fatalf("absorbHead: %v", err)
	}

	if got := nzbOut.Meta["title"]; len(got) != 1 || got[0] != "Test Direct Title" {
		t.Errorf("expected title 'Test Direct Title', got %v", got)
	}
	if got := nzbOut.Meta["category"]; len(got) != 1 || got[0] != "test-category" {
		t.Errorf("expected category 'test-category' (trimmed), got %v", got)
	}
	if _, ok := nzbOut.Meta["empty"]; ok {
		t.Errorf("empty meta value should be skipped")
	}
	if _, ok := nzbOut.Meta[""]; ok {
		t.Errorf("empty meta type should be skipped")
	}
}

func TestParseNoValidFiles(t *testing.T) {
	const doc = `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file subject="no segments">
    <groups><group>g</group></groups>
  </file>
</nzb>`
	_, err := Parse(strings.NewReader(doc))
	if err == nil {
		t.Fatal("expected error on NZB with no valid files")
	}
	if !errors.Is(err, ErrNoFiles) {
		t.Fatalf("expected ErrNoFiles, got: %v", err)
	}
}

func TestParse_MaxFilesWithSkipped(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><nzb>`)
	for range maxFiles + 1 {
		b.WriteString(`<file subject="f"><groups><group>g</group></groups><segments></segments></file>`)
	}
	b.WriteString(`</nzb>`)

	_, err := Parse(strings.NewReader(b.String()))
	if err == nil {
		t.Fatal("expected error for file count matching or exceeding maxFiles via skipped files")
	}
	if !strings.Contains(err.Error(), "file count exceeds limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParse_ExactMaxSegments(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><nzb>`)
	b.WriteString(`<file subject="big" date="1700000000">`)
	b.WriteString(`<groups><group>g</group></groups><segments>`)
	// Distinct Message-IDs per segment: the parser drops a repeat as a
	// duplicate, and this test's subject is the segment-count limit, not
	// deduplication. A real NZB never repeats an ID across segments either.
	for i := 1; i <= maxSegments; i++ {
		b.WriteString(`<segment bytes="100" number="`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`">id`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`@h</segment>`)
	}
	b.WriteString(`</segments></file></nzb>`)

	got, err := Parse(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("expected no error for exactly maxSegments, got: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(got.Files))
	}
	if len(got.Files[0].Articles) != maxSegments {
		t.Fatalf("expected %d articles, got %d", maxSegments, len(got.Files[0].Articles))
	}
}

func TestParseBzip2EnvelopeInline(t *testing.T) {
	bzBytes := []byte{
		0x42, 0x5a, 0x68, 0x39, 0x31, 0x41, 0x59, 0x26, 0x53, 0x59, 0xea, 0xc7,
		0x72, 0x43, 0x00, 0x00, 0x00, 0x99, 0x80, 0x00, 0x00, 0x80, 0x05, 0x10,
		0x01, 0x00, 0x10, 0x20, 0x00, 0x21, 0x80, 0x0c, 0x02, 0x47, 0xae, 0xe2,
		0xee, 0x48, 0xa7, 0x0a, 0x12, 0x1d, 0x58, 0xee, 0x48, 0x60,
	}
	got, err := Parse(bytes.NewReader(bzBytes))
	if err != nil && !errors.Is(err, ErrNoFiles) {
		t.Fatalf("Parse bzip2 inline: %v", err)
	}
	if err == nil && got == nil {
		t.Fatal("expected non-nil NZB")
	}
}

func TestEnvelopeSelection_MoreMagicCombinations(t *testing.T) {
	tests := []struct {
		name  string
		magic []byte
	}{
		{"not gzip or bz2, first byte non-matching", []byte{0x00, 0x00}},
		{"gzip first, bad second", []byte{0x1f, 0x00}},
		{"bz first, bad second", []byte{'B', 'X'}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			br := bufio.NewReader(bytes.NewReader([]byte("<nzb/>")))
			src, closer, err := unwrapEnvelope(br, tc.magic)
			if err != nil {
				t.Fatalf("unwrapEnvelope: %v", err)
			}
			if closer != nil {
				t.Error("expected no closer")
			}
			if src != br {
				t.Error("expected passthrough reader")
			}
		})
	}
}

func TestParseZeroFilesRejects(t *testing.T) {
	const doc = `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
</nzb>`
	_, err := Parse(strings.NewReader(doc))
	if err == nil {
		t.Fatal("expected error for NZB with zero <file> elements")
	}
	if !errors.Is(err, ErrNoFiles) {
		t.Fatalf("expected ErrNoFiles, got: %v", err)
	}
}

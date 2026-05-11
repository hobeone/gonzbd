package nzb

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// --- Fuzz Targets ---

func FuzzParse(f *testing.F) {
	// Seed with a minimal valid NZB.
	f.Add([]byte(`<?xml version="1.0"?>` + "\n" +
		`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n" +
		`<file poster="a" date="1" subject="b">` + "\n" +
		`<groups><group>alt.test</group></groups>` + "\n" +
		`<segments><segment bytes="100" number="1">abc@news</segment></segments>` + "\n" +
		`</file>` + "\n" +
		`</nzb>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Parse(bytes.NewReader(data))
	})
}

// --- M4: Excessive meta tags ---

func TestParse_ExcessiveMetaTags(t *testing.T) {
	// An NZB with 10000 <meta> tags. These are small and should parse
	// without error — meta tags don't hit the segment/file caps.
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><nzb><head>`)
	for i := range 10_000 {
		fmt.Fprintf(&b, `<meta type="tag%d">value%d</meta>`, i, i)
	}
	b.WriteString(`</head>`)
	b.WriteString(`<file subject="f" date="1700000000">` +
		`<groups><group>g</group></groups>` +
		`<segments><segment bytes="100" number="1">id@h</segment></segments>` +
		`</file></nzb>`)

	got, err := Parse(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("Parse with 10000 meta tags: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(got.Files))
	}
	if len(got.Meta) != 10_000 {
		t.Errorf("len(Meta) = %d, want 10000", len(got.Meta))
	}
}

// --- M5: Huge article ID ---

func TestParse_HugeArticleID(t *testing.T) {
	// A segment with a 1 MB message-ID. The parser is bounded by
	// maxNZBSize (256 MB) so this should fit, but tests that the ID
	// is stored without truncation or panic.
	hugeID := strings.Repeat("x", 1024*1024) + "@huge.example.com"
	doc := fmt.Sprintf(`<?xml version="1.0"?><nzb>`+
		`<file subject="huge-id" date="1700000000">`+
		`<groups><group>g</group></groups>`+
		`<segments><segment bytes="100" number="1">%s</segment></segments>`+
		`</file></nzb>`, hugeID)

	got, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse with huge article ID: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(got.Files))
	}
	if got.Files[0].Articles[0].ID != hugeID {
		t.Errorf("article ID was truncated: len=%d, want %d",
			len(got.Files[0].Articles[0].ID), len(hugeID))
	}
}

// --- M6: Huge part number ---

func TestParse_HugePartNumber(t *testing.T) {
	// encoding/xml's int parser uses strconv.Atoi under the hood, which
	// on 64-bit platforms parses into int (64-bit). Verify that a very
	// large but valid 64-bit number parses without overflow or panic.
	// Note: xml segment Number field is int, so values up to max int work.
	doc := `<?xml version="1.0"?><nzb>` +
		`<file subject="huge-part" date="1700000000">` +
		`<groups><group>g</group></groups>` +
		`<segments><segment bytes="100" number="999999999999999999">id@h</segment></segments>` +
		`</file></nzb>`

	got, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse with huge part number: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(got.Files))
	}
	// The article should have been accepted with the large part number.
	if len(got.Files[0].Articles) != 1 {
		t.Fatalf("len(Articles) = %d, want 1", len(got.Files[0].Articles))
	}
	if got.Files[0].Articles[0].Number != 999999999999999999 {
		t.Errorf("Number = %d, want 999999999999999999", got.Files[0].Articles[0].Number)
	}
}

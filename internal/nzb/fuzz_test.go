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
	// A segment with a 1 MB message-ID, alongside a well-formed one. The
	// subject here is robustness: the parser is bounded by maxNZBSize
	// (256 MB) so the document fits, and an ID this size must not panic,
	// truncate, or take the rest of the file down with it.
	//
	// Its disposition is a counted rejection: no NNTP command line can
	// carry it (RFC 3977 §3.1 bounds arguments at 497 octets), so the
	// segment could never have been fetched. The sibling segment is the
	// part that matters — a rejection is scoped to the segment that earned
	// it, not the file it appeared in.
	hugeID := strings.Repeat("x", 1024*1024) + "@huge.example.com"
	doc := fmt.Sprintf(`<?xml version="1.0"?><nzb>`+
		`<file subject="huge-id" date="1700000000">`+
		`<groups><group>g</group></groups>`+
		`<segments><segment bytes="100" number="1">%s</segment>`+
		`<segment bytes="100" number="2">ok@huge.example.com</segment></segments>`+
		`</file></nzb>`, hugeID)

	got, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse with huge article ID: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(got.Files))
	}
	if got.OversizeMessageIDs != 1 {
		t.Errorf("OversizeMessageIDs = %d, want 1", got.OversizeMessageIDs)
	}
	if n := len(got.Files[0].Articles); n != 1 {
		t.Fatalf("len(Articles) = %d, want 1 — only the oversize segment should drop", n)
	}
	if id := got.Files[0].Articles[0].ID; id != "ok@huge.example.com" {
		t.Errorf("surviving article ID = %q, want the well-formed sibling", id)
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

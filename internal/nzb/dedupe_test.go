package nzb

import (
	"crypto/md5" //nolint:gosec // test only: parity check against the parser's own digest
	"fmt"
	"strings"
	"testing"
)

// nzbWithSegments builds a minimal NZB whose <file> elements carry the given
// segment blocks, so a test can state exactly the segment shape under test
// rather than reaching for a shared fixture that other tests also constrain.
func nzbWithSegments(files ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><nzb>`)
	for i, segs := range files {
		fmt.Fprintf(&b, `<file subject="f%d.bin" date="1700000000">`, i)
		b.WriteString(`<groups><group>alt.binaries.test</group></groups><segments>`)
		b.WriteString(segs)
		b.WriteString(`</segments></file>`)
	}
	b.WriteString(`</nzb>`)
	return b.String()
}

func seg(number, bytes int, id string) string {
	return fmt.Sprintf(`<segment bytes="%d" number="%d">%s</segment>`, bytes, number, id)
}

// A Message-ID addresses exactly one article on Usenet, so the same ID on two
// segments is malformed however the part numbers fall. Both copies name the
// same bytes, and keeping both puts two manifest articles behind one identity —
// which is what makes a Message-ID lookup ambiguous downstream.
func TestParse_DuplicateMessageIDWithinAFileIsDropped(t *testing.T) {
	src := nzbWithSegments(seg(1, 100, "dup@h") + seg(2, 100, "dup@h") + seg(3, 100, "other@h"))

	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.DuplicateMessageIDs != 1 {
		t.Errorf("DuplicateMessageIDs = %d, want 1", got.DuplicateMessageIDs)
	}
	arts := got.Files[0].Articles
	if len(arts) != 2 {
		t.Fatalf("len(Articles) = %d, want 2 — the repeated Message-ID was kept", len(arts))
	}
	if arts[0].ID != "dup@h" || arts[0].Number != 1 {
		t.Errorf("Articles[0] = %+v, want the FIRST occurrence, part 1", arts[0])
	}
	if arts[1].ID != "other@h" {
		t.Errorf("Articles[1].ID = %q, want other@h", arts[1].ID)
	}
}

// The manifest index this protects is job-wide, so per-file uniqueness is not
// enough: two <file> elements naming one article collide in the same way.
func TestParse_DuplicateMessageIDAcrossFilesIsDropped(t *testing.T) {
	src := nzbWithSegments(
		seg(1, 100, "shared@h")+seg(2, 100, "a2@h"),
		seg(1, 100, "shared@h")+seg(2, 100, "b2@h"),
	)

	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.DuplicateMessageIDs != 1 {
		t.Errorf("DuplicateMessageIDs = %d, want 1", got.DuplicateMessageIDs)
	}

	seen := map[string]int{}
	for _, f := range got.Files {
		for _, a := range f.Articles {
			seen[a.ID]++
		}
	}
	if seen["shared@h"] != 1 {
		t.Errorf("shared@h appears %d times across files, want 1", seen["shared@h"])
	}
	if seen["a2@h"] != 1 || seen["b2@h"] != 1 {
		t.Errorf("the non-duplicate articles were disturbed: %v", seen)
	}
}

// The digest is the duplicate-JOB detection key and covers ACCEPTED article
// IDs only. A segment the parser drops contributes nothing, so the key
// describes the job the document actually produced rather than the text it
// was written from — which is what lets a rejection rule be added, removed
// or reordered without changing any document's identity as a side effect.
//
// The consequence worth pinning: two NZBs differing only in a dropped
// duplicate hash the same, and the second is recognised as a duplicate job.
func TestParse_DuplicateMessageIDIsExcludedFromTheDigest(t *testing.T) {
	src := nzbWithSegments(seg(1, 100, "dup@h") + seg(2, 100, "dup@h") + seg(3, 100, "other@h"))

	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	h := md5.New() //nolint:gosec // test only
	for _, id := range []string{"dup@h", "other@h"} {
		_, _ = h.Write([]byte(id))
	}
	var want [16]byte
	copy(want[:], h.Sum(nil))

	if got.MD5 != want {
		t.Errorf("MD5 = %x, want %x — the dropped duplicate contributed to the "+
			"digest, so a rejection rule is still able to change an NZB's identity",
			got.MD5, want)
	}

	// The same document without the duplicate segment must hash identically.
	clean := nzbWithSegments(seg(1, 100, "dup@h") + seg(3, 100, "other@h"))
	cleanParsed, err := Parse(strings.NewReader(clean))
	if err != nil {
		t.Fatalf("Parse(clean): %v", err)
	}
	if cleanParsed.MD5 != got.MD5 {
		t.Errorf("MD5 with duplicate = %x, without = %x — the two must agree, "+
			"since the accepted article set is identical",
			got.MD5, cleanParsed.MD5)
	}
}

// A file whose every segment duplicates an earlier file's yields no articles
// and is omitted, on the same terms as a file whose segments all fail the size
// check.
func TestParse_FileOfOnlyDuplicatesIsSkipped(t *testing.T) {
	src := nzbWithSegments(
		seg(1, 100, "x@h"),
		seg(1, 100, "x@h"),
	)

	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("len(Files) = %d, want 1", len(got.Files))
	}
	if got.SkippedFiles != 1 {
		t.Errorf("SkippedFiles = %d, want 1", got.SkippedFiles)
	}
}

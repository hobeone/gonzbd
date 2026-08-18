package nzb

import (
	"strings"
	"testing"
)

// The A-block splits Message-ID rules on FETCHABILITY, not RFC conformance:
// write the "BODY <%s>\r\n" line the ID produces and ask whether a server
// could answer it. A rule that makes the request malformed rejects the
// segment; a rule the request survives merely counts it. These tests pin
// which side each rule falls on, because getting it backwards either drops
// articles that download today or dispatches ones that cannot.

// A well-formed ID must survive every rule untouched. This is the case the
// rest of the file is measured against — a validation change that quietly
// starts rejecting ordinary IDs would fail here first.
func TestParse_WellFormedMessageIDIsUntouched(t *testing.T) {
	got, err := Parse(strings.NewReader(nzbWithSegments(seg(1, 100, "plain@example.com"))))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n := len(got.Files[0].Articles); n != 1 {
		t.Fatalf("len(Articles) = %d, want 1", n)
	}
	if id := got.Files[0].Articles[0].ID; id != "plain@example.com" {
		t.Errorf("ID = %q, want it stored verbatim", id)
	}
	if got.EmptyMessageIDs+got.OversizeMessageIDs+got.MalformedMessageIDs != 0 {
		t.Errorf("a well-formed ID was rejected: empty=%d oversize=%d malformed=%d",
			got.EmptyMessageIDs, got.OversizeMessageIDs, got.MalformedMessageIDs)
	}
	if got.NonConformantMessageIDs+got.NonASCIIMessageIDs+got.MessageIDsMissingAtSign != 0 {
		t.Errorf("a well-formed ID was flagged: long=%d nonascii=%d noat=%d",
			got.NonConformantMessageIDs, got.NonASCIIMessageIDs, got.MessageIDsMissingAtSign)
	}
}

// An NZB may write its segment text with or without the angle-bracket
// wrapper — internal/nntp documented exactly that tolerance at the wire
// layer. Rejecting a bracketed ID would therefore drop EVERY segment of such
// a document and report it as malformed, so the wrapper is normalised away
// and the bare form stored.
func TestParse_AngleBracketWrapperIsNormalisedNotRejected(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"wrapped", "&lt;a@example.com&gt;", "a@example.com"},
		{"wrapped with padding", "  &lt;a@example.com&gt;  ", "a@example.com"},
		{"padding inside the wrapper", "&lt; a@example.com &gt;", "a@example.com"},
		{"bare", "a@example.com", "a@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(nzbWithSegments(seg(1, 100, tc.raw))))
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.raw, err)
			}
			if n := len(got.Files[0].Articles); n != 1 {
				t.Fatalf("len(Articles) = %d, want 1 — %q was rejected", n, tc.raw)
			}
			if id := got.Files[0].Articles[0].ID; id != tc.want {
				t.Errorf("ID = %q, want %q", id, tc.want)
			}
		})
	}
}

// Only a MATCHED pair is stripped, so an unbalanced or interior bracket
// still reaches the rejection rule. "a>b" would close the wire wrapper
// early; trimming any trailing '>' rather than a matched pair would have
// silently accepted it.
func TestParse_InteriorAngleBracketIsStillRejected(t *testing.T) {
	for _, raw := range []string{"a&gt;b@h", "a&lt;b@h", "&lt;a&gt;b@h"} {
		got, err := Parse(strings.NewReader(
			nzbWithSegments(seg(1, 100, raw) + seg(2, 100, "ok@h"))))
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if got.MalformedMessageIDs != 1 {
			t.Errorf("MalformedMessageIDs = %d for %q, want 1", got.MalformedMessageIDs, raw)
		}
		if n := len(got.Files[0].Articles); n != 1 {
			t.Errorf("len(Articles) = %d for %q, want 1 (the sibling)", n, raw)
		}
	}
}

// A1/A3/A4: bytes that make the wire request malformed.
//
// CR and LF are the NNTP command-injection vector, and after this change L0
// is the ONLY layer that refuses them — internal/nntp's own guard is gone.
// They are written here as XML character references on purpose: a literal CR
// in the document is folded to LF by XML line-ending normalisation, so
// "&#13;" is the only way to prove a bare CR can reach the parser. It can,
// which is what makes this rule load-bearing rather than theoretical.
func TestParse_UnfetchableMessageIDsAreRejected(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		counter   func(*NZB) int
	}{
		{"empty", "", func(n *NZB) int { return n.EmptyMessageIDs }},
		{"whitespace only", "   ", func(n *NZB) int { return n.EmptyMessageIDs }},
		{"empty wrapper", "&lt;&gt;", func(n *NZB) int { return n.EmptyMessageIDs }},
		{"interior space", "a b@h", func(n *NZB) int { return n.MalformedMessageIDs }},
		{"interior tab", "a&#9;b@h", func(n *NZB) int { return n.MalformedMessageIDs }},
		{"carriage return", "a&#13;b@h", func(n *NZB) int { return n.MalformedMessageIDs }},
		{"line feed", "a&#10;b@h", func(n *NZB) int { return n.MalformedMessageIDs }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(
				nzbWithSegments(seg(1, 100, tc.raw) + seg(2, 100, "ok@h"))))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if n := tc.counter(got); n != 1 {
				t.Errorf("counter = %d, want 1 for %q", n, tc.raw)
			}
			if n := len(got.Files[0].Articles); n != 1 {
				t.Fatalf("len(Articles) = %d, want 1 — only the bad segment should drop", n)
			}
			if id := got.Files[0].Articles[0].ID; id != "ok@h" {
				t.Errorf("surviving ID = %q, want ok@h", id)
			}
		})
	}
}

// A2a rejects at the NNTP argument bound (495 bare octets), NOT at the RFC's
// conformance limit. The boundary matters in both directions: one octet
// under must still download, one octet over could never have been requested.
func TestParse_MessageIDLengthBoundaries(t *testing.T) {
	idOf := func(n int) string { return strings.Repeat("x", n-2) + "@h" }

	for _, tc := range []struct {
		name          string
		length        int
		wantAccepted  bool
		wantOversize  int
		wantNonConfrm int
	}{
		{"at the RFC limit", 248, true, 0, 0},
		{"one over the RFC limit", 249, true, 0, 1},
		{"at the argument bound", 495, true, 0, 1},
		{"one over the argument bound", 496, false, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := idOf(tc.length)
			if len(id) != tc.length {
				t.Fatalf("fixture length = %d, want %d", len(id), tc.length)
			}
			got, err := Parse(strings.NewReader(
				nzbWithSegments(seg(1, 100, id) + seg(2, 100, "ok@h"))))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			accepted := len(got.Files[0].Articles) == 2
			if accepted != tc.wantAccepted {
				t.Errorf("accepted = %v, want %v (len %d)", accepted, tc.wantAccepted, tc.length)
			}
			if got.OversizeMessageIDs != tc.wantOversize {
				t.Errorf("OversizeMessageIDs = %d, want %d", got.OversizeMessageIDs, tc.wantOversize)
			}
			if got.NonConformantMessageIDs != tc.wantNonConfrm {
				t.Errorf("NonConformantMessageIDs = %d, want %d",
					got.NonConformantMessageIDs, tc.wantNonConfrm)
			}
		})
	}
}

// A2b/A5/A6 are counted, not rejected: each leaves a requestable identifier,
// so refusing would fail articles that download today. The article surviving
// is the assertion that matters — the counter alone would pass even if the
// segment had been dropped.
func TestParse_NonConformantButFetchableMessageIDsAreKept(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		counter   func(*NZB) int
	}{
		{"non-ascii", "café@h", func(n *NZB) int { return n.NonASCIIMessageIDs }},
		{"no at sign", "no-at-sign", func(n *NZB) int { return n.MessageIDsMissingAtSign }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(nzbWithSegments(seg(1, 100, tc.raw))))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if n := len(got.Files[0].Articles); n != 1 {
				t.Fatalf("len(Articles) = %d, want 1 — %q must be KEPT, not rejected", n, tc.raw)
			}
			if id := got.Files[0].Articles[0].ID; id != tc.raw {
				t.Errorf("ID = %q, want %q stored verbatim", id, tc.raw)
			}
			if n := tc.counter(got); n != 1 {
				t.Errorf("counter = %d, want 1", n)
			}
			if got.EmptyMessageIDs+got.OversizeMessageIDs+got.MalformedMessageIDs != 0 {
				t.Errorf("%q was counted as a rejection", tc.raw)
			}
		})
	}
}

// The advisory counters are evaluated at the acceptance point, so a segment
// dropped by a LATER rule is never also reported as a kept anomaly. Without
// that ordering an operator reading the log would see one segment reported
// twice, under two categories that contradict each other.
func TestParse_DroppedSegmentIsNotAlsoCountedAsKept(t *testing.T) {
	// Rejected for implausible size, and would otherwise trip A6 (no '@').
	got, err := Parse(strings.NewReader(
		nzbWithSegments(seg(1, 0, "no-at-sign") + seg(2, 100, "ok@h"))))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.BadArticles != 1 {
		t.Fatalf("BadArticles = %d, want 1", got.BadArticles)
	}
	if got.MessageIDsMissingAtSign != 0 {
		t.Errorf("MessageIDsMissingAtSign = %d, want 0 — the segment was dropped for "+
			"size, so reporting it as a kept anomaly contradicts that",
			got.MessageIDsMissingAtSign)
	}
}

// Rejection rules are checked before the part-number and size rules, so an
// unfetchable ID never counts as a legitimate claim on a part number. The
// alternative would report a malformed ID as a duplicate part, naming the
// wrong defect.
func TestParse_UnfetchableIDDoesNotClaimAPartNumber(t *testing.T) {
	got, err := Parse(strings.NewReader(
		nzbWithSegments(seg(1, 100, "a b@h") + seg(1, 100, "real@h"))))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.MalformedMessageIDs != 1 {
		t.Errorf("MalformedMessageIDs = %d, want 1", got.MalformedMessageIDs)
	}
	if got.DuplicateArticles != 0 {
		t.Errorf("DuplicateArticles = %d, want 0 — the malformed segment never "+
			"claimed part 1, so the real one is not a duplicate", got.DuplicateArticles)
	}
	if n := len(got.Files[0].Articles); n != 1 || got.Files[0].Articles[0].ID != "real@h" {
		t.Errorf("Articles = %+v, want just real@h", got.Files[0].Articles)
	}
}

// The digest covers accepted IDs only (F6), so a rejected Message-ID must
// not change the document's identity. This is what lets the rules above be
// added, reordered or removed without breaking duplicate-job detection.
func TestParse_RejectedMessageIDsDoNotChangeTheDigest(t *testing.T) {
	withBad := nzbWithSegments(seg(1, 100, "a b@h") + seg(2, 100, "ok@h"))
	withoutBad := nzbWithSegments(seg(2, 100, "ok@h"))

	a, err := Parse(strings.NewReader(withBad))
	if err != nil {
		t.Fatalf("Parse(withBad): %v", err)
	}
	b, err := Parse(strings.NewReader(withoutBad))
	if err != nil {
		t.Fatalf("Parse(withoutBad): %v", err)
	}
	if a.MD5 != b.MD5 {
		t.Errorf("MD5 with rejected segment = %x, without = %x — a rejection rule "+
			"is changing document identity", a.MD5, b.MD5)
	}
}

// The byte-level predicates, exercised directly because XML cannot carry
// every input they must handle: encoding/xml rejects U+0000 and the other C0
// controls outright ("illegal character code"), so no document reaching
// partitionSegments can contain them. Those arms are therefore unreachable
// through Parse and would otherwise be untested — and untestable code is how
// the defects this package's contract exists to prevent got written.
func TestMessageIDPredicates(t *testing.T) {
	t.Run("unfetchableMessageIDBytes", func(t *testing.T) {
		for _, b := range []string{" ", "\t", "\r", "\n", "\x00", "<", ">"} {
			if !strings.ContainsAny("a"+b+"b", unfetchableMessageIDBytes) {
				t.Errorf("%q is not in the unfetchable set, want it to be", b)
			}
		}
		// Ordinary ID bytes, and the C0 controls that survive interpolation
		// intact — the server simply answers 430 for those, so they are
		// counted by the printable-ASCII rule rather than rejected here.
		for _, b := range []string{"a", "Z", "0", "@", ".", "-", "\x01", "\x1f", "\x7f"} {
			if strings.ContainsAny(b, unfetchableMessageIDBytes) {
				t.Errorf("%q is in the unfetchable set, want it not to be", b)
			}
		}
		// A non-ASCII rune must not match: every byte in the set is ASCII, so
		// it cannot collide with a UTF-8 continuation byte.
		if strings.ContainsAny("café@h", unfetchableMessageIDBytes) {
			t.Error("a non-ASCII ID matched the unfetchable set")
		}
	})

	t.Run("normaliseMessageID", func(t *testing.T) {
		for _, tc := range []struct{ in, want string }{
			{"a@h", "a@h"},
			{"  a@h  ", "a@h"},
			{"<a@h>", "a@h"},
			{"  <a@h>  ", "a@h"},
			{"< a@h >", "a@h"},
			{"<>", ""},
			{"", ""},
			{"   ", ""},
			// Unmatched brackets are NOT trimmed, so they survive to be
			// rejected. Trimming either end independently would silently
			// accept an ID that closes the wire wrapper early.
			{"<a@h", "<a@h"},
			{"a@h>", "a@h>"},
			{"<", "<"},
			{">", ">"},
		} {
			if got := normaliseMessageID(tc.in); got != tc.want {
				t.Errorf("normaliseMessageID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("nonPrintableASCII", func(t *testing.T) {
		for _, id := range []string{"caf\xc3\xa9@h", "a\x01b", "a\x7fb", "a\x00b", "a b"} {
			if !nonPrintableASCII(id) {
				t.Errorf("nonPrintableASCII(%q) = false, want true", id)
			}
		}
		for _, id := range []string{"a@h", "!~", "plain@example.com", ""} {
			if nonPrintableASCII(id) {
				t.Errorf("nonPrintableASCII(%q) = true, want false", id)
			}
		}
	})
}

// MessageIDIsFetchable is the exported form of the rejection rules, for
// consumers holding an ID this package did not just parse — internal/queue
// re-applies it to manifests read back from disk. It must agree exactly with
// what partitionSegments refuses, or the two layers disagree about which IDs
// are safe to put on the wire.
func TestMessageIDIsFetchable(t *testing.T) {
	for _, id := range []string{
		"plain@example.com",
		"no-at-sign",
		"café@h",                        // non-conformant, still requestable
		"a\x01b@h",                      // control byte, survives interpolation
		strings.Repeat("x", 493) + "@h", // exactly at the argument bound
	} {
		if !MessageIDIsFetchable(id) {
			t.Errorf("MessageIDIsFetchable(%q) = false, want true", id)
		}
	}
	for _, id := range []string{
		"",
		"a b@h", "a\tb@h", "a\rb@h", "a\nb@h", "a\x00b@h",
		"a<b@h", "a>b@h", "<a@h", "a@h>",
		strings.Repeat("x", 494) + "@h", // one octet over
	} {
		if MessageIDIsFetchable(id) {
			t.Errorf("MessageIDIsFetchable(%q) = true, want false", id)
		}
	}
}

// The exported predicate and the parser must not drift apart: every ID the
// parser accepts must be fetchable, and every ID it rejects for a
// fetchability reason must not be. This drives both through Parse so a change
// to one without the other shows up here rather than at a call site.
//
// The two columns differ because XML decodes the segment text before the
// parser sees it — "&lt;" is how a '<' has to be written in a document, and
// the predicate is asked about the decoded form.
func TestMessageIDIsFetchableAgreesWithTheParser(t *testing.T) {
	for _, tc := range []struct{ xmlText, decoded string }{
		{"plain@example.com", "plain@example.com"},
		{"no-at-sign", "no-at-sign"},
		{"café@h", "café@h"},
		{"a b@h", "a b@h"},
		{"a&gt;b@h", "a>b@h"},
		{"a&lt;b@h", "a<b@h"},
		{"a&#13;b@h", "a\rb@h"},
		{"", ""},
		{strings.Repeat("x", 493) + "@h", strings.Repeat("x", 493) + "@h"},
		{strings.Repeat("x", 494) + "@h", strings.Repeat("x", 494) + "@h"},
	} {
		got, err := Parse(strings.NewReader(
			nzbWithSegments(seg(1, 100, tc.xmlText) + seg(2, 100, "sentinel@h"))))
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.xmlText, err)
		}
		accepted := len(got.Files[0].Articles) == 2
		if want := MessageIDIsFetchable(normaliseMessageID(tc.decoded)); accepted != want {
			t.Errorf("parser accepted %q = %v, but MessageIDIsFetchable(%q) says %v",
				tc.xmlText, accepted, tc.decoded, want)
		}
	}
}

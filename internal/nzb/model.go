// Package nzb parses Usenet NZB files into an in-memory model ready for
// queue ingestion.
//
// # Scope
//
// This package is deliberately narrow: it reads XML and returns a struct.
// It does not open network connections, touch the config, or coordinate
// with the queue manager. Callers own the lifecycle of whatever io.Reader
// they hand in.
//
// # Format support
//
// Parse accepts three input forms and detects them by magic bytes:
//
//   - plain XML                 (first byte '<')
//   - gzip-compressed XML       (0x1f 0x8b)
//   - bzip2-compressed XML      ("BZ")
//
// This matches SABnzbd's long-standing behaviour of accepting "nzb.gz" and
// "nzb.bz2" in disguise, where operators rename compressed NZBs to ".nzb"
// and expect the daemon to figure it out.
//
// # Parity with the Python parser
//
// The Go parser follows the on-disk semantics of
// sabnzbd/nzbparser.py#nzbfile_parser, with the deliberate divergences noted
// below. Byte-for-byte parity is not a goal: GoNZBD is not a drop-in
// replacement, and no artefact it writes is read by the Python implementation.
//
//   - <meta> is multi-valued: the same type attribute may repeat, and all
//     values are preserved in document order.
//   - Each <file>'s articles are sorted by part number; duplicate part
//     numbers with the same ID are silently skipped, duplicates with a
//     different ID bump DuplicateArticles.
//   - A Message-ID already claimed by an accepted segment ANYWHERE in the
//     document is dropped and bumps DuplicateMessageIDs. This is a
//     deliberate divergence from the Python parser, which keeps such
//     segments: it guarantees Message-ID is a unique key within a job, so
//     downstream lookups cannot silently address the wrong article.
//   - Articles with bytes <= 0 or bytes >= 8 MiB bump BadArticles and are
//     excluded from the file.
//   - A segment whose Message-ID could not produce a fetchable NNTP request
//     is excluded and bumps one of EmptyMessageIDs, OversizeMessageIDs or
//     MalformedMessageIDs. A segment whose Message-ID merely violates the
//     RFC while staying requestable is KEPT, and bumps one of
//     NonConformantMessageIDs, NonASCIIMessageIDs or
//     MessageIDsMissingAtSign. The Python parser validates neither.
//   - A Message-ID wrapped in angle brackets is normalised to its bare form
//     rather than rejected; Article.ID never carries the wrapper.
//   - A file with zero valid articles is omitted and bumps SkippedFiles.
//   - MD5 is the digest of every ACCEPTED article ID, in source order.
//     Segments this parser rejects contribute nothing, so the key describes
//     the job the document actually produced and no rejection rule can
//     change a document's identity as a side effect. This diverges from the
//     Python parser, which hashes rejected IDs too; two NZBs differing only
//     in their rejected segments are duplicates here and distinct there.
package nzb

import "time"

// NZB is a fully-parsed Usenet NZB document.
type NZB struct {
	// Meta holds the <head>/<meta type="K">V</meta> tags. A key may
	// appear multiple times (e.g. several "category" entries); values
	// are stored in document order.
	Meta map[string][]string

	// Files is every <file> element in document order. Files whose
	// articles all failed validation are excluded (see SkippedFiles).
	Files []File

	// Groups is the de-duplicated union of <group> elements across
	// every file, in the order they were first seen.
	Groups []string

	// MD5 is the MD5 digest of every accepted article ID concatenated in
	// source order; rejected segments contribute nothing. It is the
	// duplicate-job key — see the package doc for what that implies.
	MD5 [16]byte

	// AvgAge is the mean posted-date across every file that contributed
	// a timestamp. Zero value when no files contributed.
	AvgAge time.Time

	// DuplicateArticles counts segments whose part number collided with
	// an earlier segment and whose ID differed (indicating a malformed
	// NZB rather than a harmless retransmission).
	DuplicateArticles int

	// DuplicateMessageIDs counts segments dropped because their Message-ID
	// had already been claimed by an accepted segment anywhere in the
	// document — not merely in the same <file>.
	//
	// A Message-ID addresses exactly one article, so a repeat names bytes
	// that are already accounted for. Keeping both copies would put two
	// manifest articles behind one identity, which no Message-ID lookup can
	// resolve unambiguously; downstream code may therefore assume the map
	// from Message-ID to article is injective within a job.
	DuplicateMessageIDs int

	// BadArticles counts segments rejected for implausible size.
	BadArticles int

	// The Message-ID rejection counters. Each names a segment that was
	// DROPPED because the NNTP request its Message-ID produces could not
	// have named an article on any server — RFC conformance is not the
	// criterion, fetchability is.
	//
	// EmptyMessageIDs counts segments whose <segment> text was empty or
	// consisted only of whitespace and an angle-bracket wrapper.
	EmptyMessageIDs int

	// OversizeMessageIDs counts Message-IDs too long to fit an NNTP command
	// argument (RFC 3977 §3.1), which makes the server reject the command
	// line rather than the article.
	OversizeMessageIDs int

	// MalformedMessageIDs counts Message-IDs containing a byte that breaks
	// the wire request: SP or HT (argument tokenisation), CR, LF or NUL
	// (the command line itself, and the injection vector), or an interior
	// '<' or '>' (the angle-bracket wrapper).
	MalformedMessageIDs int

	// The Message-ID advisory counters. Unlike the three above, these
	// segments are KEPT and downloaded — each records an RFC violation that
	// still leaves a requestable identifier. They exist so that promoting
	// any of them to a rejection can rest on observed frequency rather than
	// on assumption; nothing downstream branches on them.
	//
	// NonConformantMessageIDs counts IDs longer than RFC 3977 §3.6's limit
	// but still short enough to request.
	NonConformantMessageIDs int

	// NonASCIIMessageIDs counts IDs containing a byte outside printable
	// US-ASCII — both non-ASCII bytes and stray control bytes that survive
	// interpolation intact.
	NonASCIIMessageIDs int

	// MessageIDsMissingAtSign counts IDs with no '@', which RFC 5536 §3.1.3
	// requires but which servers may nonetheless answer.
	MessageIDsMissingAtSign int

	// SkippedFiles counts <file> elements that yielded zero valid
	// articles after dedup and size checks.
	SkippedFiles int
}

// File is one <file> element: a single Usenet posting made up of
// numbered article segments.
type File struct {
	// Subject is the file's subject line. Defaults to "unknown" when
	// the source omitted the attribute.
	Subject string

	// Date is the posting timestamp. Defaults to the parse wall-clock
	// when the source omitted or malformed the date attribute.
	Date time.Time

	// Groups is the list of newsgroups the file was posted to, in
	// document order (not deduplicated; see NZB.Groups for the union).
	Groups []string

	// Articles is the file's segments sorted by part number ascending.
	Articles []Article

	// Bytes is the sum of Articles[].Bytes; the NZB's claim of the
	// decoded file size. Untrusted — use only for display and
	// free-space pre-checks.
	Bytes int64
}

// Article is one <segment>: a single NNTP message-id plus the byte
// count the poster claimed and the part number used to order the file.
type Article struct {
	// ID is the NNTP message-id without angle brackets.
	ID string

	// Bytes is the poster-claimed size. Validated to be in (0, 8 MiB).
	Bytes int

	// Number is the 1-based part number within its parent file.
	Number int
}

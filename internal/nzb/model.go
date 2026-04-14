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
// The Go parser reproduces the on-disk semantics of
// sabnzbd/nzbparser.py#nzbfile_parser one-for-one:
//
//   - <meta> is multi-valued: the same type attribute may repeat, and all
//     values are preserved in document order.
//   - Each <file>'s articles are sorted by part number; duplicate part
//     numbers with the same ID are silently skipped, duplicates with a
//     different ID bump DuplicateArticles.
//   - Articles with bytes <= 0 or bytes >= 8 MiB bump BadArticles and are
//     excluded from the file.
//   - A file with zero valid articles is omitted and bumps SkippedFiles.
//   - MD5 is the digest of every structurally-complete article ID, in
//     source order, including IDs that were later deduped or rejected for
//     size. Callers relying on MD5 for duplicate-job detection must use
//     this exact order, or jobs imported from Python SABnzbd won't match.
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

	// MD5 is the MD5 digest of every structurally-complete article ID
	// concatenated in source order. See the package doc for the exact
	// ordering rule; it is load-bearing for duplicate-job detection.
	MD5 [16]byte

	// AvgAge is the mean posted-date across every file that contributed
	// a timestamp. Zero value when no files contributed.
	AvgAge time.Time

	// DuplicateArticles counts segments whose part number collided with
	// an earlier segment and whose ID differed (indicating a malformed
	// NZB rather than a harmless retransmission).
	DuplicateArticles int

	// BadArticles counts segments rejected for implausible size.
	BadArticles int

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

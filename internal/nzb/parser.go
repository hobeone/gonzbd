package nzb

import (
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"crypto/md5" //nolint:gosec // MD5 is used for duplicate-job detection, not security
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/charmap"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// ErrNoFiles is returned when an NZB file contains no <file> elements at all.
var ErrNoFiles = errors.New("nzb: no <file> elements found")

// maxArticleSize is the upper bound on a plausible NNTP article payload
// (8 MiB, inclusive). Values at or above this are treated as malformed.
// Matches Python sabnzbd/nzbparser.py which uses `>= 2**23`.
const maxArticleSize = 1 << 23

const (
	// maxNZBSize caps the (decompressed) XML input to the NZB parser.
	// 256 MiB is far beyond any legitimate NZB and prevents billion-laughs
	// XML bombs and gzip/bzip2 decompression bombs from causing OOM.
	maxNZBSize = 256 * 1024 * 1024 // 256 MB

	// maxFiles caps the number of <file> elements accepted in a single NZB.
	// The largest known real-world NZBs have ~5 000 files.
	maxFiles = 50_000

	// maxSegments caps the total number of <segment> elements across the
	// entire NZB. At ~1 MB per segment, 500 000 covers ~500 GB downloads.
	maxSegments = 500_000
)

// charsetReader lets encoding/xml accept the two charsets that appear
// in NZB files in the wild: utf-8 (modern) and iso-8859-1 (legacy, still
// the default in the newzBin DTD). Anything else is refused rather than
// silently decoded as latin-1 — NZBs are normally ASCII-only inside the
// tags regardless of what the prolog claims, and a surprise encoding
// suggests a corrupted file.
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "", "utf-8", "utf8":
		return input, nil
	case "iso-8859-1", "latin1", "latin-1", "iso8859-1":
		return charmap.ISO8859_1.NewDecoder().Reader(input), nil
	}
	return nil, fmt.Errorf("nzb: unsupported XML charset %q", label)
}

// Parse decodes an NZB document from r. Gzip and bzip2 envelopes are
// detected by magic bytes and transparently unwrapped, so callers can
// pass the raw file handle without inspecting the extension.
//
// A structurally broken document returns (nil, error). Counters on the
// returned *NZB record recoverable issues (duplicate parts, implausible
// sizes, empty files); Parse never fails for those alone.
func Parse(r io.Reader) (*NZB, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("nzb: peek magic bytes: %w", err)
	}
	if len(magic) == 0 {
		// Empty input is never a valid NZB; surface it clearly rather
		// than returning a zero-Files NZB that callers would need to
		// re-check.
		return nil, errors.New("nzb: empty input")
	}

	src, closer, err := unwrapEnvelope(br, magic)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer() //nolint:errcheck // best-effort close of decompressor on the read path
	}
	return parseXML(src)
}

// unwrapEnvelope returns a reader yielding plain XML, plus an optional
// closer for underlying decompressors. gzip.Reader must be closed to
// free its buffer; bzip2.Reader has no Close.
func unwrapEnvelope(br *bufio.Reader, magic []byte) (io.Reader, func() error, error) {
	if len(magic) < 2 {
		return br, nil, nil
	}
	switch {
	case magic[0] == 0x1f && magic[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, nil, fmt.Errorf("nzb: gzip envelope: %w", err)
		}
		return io.LimitReader(gz, maxNZBSize), gz.Close, nil
	case magic[0] == 'B' && magic[1] == 'Z':
		return io.LimitReader(bzip2.NewReader(br), maxNZBSize), nil, nil
	}
	return br, nil, nil
}

// xmlHead / xmlFile / xmlSegment are wire-format shims. They exist only
// to let encoding/xml populate their fields; the public model lives in
// model.go and is populated via convertFile.
type xmlHead struct {
	Metas []xmlMeta `xml:"meta"`
}
type xmlMeta struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}
type xmlFile struct {
	Subject  string       `xml:"subject,attr"`
	Date     string       `xml:"date,attr"`
	Groups   []string     `xml:"groups>group"`
	Segments []xmlSegment `xml:"segments>segment"`
}
type xmlSegment struct {
	Bytes  int    `xml:"bytes,attr"`
	Number int    `xml:"number,attr"`
	ID     string `xml:",chardata"`
}

// parseXML walks the document at token granularity, decoding each
// <head> and <file> subtree with DecodeElement. This keeps memory
// proportional to the largest single <file>, not the whole document.
func parseXML(r io.Reader) (*NZB, error) {
	dec := xml.NewDecoder(io.LimitReader(r, maxNZBSize))
	// Namespaces are present in real NZBs (xmlns="http://www.newzbin.com/DTD/2003/nzb").
	// We match by Local name only.
	dec.CharsetReader = charsetReader

	now := time.Now()
	out := &NZB{Meta: make(map[string][]string)}
	digest := md5.New() //nolint:gosec // see package-level justification
	seenGroups := make(map[string]struct{})
	// Message-ID uniqueness is job-wide, not per-file: a Message-ID addresses
	// exactly one article on Usenet, so the same ID in two <file> elements is
	// as malformed as the same ID twice in one. The set therefore lives here
	// and is threaded down, rather than being rebuilt per file.
	seenIDs := make(map[string]struct{})

	var ageSum int64
	var ageCount int
	var totalSegments int

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("nzb: read token: %w", err)
		}

		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}

		switch strings.ToLower(se.Name.Local) {
		case "head":
			if err := absorbHead(dec, &se, out); err != nil {
				return nil, err
			}
		case "file":
			if len(out.Files)+out.SkippedFiles >= maxFiles {
				return nil, fmt.Errorf("nzb: file count exceeds limit of %d", maxFiles)
			}
			ts, segs, err := absorbFile(dec, &se, out, digest, seenGroups, seenIDs, now)
			if err != nil {
				return nil, err
			}
			totalSegments += segs
			if totalSegments > maxSegments {
				return nil, fmt.Errorf("nzb: segment count exceeds limit of %d", maxSegments)
			}
			if ts != 0 {
				ageSum += ts
				ageCount++
			}
		}
	}

	if len(out.Files) == 0 {
		return nil, ErrNoFiles
	}

	copy(out.MD5[:], digest.Sum(nil))
	if ageCount > 0 {
		out.AvgAge = time.Unix(ageSum/int64(ageCount), 0)
	}
	return out, nil
}

func absorbHead(dec *xml.Decoder, se *xml.StartElement, out *NZB) error {
	var head xmlHead
	if err := dec.DecodeElement(&head, se); err != nil {
		return fmt.Errorf("nzb: decode <head>: %w", err)
	}
	for _, m := range head.Metas {
		v := strings.TrimSpace(m.Value)
		if m.Type == "" || v == "" {
			continue
		}
		out.Meta[m.Type] = append(out.Meta[m.Type], v)
	}
	return nil
}

// absorbFile decodes one <file> subtree and appends it to out if it has
// any valid articles. The returned timestamp is zero for skipped files;
// the caller folds non-zero values into its average-age rolling sum.
// Matches Python's behavior of excluding skipped files from avg_age.
// The second return value is the number of raw XML segments in this file
// (for the caller's total-segment-count budget).
func absorbFile(
	dec *xml.Decoder,
	se *xml.StartElement,
	out *NZB,
	digest hash.Hash,
	seenGroups map[string]struct{},
	seenIDs map[string]struct{},
	now time.Time,
) (timestamp int64, numSegments int, err error) {
	var xf xmlFile
	if err := dec.DecodeElement(&xf, se); err != nil {
		return 0, 0, fmt.Errorf("nzb: decode <file>: %w", err)
	}

	file, ts, counters := convertFile(xf, now, digest, seenIDs)

	out.DuplicateArticles += counters.dupes
	out.DuplicateMessageIDs += counters.dupeIDs
	out.BadArticles += counters.bad
	out.EmptyMessageIDs += counters.emptyID
	out.OversizeMessageIDs += counters.oversizeID
	out.MalformedMessageIDs += counters.malformedID
	out.NonConformantMessageIDs += counters.nonConformantID
	out.NonASCIIMessageIDs += counters.nonASCIIID
	out.MessageIDsMissingAtSign += counters.missingAtID

	if len(file.Articles) == 0 {
		out.SkippedFiles++
		return 0, len(xf.Segments), nil
	}
	for _, g := range file.Groups {
		if _, dup := seenGroups[g]; dup {
			continue
		}
		seenGroups[g] = struct{}{}
		out.Groups = append(out.Groups, g)
	}
	out.Files = append(out.Files, file)
	return ts, len(xf.Segments), nil
}

type articleCounters struct {
	dupes, dupeIDs, bad int

	// Rejected: the segment is dropped because the wire request its
	// Message-ID produces could not name an article on any server.
	emptyID, oversizeID, malformedID int

	// Counted: the segment is KEPT and downloaded normally. These record
	// RFC violations that leave a requestable identifier, so that a future
	// decision to promote one to a rejection can rest on evidence rather
	// than on how often the shape is imagined to occur.
	nonConformantID, nonASCIIID, missingAtID int
}

// Message-ID bounds, derived from RFC 3977. Both of the RFC's limits are
// stated for the BRACKETED form — §3.6 defines a message-id as beginning
// "<" and ending ">", then bounds that whole thing — while Article.ID holds
// the bare form. Each constant is therefore two octets smaller than the
// number the RFC prints.
const (
	// maxMessageIDOctets is the largest bare Message-ID that fits an NNTP
	// command argument. RFC 3977 §3.1: "The arguments MUST NOT exceed 497
	// octets." The argument is "<" + id + ">", so id <= 495. Exceeding it
	// makes the server reject the command LINE rather than the article,
	// which is why this half rejects.
	//
	// The same section's 512-octet command-line limit never binds here:
	// "BODY " + "<id>" + CRLF is len(id)+9, permitting 503. It could only
	// bind if a verb plus its space exceeded 512-497-2 = 13 octets, and the
	// longest verb taking a message-id is ARTICLE at 8 including the space.
	maxMessageIDOctets = 495

	// rfcMessageIDOctets is RFC 3977 §3.6's conformance limit ("between 3
	// and 250 octets"), bracketed, so 248 bare. Exceeding it is COUNTED,
	// not rejected: the request stays well-formed and a server may answer
	// it, so refusing would fail articles that download today.
	rfcMessageIDOctets = 248
)

// unfetchableMessageIDBytes are the bytes that make the wire request
// unfetchable, as opposed to merely non-conformant.
//
// SP and HT break the command's argument tokenisation; CR, LF and NUL break
// the command line itself and are the NNTP injection vector; '<' and '>'
// break the angle-bracket wrapper the ID is interpolated into. RFC 3977 §3.6
// permits '>' only as the final octet; normaliseMessageID has already removed
// it in that position WHEN it closed a matched pair, so anything reaching
// this set is an unmatched or interior bracket, which is the case to refuse.
//
// Other control bytes are deliberately absent: they survive interpolation
// intact, so the server simply answers 430. They are counted by the
// printable-ASCII rule instead of rejected.
//
// Every byte here is ASCII, so strings.ContainsAny cannot match inside a
// multi-byte UTF-8 sequence — a non-ASCII Message-ID is unaffected by this
// set and falls to the printable-ASCII counter instead.
const unfetchableMessageIDBytes = " \t\r\n\x00<>"

// MessageIDIsFetchable reports whether id could name an article on a server —
// that is, whether interpolating it into an NNTP command produces a
// well-formed request. It is the exported form of the rejection rules
// partitionSegments applies, and exists so that a consumer holding a
// Message-ID this package did not just parse can apply the same test rather
// than reimplementing it.
//
// It expects the bare form, as stored in Article.ID: normaliseMessageID has
// already stripped any angle-bracket wrapper, so a bracket reaching here is
// unmatched or interior and is refused.
//
// This is deliberately NOT a conformance check. An ID may be fetchable and
// still violate RFC 3977 — over 248 octets, non-ASCII, or lacking '@' — and
// those are counted at parse time rather than refused.
func MessageIDIsFetchable(id string) bool {
	return id != "" &&
		len(id) <= maxMessageIDOctets &&
		!strings.ContainsAny(id, unfetchableMessageIDBytes)
}

// normaliseMessageID trims surrounding whitespace and a matched angle-bracket
// wrapper, returning the bare form stored in Article.ID.
//
// The wrapper is stripped rather than rejected because an NZB may write its
// segment text either way — internal/nntp documented exactly that tolerance
// at the wire layer, so rejecting a bracketed ID here would drop every
// segment of such a document. Stripping a MATCHED pair only, rather than
// trimming any leading or trailing angle bracket, keeps "a>b" rejectable.
func normaliseMessageID(raw string) string {
	id := strings.TrimSpace(raw)
	if len(id) >= 2 && id[0] == '<' && id[len(id)-1] == '>' {
		id = strings.TrimSpace(id[1 : len(id)-1])
	}
	return id
}

// nonPrintableASCII reports whether id contains a byte outside the printable
// US-ASCII range RFC 3977 §3.6 requires. Covers both non-ASCII (>= 0x80) and
// stray control bytes in one counter, since the disposition is identical.
func nonPrintableASCII(id string) bool {
	for i := range len(id) {
		if id[i] < 0x21 || id[i] > 0x7e {
			return true
		}
	}
	return false
}

// convertFile transforms a wire-format xmlFile into the public File
// model, applying the dedup and size-sanity rules and folding accepted
// article IDs into digest in source order.
func convertFile(xf xmlFile, now time.Time, digest hash.Hash, seenIDs map[string]struct{}) (File, int64, articleCounters) {
	// Since we don't have user config here, we use default options.
	subject := fsutil.SanitizeFilename(ExtractFilenameFromSubject(xf.Subject), fsutil.SanitizeOptions{})

	ts := now.Unix()
	if trimmed := strings.TrimSpace(xf.Date); trimmed != "" {
		if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			ts = n
		}
	}

	file := File{
		Subject: subject,
		Date:    time.Unix(ts, 0),
		Groups:  xf.Groups,
	}

	byPart, counters := partitionSegments(xf.Segments, digest, seenIDs)
	normalizeFileStruct(&file, byPart)

	if len(file.Articles) == 0 {
		return file, 0, counters
	}
	return file, ts, counters
}

// partitionSegments iterates through raw xmlSegment elements, filtering out
// structurally invalid or out-of-bounds segments and deduplicating by part number.
// Accepted segment IDs are hashed into digest in source order.
func partitionSegments(segments []xmlSegment, digest hash.Hash, seenIDs map[string]struct{}) (map[int]Article, articleCounters) {
	byPart := make(map[int]Article, len(segments))
	var counters articleCounters
	for _, s := range segments {
		id := normaliseMessageID(s.ID)
		// Structural validity: the part number must be a positive integer.
		// A missing or zero number is a malformed XML attribute rather than
		// a Message-ID defect, and is dropped without a counter.
		if s.Number <= 0 {
			continue
		}

		// Message-ID validity, checked before the part-number and size rules
		// so that an unfetchable ID is never treated as a legitimate claim
		// on a part number. Each of these makes the wire request malformed,
		// so the segment could not have downloaded under any server.
		if id == "" {
			counters.emptyID++
			continue
		}
		if len(id) > maxMessageIDOctets {
			counters.oversizeID++
			continue
		}
		if strings.ContainsAny(id, unfetchableMessageIDBytes) {
			counters.malformedID++
			continue
		}

		if prev, seen := byPart[s.Number]; seen {
			if prev.ID != id {
				counters.dupes++
			}
			continue
		}
		// A repeated Message-ID names bytes an earlier segment already claims.
		// Keeping both would put two manifest articles behind one identity,
		// which no Message-ID lookup can then resolve unambiguously — so the
		// later one is dropped and counted.
		//
		// The dropped segment's bytes leave with it, and that is deliberate.
		// File.Bytes is the sum of Articles[].Bytes and is what JobProgress
		// derives both expected and remaining bytes from, so counting bytes for
		// an article that is not in the manifest strands them in `remaining` —
		// it can never be downloaded and never failed.
		//
		// The cost is accepted rather than unnoticed: File.Bytes also becomes
		// the assembler's ExpectedSize, which bounds writes to ExpectedSize +
		// 12.5%. Dropping k of N equally-sized segments leaves that bound at
		// (N-k)/N * 1.02 * 1.125 of the file's true size, which falls under 1.0
		// once k/N passes ~12.85% — one duplicate in a file of seven segments
		// or fewer. Such a file is already malformed and bound for par2, and
		// the alternative distorts the size accounting of every affected job to
		// avoid it.
		if _, dup := seenIDs[id]; dup {
			counters.dupeIDs++
			continue
		}
		if s.Bytes <= 0 || s.Bytes >= maxArticleSize {
			counters.bad++
			continue
		}

		// Everything below runs once, on acceptance, and is the single place a
		// segment becomes an Article. The digest covers accepted IDs only: it
		// is the duplicate-job key, and deriving it from what the job actually
		// contains means no rejection rule can change a document's identity as
		// a side effect. Rejections may therefore be added, removed or
		// reordered above without touching NZB.MD5.
		//
		// seenIDs is recorded here for the same reason: a segment rejected for
		// implausible size never claimed the ID, so a later well-formed segment
		// carrying it is not a duplicate of anything.
		// The counted rows are evaluated HERE, not beside the rejections
		// above, because they describe a segment that was kept. Counting one
		// earlier would report an anomaly for a segment a later rule then
		// dropped, and the reporting distinguishes the two by that fact.
		if len(id) > rfcMessageIDOctets {
			counters.nonConformantID++
		}
		if nonPrintableASCII(id) {
			counters.nonASCIIID++
		}
		if !strings.Contains(id, "@") {
			counters.missingAtID++
		}

		_, _ = digest.Write([]byte(id)) //nolint:errcheck // hash.Hash never returns a non-nil error
		seenIDs[id] = struct{}{}
		byPart[s.Number] = Article{ID: id, Bytes: s.Bytes, Number: s.Number}
	}
	return byPart, counters
}

// normalizeFileStruct takes the partitioned segments map, sorts articles in
// ascending order by part number, and normalizes the File struct's Articles
// slice and total Bytes count.
func normalizeFileStruct(file *File, byPart map[int]Article) {
	if file == nil {
		return
	}
	parts := make([]int, 0, len(byPart))
	for p := range byPart {
		parts = append(parts, p)
	}
	slices.Sort(parts)

	file.Articles = make([]Article, 0, len(parts))
	var totalBytes int64
	for _, p := range parts {
		a := byPart[p]
		file.Articles = append(file.Articles, a)
		totalBytes += int64(a.Bytes)
	}
	file.Bytes = totalBytes
}

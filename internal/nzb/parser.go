package nzb

import (
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"crypto/md5" //nolint:gosec // MD5 is used for duplicate-job detection, not security; digest must match Python SABnzbd
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/fsutil"
)

// maxArticleSize is the upper bound on a plausible NNTP article payload
// (8 MiB, inclusive). Values at or above this are treated as malformed.
// Matches Python sabnzbd/nzbparser.py which uses `>= 2**23`.
const maxArticleSize = 1 << 23

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
		return &latin1Reader{src: input}, nil
	}
	return nil, fmt.Errorf("nzb: unsupported XML charset %q", label)
}

// latin1Reader re-encodes an ISO-8859-1 byte stream into UTF-8 on the
// fly. Each input byte is a Unicode codepoint (by construction of
// latin-1), so we emit either the byte verbatim (0x00-0x7F) or a 2-byte
// UTF-8 sequence (0x80-0xFF). The implementation keeps no unbounded
// buffer: if the caller's buffer would be truncated mid-high-byte (where
// one input byte expands to two), we stash the pending byte and finish
// on the next Read.
type latin1Reader struct {
	src     io.Reader
	pending byte // 0 when no pending output byte; otherwise the trailing UTF-8 byte
	hasPend bool
}

func (r *latin1Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n := 0
	if r.hasPend {
		p[0] = r.pending
		r.hasPend = false
		n = 1
		if n == len(p) {
			return n, nil
		}
	}
	// Read at most half of the remaining space so every high byte has
	// room for its 2-byte expansion.
	readLen := (len(p) - n + 1) / 2
	if readLen == 0 {
		return n, nil
	}
	buf := make([]byte, readLen)
	m, err := r.src.Read(buf)
	for i := 0; i < m; i++ {
		b := buf[i]
		if b < 0x80 {
			p[n] = b
			n++
			continue
		}
		// b is 0x80..0xFF; emit as two-byte UTF-8: 110xxxxx 10xxxxxx.
		p[n] = 0xC0 | (b >> 6)
		n++
		lo := 0x80 | (b & 0x3F)
		if n < len(p) {
			p[n] = lo
			n++
		} else {
			r.pending = lo
			r.hasPend = true
		}
	}
	return n, err
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
		return gz, gz.Close, nil
	case magic[0] == 'B' && magic[1] == 'Z':
		return bzip2.NewReader(br), nil, nil
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
	dec := xml.NewDecoder(r)
	// Namespaces are present in real NZBs (xmlns="http://www.newzbin.com/DTD/2003/nzb").
	// We match by Local name only.
	dec.CharsetReader = charsetReader

	now := time.Now()
	out := &NZB{Meta: make(map[string][]string)}
	digest := md5.New() //nolint:gosec // see package-level justification
	seenGroups := make(map[string]struct{})

	var ageSum int64
	var ageCount int

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
			ts, err := absorbFile(dec, &se, out, digest, seenGroups, now)
			if err != nil {
				return nil, err
			}
			if ts != 0 {
				ageSum += ts
				ageCount++
			}
		}
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
func absorbFile(
	dec *xml.Decoder,
	se *xml.StartElement,
	out *NZB,
	digest hash.Hash,
	seenGroups map[string]struct{},
	now time.Time,
) (int64, error) {
	var xf xmlFile
	if err := dec.DecodeElement(&xf, se); err != nil {
		return 0, fmt.Errorf("nzb: decode <file>: %w", err)
	}

	file, ts, counters := convertFile(xf, now, digest)

	out.DuplicateArticles += counters.dupes
	out.BadArticles += counters.bad

	if len(file.Articles) == 0 {
		out.SkippedFiles++
		return 0, nil
	}
	for _, g := range file.Groups {
		if _, dup := seenGroups[g]; dup {
			continue
		}
		seenGroups[g] = struct{}{}
		out.Groups = append(out.Groups, g)
	}
	out.Files = append(out.Files, file)
	return ts, nil
}

type articleCounters struct {
	dupes, bad int
}

// convertFile transforms a wire-format xmlFile into the public File
// model, applying the dedup and size-sanity rules and folding article
// IDs into digest in source order.
func convertFile(xf xmlFile, now time.Time, digest hash.Hash) (File, int64, articleCounters) {
	subject := fsutil.SanitizeFilename(ExtractFilenameFromSubject(xf.Subject))

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

	byPart := make(map[int]Article, len(xf.Segments))
	var counters articleCounters
	for _, s := range xf.Segments {
		id := strings.TrimSpace(s.ID)
		// Structural validity: id must be present and the part number
		// must be a positive integer. Missing/zero mirrors Python's
		// try/except around int(attrib.get(...)): silent drop, no MD5
		// update, no counter bump.
		if id == "" || s.Number <= 0 {
			continue
		}

		// Hash every structurally-valid article ID, even ones we'll
		// reject below. See the package doc: digest ordering is
		// load-bearing for cross-implementation dedup.
		_, _ = digest.Write([]byte(id)) //nolint:errcheck // hash.Hash never returns a non-nil error

		if prev, seen := byPart[s.Number]; seen {
			if prev.ID != id {
				counters.dupes++
			}
			continue
		}
		if s.Bytes <= 0 || s.Bytes >= maxArticleSize {
			counters.bad++
			continue
		}
		byPart[s.Number] = Article{ID: id, Bytes: s.Bytes, Number: s.Number}
	}

	parts := make([]int, 0, len(byPart))
	for p := range byPart {
		parts = append(parts, p)
	}
	sort.Ints(parts)

	file.Articles = make([]Article, 0, len(parts))
	for _, p := range parts {
		a := byPart[p]
		file.Articles = append(file.Articles, a)
		file.Bytes += int64(a.Bytes)
	}

	if len(file.Articles) == 0 {
		return file, 0, counters
	}
	return file, ts, counters
}

// Command nzbprobe exercises gonzbd's identify-then-verify path against a REAL
// NZB and REAL news servers.
//
// It reports three things:
//
//  1. What an NZB declares as a file's byte count, against the length par2
//     records for the same file. Every unit test supplies those two numbers
//     from one source, so no test can tell whether they agree in the world.
//     They do not: measured at 1.03x-1.08x. That gap is why nothing in this
//     package compares the two, and why a fallback that did could never fire.
//
//  2. Whether the delivered files can be IDENTIFIED against the par2 index by
//     Hash16k, which costs 16 KB per file. For an obfuscated release this is
//     the only step that can succeed, since no delivered name resembles a par2
//     name (#492).
//
//  3. Under -full, whether the identified files then VERIFY by CRC32. This is
//     the half that needs the whole payload.
//
// It was written to indict the shipped behaviour and now measures the
// replacement. The section that printed what the old name-matching verifier
// concluded is gone with the code it probed; see the note where it stood.
//
// Without -full it downloads the par2 index and one article per file. With
// -full it downloads everything, which is the point: the CRC half cannot be
// exercised any other way.
//
// This is a diagnostic, not a gate. It reaches the network and needs real
// credentials, so nothing in CI runs it and it takes both paths as arguments.
//
// Usage:
//
//	go run ./scripts/nzbprobe -config ~/.config/gonzbd/gonzbd.yaml -nzb some.nzb.gz -download
//	go run ./scripts/nzbprobe -config ~/.config/gonzbd/gonzbd.yaml -nzb some.nzb.gz -download -full
package main

import (
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/par2"
)

func main() {
	cfgPath := flag.String("config", "", "path to gonzbd.yaml with real servers")
	nzbPath := flag.String("nzb", "", "path to the .nzb or .nzb.gz to probe")
	download := flag.Bool("download", false, "fetch the par2 index from the servers in -config")
	full := flag.Bool("full", false, "also download every payload article, so the CRC half of identify-then-verify can be exercised")
	flag.Parse()

	if *nzbPath == "" {
		fmt.Fprintln(os.Stderr, "nzbprobe: -nzb is required")
		os.Exit(2)
	}

	doc, err := parseNZB(*nzbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nzbprobe: parse %s: %v\n", *nzbPath, err)
		os.Exit(1)
	}

	fmt.Printf("=== NZB: %s\n", filepath.Base(*nzbPath))
	fmt.Printf("%d file(s), %d group(s)\n\n", len(doc.Files), len(doc.Groups))

	fmt.Printf("%-52s %14s %9s  %s\n", "SUBJECT (parsed filename)", "DECLARED BYTES", "ARTICLES", "KIND")
	fmt.Println(strings.Repeat("-", 100))

	var totalDeclared int64
	for _, f := range doc.Files {
		name := deliveredName(f.Subject)
		kind := "content"
		switch {
		case isRecoveryVolume(name):
			kind = "par2 recovery volume"
		case strings.HasSuffix(strings.ToLower(name), ".par2"):
			kind = "par2 INDEX"
		}
		fmt.Printf("%-52s %14d %9d  %s\n", truncate(name, 52), f.Bytes, len(f.Articles), kind)
		totalDeclared += f.Bytes
	}
	fmt.Printf("\ntotal declared: %d bytes (%.1f MiB)\n", totalDeclared, float64(totalDeclared)/(1024*1024))

	if !*download {
		fmt.Println("\n(-download not set: stopping before any network access)")
		return
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "nzbprobe: -download requires -config")
		os.Exit(2)
	}
	if err := probeIndex(*cfgPath, doc, *full); err != nil {
		fmt.Fprintf(os.Stderr, "nzbprobe: %v\n", err)
		os.Exit(1)
	}
}

func parseNZB(path string) (*nzb.NZB, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied diagnostic input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var r io.Reader = f
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		gz, gzErr := gzip.NewReader(f)
		if gzErr != nil {
			return nil, fmt.Errorf("gzip: %w", gzErr)
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	return nzb.Parse(r)
}

// deliveredName is the name production uses for a delivered file.
//
// nzb.File.Subject is NOT the raw subject line: convertFile already ran it
// through ExtractFilenameFromSubject and SanitizeFilename, and Manifest
// carries that result through to Manifest.FileSubject — which is exactly what
// both call sites hand to par2 as AssembledFile.FileName. Re-extracting a
// quoted run here would probe a name production never sees.
//
// The distinction is not academic for this NZB. ExtractFilenameFromSubject
// REFUSES a purely-hex name — reExcessiveObfuscation is
// `^[0-9a-f]{16,}\.[a-z0-9]{2,4}$` — so "a0576589….tar" is rejected and the
// whole subject is used instead, sanitized to "[1_1]". The .volN+M.par2 names
// escape the same rule only because "+" and the extra dot break the pattern.
// So the name par2 is asked to match against is a part counter.
func deliveredName(subject string) string { return subject }

// productionName reproduces what both call sites hand par2 as
// AssembledFile.FileName:
//
//	name := m.FileSubject(fi)
//	if fn := p.FileFilename(fi); fn != "" { name = fn }
//
// FileFilename is the yEnc-declared name recorded during assembly, so once a
// file has been downloaded it wins over the subject. Probing the subject
// alone would compare a string production never uses.
func productionName(f nzb.File, yenc string) string {
	if yenc != "" {
		return yenc
	}
	return f.Subject
}

func isRecoveryVolume(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".par2") && strings.Contains(l, ".vol")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func sortedNames(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// probeIndex downloads the par2 INDEX only, parses it, and prints the two
// comparisons this tool exists for.
func probeIndex(cfgPath string, doc *nzb.NZB, full bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	servers := cfg.GetServers()
	if len(servers) == 0 {
		return errors.New("config has no servers")
	}

	budget := 3 * time.Minute
	if full {
		budget = 45 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	var conn *nntp.Conn
	for _, s := range servers {
		if !s.Enable {
			continue
		}
		c, dErr := nntp.Dial(ctx, s)
		if dErr != nil {
			fmt.Printf("  server %s: dial failed: %v\n", s.Host, dErr)
			continue
		}
		fmt.Printf("  connected: %s\n", s.Host)
		conn = c
		break
	}
	if conn == nil {
		return errors.New("no server accepted a connection")
	}
	defer func() { _ = conn.Close() }()

	// One article per file, to learn the yEnc-declared name and size.
	//
	// This is what production actually matches on. Both par2 consumers — the
	// on-demand fetch decision and the quickcheck stage — build their
	// par2.AssembledFile list from a name that prefers FileProgress.Filename
	// over Manifest.FileSubject when it is set (app.resolvedName), and
	// Filename comes from the yEnc header — so probing Subject alone would
	// compare a name the real code never uses. For this
	// NZB the difference is total: Subject for the payload is the whole
	// sanitized subject line, because ExtractFilenameFromSubject refuses its
	// purely-hex name.
	fmt.Printf("\n=== yEnc headers (one article per file)\n")
	fmt.Printf("%-46s %16s  %s\n", "yEnc name=", "yEnc size=", "NZB DECLARED")
	fmt.Println(strings.Repeat("-", 88))
	yencName := make(map[int]string, len(doc.Files))
	// Retained so the identification experiment below can hash the first 16 KB
	// without a second fetch. Only the part at offset 0 is useful for that.
	headData := make(map[int][]byte, len(doc.Files))
	for i := range doc.Files {
		f := &doc.Files[i]
		if len(f.Articles) == 0 {
			continue
		}
		body, fErr := conn.Fetch(ctx, f.Articles[0].ID)
		if fErr != nil {
			fmt.Printf("  (fetch failed for %s: %v)\n", truncate(f.Subject, 30), fErr)
			continue
		}
		art, dErr := decoder.DecodeArticle(body)
		if dErr != nil {
			fmt.Printf("  (decode failed for %s: %v)\n", truncate(f.Subject, 30), dErr)
			continue
		}
		yencName[i] = art.Filename
		if art.Offset == 0 {
			headData[i] = art.Data
		}
		fmt.Printf("%-46s %16d  %14d\n", truncate(art.Filename, 46), art.TotalSize, f.Bytes)
	}

	// The index is found by its yEnc name, not its subject: for this NZB the
	// subject sanitizes to "[1_5] - _3f2…par2_ yEnc (1_1)", which does not
	// end in .par2 at all.
	// Neither name is usable for this: the yEnc names carry no extension at
	// all, and the subject-derived name has been sanitized into a whole
	// subject line. What survives is that the SUBJECT still mentions .par2,
	// so classify on that and write the file out under the name the subject
	// claims — which is what has to be on disk for FindPar2Files to see it.
	var index *nzb.File
	var indexName string
	for i := range doc.Files {
		s := strings.ToLower(doc.Files[i].Subject)
		if strings.Contains(s, ".par2") && !strings.Contains(s, ".vol") {
			index = &doc.Files[i]
			indexName = "index.par2"
			break
		}
	}
	if index == nil {
		return errors.New("no par2 index file in this NZB")
	}
	fmt.Printf("\n=== downloading par2 index: %s (%d article(s))\n", indexName, len(index.Articles))

	// Reassemble the index from its parts, in offset order.
	parts := make(map[int64][]byte, len(index.Articles))
	var total int64
	for _, a := range index.Articles {
		body, fErr := conn.Fetch(ctx, a.ID)
		if fErr != nil {
			return fmt.Errorf("fetch %s: %w", a.ID, fErr)
		}
		art, dErr := decoder.DecodeArticle(body)
		if dErr != nil {
			return fmt.Errorf("decode %s: %w", a.ID, dErr)
		}
		parts[art.Offset] = art.Data
		if end := art.Offset + int64(len(art.Data)); end > total {
			total = end
		}
	}
	assembled := make([]byte, total)
	for off, data := range parts {
		copy(assembled[off:], data)
	}
	fmt.Printf("  assembled %d bytes\n", len(assembled))

	tmp, err := os.MkdirTemp("", "nzbprobe")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	indexPath := filepath.Join(tmp, indexName)
	if wErr := os.WriteFile(indexPath, assembled, 0o600); wErr != nil {
		return wErr
	}

	descs, err := par2.ParseFileDescriptions(indexPath)
	if err != nil {
		return fmt.Errorf("parse par2 index: %w", err)
	}

	// --- Question 1: NZB declared bytes vs par2's recorded length ---
	fmt.Printf("\n=== what par2 says the files are\n")
	fmt.Printf("%-52s %14s\n", "PAR2 FileDesc.FileName", "FileSize")
	fmt.Println(strings.Repeat("-", 70))
	par2Sizes := make(map[string]uint64, len(descs))
	for _, d := range descs {
		fmt.Printf("%-52s %14d\n", truncate(d.FileName, 52), d.FileSize)
		par2Sizes[d.FileName] = d.FileSize
	}

	// Why matchCRCSize is dead: its key is {crc, size} with exact equality,
	// and these two columns are the sizes it compares. par2 records the
	// decoded length; the NZB declares the yEnc-encoded byte count.
	fmt.Printf("\n=== NZB declared bytes vs par2's recorded length\n")
	fmt.Printf("%-34s %14s %14s %9s  %s\n", "NZB FILE", "NZB DECLARED", "PAR2 SIZE", "RATIO", "PAR2 NAME")
	fmt.Println(strings.Repeat("-", 100))
	for i, f := range doc.Files {
		name := productionName(f, yencName[i])
		if strings.HasSuffix(strings.ToLower(name), ".par2") {
			continue
		}
		for _, pn := range sortedNames(par2Sizes) {
			ps := par2Sizes[pn]
			fmt.Printf("%-34s %14d %14d %8.4fx  %s\n",
				truncate(name, 34), f.Bytes, ps, float64(f.Bytes)/float64(ps), truncate(pn, 30))
		}
	}

	// The "what does the shipped code conclude" section that used to sit here
	// is gone, and its absence is the point.
	//
	// It printed par2.VerifyCRCs' counters over the real set and then evaluated
	// the two guards those counters fed: "Matched==0 && Mismatched==0 &&
	// NoCRC==0", which discarded the recovery volumes, and
	// "NoCRC+Unverified+Mismatched > 0", which marked the job damaged. Both are
	// what this probe was written to indict (#492), and neither exists any
	// more: verification no longer matches par2 entries by name, so there is no
	// standalone VerifyCRCs to call and no ambiguous counter to evaluate.
	//
	// What follows is what the code now does, not a proposal.
	return probeIdentifyThenVerify(ctx, conn, doc, descs, yencName, headData, tmp, full)
}

// probeIdentifyThenVerify exercises the proposed algorithm against the same
// real set: identify each delivered file by Hash16k, then verify the
// identified file by CRC32.
//
// The two halves have very different costs and are reported separately for
// that reason. Identification needs 16 KB per file and runs always;
// verification needs the whole file and runs only under -full. A run without
// -full still answers the question that matters most — whether the delivered
// files can be identified at all — because that is the step the current code
// never reaches.
func probeIdentifyThenVerify(
	ctx context.Context,
	conn *nntp.Conn,
	doc *nzb.NZB,
	descs []par2.FileDesc,
	yencName map[int]string,
	headData map[int][]byte,
	tmp string,
	full bool,
) error {
	byHash := make(map[[16]byte]par2.FileDesc, len(descs))
	for _, d := range descs {
		byHash[d.Hash16k] = d
	}

	fmt.Printf("\n=== PROPOSED: phase 1 — identify by Hash16k (16 KB per file)\n")
	fmt.Printf("%-34s %-34s %s\n", "DELIVERED NAME", "IDENTIFIED AS", "RESULT")
	fmt.Println(strings.Repeat("-", 96))

	hashDir := filepath.Join(tmp, "payload")
	if err := os.MkdirAll(hashDir, 0o750); err != nil {
		return err
	}

	identified := make(map[int]par2.FileDesc)
	var contentFiles, idOK int
	for i, f := range doc.Files {
		name := productionName(f, yencName[i])
		if strings.Contains(strings.ToLower(f.Subject), ".par2") {
			continue // par2's own files are not protected by it
		}
		contentFiles++

		head, ok := headData[i]
		if !ok {
			fmt.Printf("%-34s %-34s %s\n", truncate(name, 34), "-", "no offset-0 article fetched")
			continue
		}

		// Written to disk and hashed through the production helper rather
		// than an inline md5, so this probes ComputeHash16k itself — the
		// function the real matcher would call.
		p := filepath.Join(hashDir, fmt.Sprintf("delivered_%02d", i))
		if err := os.WriteFile(p, head, 0o600); err != nil {
			return err
		}
		h, err := par2.ComputeHash16k(p)
		if err != nil {
			return fmt.Errorf("hash16k %s: %w", name, err)
		}
		// head is ONE article's payload, not the file. par2's Hash16k covers
		// the first 16 KiB of the whole file (or all of it, when smaller), so
		// a first article carrying fewer than 16 KiB of a larger file hashes a
		// prefix par2 never recorded. Reporting that as "no Hash16k match"
		// would be a probe artefact indistinguishable from the real verdict,
		// and this tool exists to be believed about exactly that.
		//
		// Gated on the file having more than one segment: a single-segment
		// file IS its first article, so a short head is the whole thing and
		// the hash is the one par2 recorded.
		const hash16kLen = 16 * 1024
		if len(head) < hash16kLen && len(f.Articles) > 1 {
			fmt.Printf("%-34s %-34s %s\n", truncate(name, 34), "-",
				fmt.Sprintf("inconclusive: first article is %d B, under the %d B par2 hashes", len(head), hash16kLen))
			continue
		}
		if d, hit := byHash[h]; hit {
			identified[i] = d
			idOK++
			fmt.Printf("%-34s %-34s %s\n", truncate(name, 34), truncate(d.FileName, 34), "MATCH")
			continue
		}
		fmt.Printf("%-34s %-34s %s\n", truncate(name, 34), "-", "no Hash16k match")
	}

	fmt.Printf("\n  identified %d of %d delivered content file(s)\n", idOK, contentFiles)
	fmt.Printf("  par2 index describes %d file(s); %d accounted for\n", len(descs), idOK)

	// Distinct par2 ENTRIES matched, not delivered files that matched one.
	// Two delivered files sharing their first 16 KiB both match the same
	// FileDesc, so idOK can reach len(descs) with entries still unclaimed —
	// and the probe would print "every par2 entry accounted for: true" where
	// production, which asks id.Accounted() over distinct entries, fetches.
	// A diagnostic that can disagree with the code it exists to predict is
	// worse than none.
	accounted := make(map[[16]byte]struct{}, len(identified))
	for _, d := range identified {
		accounted[d.Hash16k] = struct{}{}
	}
	allAccounted := len(accounted) == len(descs)
	fmt.Printf("  every par2 entry accounted for: %v (%d distinct entr(y/ies) matched)\n",
		allAccounted, len(accounted))
	if !allAccounted {
		fmt.Printf("    -> proposed design: fetch ALL recovery volumes, full par2 repair\n")
	}

	if !full {
		fmt.Printf("\n=== PROPOSED: phase 2 — verify by CRC32\n")
		fmt.Printf("  (skipped: needs the whole payload; re-run with -full)\n")
		return nil
	}

	fmt.Printf("\n=== PROPOSED: phase 2 — verify by CRC32 (downloading full payload)\n")
	fmt.Printf("%-34s %10s %10s  %s\n", "IDENTIFIED AS", "PAR2 CRC", "OURS", "RESULT")
	fmt.Println(strings.Repeat("-", 78))

	var verified, mismatched, noPar2CRC int
	for i, d := range identified {
		data, err := assembleFile(ctx, conn, &doc.Files[i])
		if err != nil {
			fmt.Printf("%-34s %10s %10s  %v\n", truncate(d.FileName, 34), "-", "-", err)
			continue
		}
		if uint64(len(data)) != d.FileSize {
			fmt.Printf("  note: %s assembled to %d bytes, par2 says %d\n",
				truncate(d.FileName, 30), len(data), d.FileSize)
		}
		ours := crc32.ChecksumIEEE(data)
		switch d.FileCRC32 {
		case 0:
			noPar2CRC++
			fmt.Printf("%-34s %10s %08x  no IFSC data in set — unverifiable\n", truncate(d.FileName, 34), "-", ours)
		case ours:
			verified++
			fmt.Printf("%-34s %08x   %08x  VERIFIED\n", truncate(d.FileName, 34), d.FileCRC32, ours)
		default:
			mismatched++
			fmt.Printf("%-34s %08x   %08x  MISMATCH\n", truncate(d.FileName, 34), d.FileCRC32, ours)
		}
	}

	fmt.Printf("\n=== PROPOSED: verdict\n")
	fmt.Printf("  identified=%d verified=%d mismatched=%d noPar2CRC=%d\n",
		idOK, verified, mismatched, noPar2CRC)
	switch {
	case allAccounted && mismatched == 0 && noPar2CRC == 0:
		fmt.Printf("  -> quickcheck PASSES: rename the identified files, skip recovery volumes, unpack\n")
	default:
		fmt.Printf("  -> fetch ALL recovery volumes, full par2 repair\n")
	}
	return nil
}

// assembleFile downloads every article of one NZB file and returns the
// decoded payload in offset order.
func assembleFile(ctx context.Context, conn *nntp.Conn, f *nzb.File) ([]byte, error) {
	parts := make(map[int64][]byte, len(f.Articles))
	var total int64
	for _, a := range f.Articles {
		body, err := conn.Fetch(ctx, a.ID)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", a.ID, err)
		}
		art, err := decoder.DecodeArticle(body)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", a.ID, err)
		}
		parts[art.Offset] = art.Data
		if end := art.Offset + int64(len(art.Data)); end > total {
			total = end
		}
	}
	out := make([]byte, total)
	for off, data := range parts {
		copy(out[off:], data)
	}
	return out, nil
}

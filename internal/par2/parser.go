package par2

import (
	"bytes"
	"crypto/md5" //nolint:gosec // md5 is used for packet integrity by PAR2 spec
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/crc32util"
)

// correctEncoding converts raw par2 filename bytes to a valid UTF-8 string,
// NFC-normalized. Par2 files created on Windows often contain ISO-8859-1
// (Latin-1) encoded filenames rather than UTF-8.
//
// The logic mirrors SABnzbd's correct_unknown_encoding():
//  1. If the bytes are valid UTF-8 → use as-is, NFC-normalize.
//  2. Otherwise → decode as ISO-8859-1 (each byte maps directly to its
//     Unicode codepoint U+0000..U+00FF), then NFC-normalize.
func correctEncoding(raw []byte) string {
	if utf8.Valid(raw) {
		return norm.NFC.String(string(raw))
	}
	// ISO-8859-1: each byte b maps to rune(b). This is a lossless
	// superset of ASCII that covers Western European characters.
	runes := make([]rune, len(raw))
	for i, b := range raw {
		runes[i] = rune(b)
	}
	return norm.NFC.String(string(runes))
}

// FileDesc contains metadata about a file protected by a PAR2 set.
type FileDesc struct {
	FileID       [16]byte // unique file identifier within the recovery set
	FileName     string
	FullHash     [16]byte // MD5 of the entire file
	Hash16k      [16]byte // MD5 of first 16KB
	FileSize     uint64
	FileCRC32    uint32 // reconstructed from IFSC slices (0 if no IFSC data)
	HasDuplicate bool   // true if another FileDesc shares this Hash16k
	// Set is the Name of the first par2 Set that parsed as a copy of this
	// entry's set (copies of one set share it), so an Identification over
	// several sets can be judged set by set. It is set by
	// collectManifestsWithOptions, which is how Identify reads entries;
	// ParseFileDescriptions leaves it empty.
	Set string
	// Undescribed marks an entry for a file the set's Main packet lists and
	// no File Description packet describes. Only FileID and Set are read from
	// the set; FileName is a placeholder naming the FileID, not a name par2
	// records, so nothing may be matched against it.
	Undescribed bool
}

// ParsedSet aggregates all information parsed from a PAR2 file's packets.
type ParsedSet struct {
	SetID     [16]byte
	SliceSize uint64
	// RecoveryFileIDs is the recovery set the Main packet lists: the files
	// the set protects, whether or not a File Description packet for each
	// survived. Nil without a Main packet, or when its body is shorter than
	// the count it declares.
	RecoveryFileIDs [][16]byte
	Files           []FileDesc             // all file descriptions
	FilesByID       map[[16]byte]*FileDesc // keyed by FileID for IFSC lookup
	By16k           map[[16]byte]*FileDesc // keyed by Hash16k (entries removed if HasDuplicate)
	RecoveryBlocks  int
	Creator         string
}

// ifscData holds raw IFSC slice data before CRC reconstruction.
type ifscData struct {
	fileID [16]byte
	slices []ifscSlice
}

type ifscSlice struct {
	md5Hash [16]byte
	crc32   uint32
}

// Sentinel errors returned when PAR2 limit safety thresholds are exceeded.
var (
	// ErrPacketBodySizeExceeded indicates that a PAR2 packet body exceeds configured size limits.
	ErrPacketBodySizeExceeded = errors.New("par2 packet body size exceeds limit")
	// ErrJunkScanLimitExceeded indicates that searching for a PAR2 packet magic signature exceeded the configured scan limit.
	ErrJunkScanLimitExceeded = errors.New("par2 junk header scan limit exceeded")
)

var (
	magic        = []byte("PAR2\x00PKT")
	typeMain     = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'M', 'a', 'i', 'n', 0x00, 0x00, 0x00, 0x00}
	typeFileDesc = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'F', 'i', 'l', 'e', 'D', 'e', 's', 'c'}
	typeIFSC     = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'I', 'F', 'S', 'C', 0x00, 0x00, 0x00, 0x00}
	typeRecovery = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'R', 'e', 'c', 'v', 'S', 'l', 'i', 'c'}
	typeCreator  = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'C', 'r', 'e', 'a', 't', 'o', 'r', 0x00}
)

// The bounds are constants.DefaultPar2MaxJunkScanBytes,
// constants.DefaultPar2MaxPacketBodySize and their floors, which config
// validates the configured values against.
const (
	defaultMaxJunkScan       = constants.DefaultPar2MaxJunkScanBytes
	defaultMaxPacketBodySize = constants.DefaultPar2MaxPacketBodySize
)

// ParseOptions defines configurable safety and memory bounds for parsing PAR2 sets.
// Zero values trigger standard safety defaults (64 KiB junk scan, 64 MiB max packet body size).
// ParseOptions structs are immutable once constructed and safe for concurrent use across goroutines.
type ParseOptions struct {
	// MaxJunkScanBytes is the maximum number of bytes to scan forward when looking for magic header signatures.
	MaxJunkScanBytes int64

	// MaxPacketBodySize is the maximum permitted allocation size in bytes for an individual PAR2 packet body.
	MaxPacketBodySize int64
}

// DefaultParseOptions returns standard safety limits for PAR2 parsing (64 KiB junk scan, 64 MiB packet size cap).
// The returned ParseOptions struct is safe for concurrent use.
func DefaultParseOptions() ParseOptions {
	return ParseOptions{
		MaxJunkScanBytes:  defaultMaxJunkScan,
		MaxPacketBodySize: defaultMaxPacketBodySize,
	}
}

// ParseOptionsFromConfig constructs ParseOptions from PostProcConfig.
// If cfg is nil, DefaultParseOptions() is returned.
func ParseOptionsFromConfig(cfg *config.PostProcConfig) ParseOptions {
	if cfg == nil {
		return DefaultParseOptions()
	}
	return ParseOptions{
		MaxJunkScanBytes:  cfg.Par2MaxJunkScanBytes,
		MaxPacketBodySize: cfg.Par2MaxPacketBodySize,
	}
}

// ParsePar2Set reads path (a .par2 file) using safety limits.
// Optional ParseOptions can be supplied to override defaults.
func ParsePar2Set(path string, opts ...ParseOptions) (*ParsedSet, error) {
	parseOpts := DefaultParseOptions()
	if len(opts) > 0 {
		parseOpts = opts[0]
	}
	return ParsePar2SetWithOptions(path, parseOpts)
}

// ParsePar2SetWithOptions reads path (a .par2 file) using caller-specified ParseOptions bounds.
// It returns a *ParsedSet or an error if packet allocations exceed opts.MaxPacketBodySize or I/O fails.
func ParsePar2SetWithOptions(path string, opts ParseOptions) (*ParsedSet, error) {
	if opts.MaxJunkScanBytes < constants.MinPar2JunkScanBytes {
		opts.MaxJunkScanBytes = defaultMaxJunkScan
	}
	if opts.MaxPacketBodySize < constants.MinPar2PacketBodySize {
		opts.MaxPacketBodySize = defaultMaxPacketBodySize
	}

	f, err := os.Open(path) //nolint:gosec // path is constructed from trusted readdir
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() < 0 {
		return nil, fmt.Errorf("negative file size: %d", fi.Size())
	}
	fileSize := uint64(fi.Size()) //nolint:gosec // G115: guarded above

	set := &ParsedSet{
		FilesByID: make(map[[16]byte]*FileDesc),
		By16k:     make(map[[16]byte]*FileDesc),
	}
	var ifscPackets []ifscData

	// earlyExitThreshold is the file size above which we stop scanning once all
	// FileDesc + IFSC packets are seen. Recovery blocks are huge and irrelevant
	// for rename/CRC purposes. Spec §4.5.
	const earlyExitThreshold = 10 * 1024 * 1024 // 10 MiB

	var (
		expectedFiles int // -1 = unknown (Main not yet parsed), 0+ = from Main body
		seenFileDescs int
		seenIFSC      int
	)
	expectedFiles = -1

	header := make([]byte, 64)
	validPackets := 0
	for {
		packetType, body, err := readNextPacketWithOptions(f, fileSize, header, opts)
		if errors.Is(err, io.EOF) || (errors.Is(err, io.ErrUnexpectedEOF) && validPackets > 0) {
			break
		}
		if err != nil {
			return nil, err
		}
		validPackets++

		handlePacket(set, &ifscPackets, packetType, header, body, &expectedFiles, &seenFileDescs, &seenIFSC)

		// Scan optimization (spec §4.5): for large par2 files, stop once
		// all FileDesc + IFSC packets have been seen. Recovery blocks are
		// huge and irrelevant for rename/CRC purposes.
		if expectedFiles > 0 && fileSize > earlyExitThreshold &&
			seenFileDescs >= expectedFiles && seenIFSC >= expectedFiles {
			break
		}
	}

	postProcessSet(set, ifscPackets)

	return set, nil
}

// isValidPacketLen reports whether packetLen meets the PAR 2.0 specification:
// minimum header size (64 bytes), upper-bounded by fileSize, and 4-byte aligned.
func isValidPacketLen(packetLen, fileSize uint64) bool {
	return packetLen >= 64 && packetLen <= fileSize && packetLen%4 == 0
}

// rewindToEmbeddedMagic inspects the already-read bytes after the candidate
// magic start (header[1:64] followed by body) when a candidate packet fails
// MD5 verification. If another PAR2 packet magic starts inside those bytes
// (because the failed candidate was a false magic match or had a corrupted
// length that overshot the next packet), it rewinds f to that embedded magic;
// if the tail of the read bytes ends with a partial prefix of magic (1..7
// bytes), it rewinds by that prefix length so the next read does not split a
// boundary-straddling magic. When neither holds (for example, a multi-MiB
// recovery slice with a single corrupted body byte), the file offset stays at
// the end of body so the parser advances to the next packet without tripping
// MaxJunkScanBytes.
//
// If seeking back to the candidate position would leave fewer than 64 bytes
// before fileSize (where no valid packet header can fit), it avoids rewinding
// into the trailing short tail, allowing the parser to read to EOF cleanly.
func rewindToEmbeddedMagic(f *os.File, fileSize uint64, header, body []byte) error {
	curPos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	fitsHeader := func(backtrack int64) bool {
		return curPos >= backtrack && uint64(curPos-backtrack)+64 <= fileSize //nolint:gosec // G115: curPos >= backtrack ensures non-negative
	}
	totalAfterStart := 63 + len(body)
	// Check magic starting inside header[1:64] (which may straddle up to 7
	// bytes into body).
	boundOverlap := min(len(body), len(magic)-1)
	var boundary [63 + 7]byte
	copy(boundary[:63], header[1:64])
	copy(boundary[63:63+boundOverlap], body[:boundOverlap])
	if idx := bytes.Index(boundary[:63+boundOverlap], magic); idx >= 0 {
		backtrack := int64(totalAfterStart - idx)
		if fitsHeader(backtrack) {
			_, err := f.Seek(-backtrack, io.SeekCurrent)
			return err
		}
		return nil
	}
	if idx := bytes.Index(body, magic); idx >= 0 {
		backtrack := int64(len(body) - idx)
		if fitsHeader(backtrack) {
			_, err := f.Seek(-backtrack, io.SeekCurrent)
			return err
		}
		return nil
	}
	// Check whether the last 1..7 bytes of (header[1:64] + body) match a
	// prefix of magic that could complete in the bytes immediately following
	// body.
	var tail [7]byte
	tailLen := min(len(magic)-1, totalAfterStart)
	if len(body) >= tailLen {
		copy(tail[:tailLen], body[len(body)-tailLen:])
	} else {
		fromHdr := tailLen - len(body)
		copy(tail[:fromHdr], header[64-fromHdr:64])
		copy(tail[fromHdr:tailLen], body)
	}
	for k := tailLen; k >= 1; k-- {
		if bytes.Equal(tail[tailLen-k:tailLen], magic[:k]) {
			backtrack := int64(k)
			if fitsHeader(backtrack) {
				_, err := f.Seek(-backtrack, io.SeekCurrent)
				return err
			}
			return nil
		}
	}
	return nil
}

// readNextPacket reads the next valid packet from the par2 file.
func readNextPacket(f *os.File, fileSize uint64, header []byte) (packetType [16]byte, body []byte, err error) {
	return readNextPacketWithOptions(f, fileSize, header, DefaultParseOptions())
}

// readNextPacketWithOptions reads the next valid packet using configured limits.
func readNextPacketWithOptions(f *os.File, fileSize uint64, header []byte, opts ParseOptions) (packetType [16]byte, body []byte, err error) {
	if opts.MaxPacketBodySize < constants.MinPar2PacketBodySize {
		opts.MaxPacketBodySize = defaultMaxPacketBodySize
	}
	for {
		_, err := io.ReadFull(f, header)
		if err != nil {
			return [16]byte{}, nil, err
		}

		if !bytes.Equal(header[0:8], magic) {
			// Scan forward up to MaxJunkScanBytes to find the next magic.
			if _, scanErr := scanForMagicWithOptions(f, magic, opts.MaxJunkScanBytes); scanErr != nil {
				return [16]byte{}, nil, scanErr
			}
			continue
		}

		packetLen := binary.LittleEndian.Uint64(header[8:16])
		if !isValidPacketLen(packetLen, fileSize) {
			slog.Warn("par2: invalid packet length, scanning for next packet", "length", packetLen, "file_size", fileSize)
			if _, scanErr := scanForMagicWithOptions(f, magic, opts.MaxJunkScanBytes); scanErr != nil {
				return [16]byte{}, nil, scanErr
			}
			continue
		}

		bodyLen := packetLen - 64
		if bodyLen > uint64(opts.MaxPacketBodySize) { //nolint:gosec // G115: clamped to at least constants.MinPar2PacketBodySize above
			return [16]byte{}, nil, fmt.Errorf("par2 packet body size (%d bytes) exceeds configured limit (%d bytes); increase 'par2_max_packet_body_size' in Settings -> Post-Processing if this is a valid ultra-large file: %w", bodyLen, opts.MaxPacketBodySize, ErrPacketBodySizeExceeded)
		}
		body := make([]byte, bodyLen)
		if n, err := io.ReadFull(f, body); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				slog.Warn("par2: packet extends past end of file, scanning for next packet", "length", packetLen, "file_size", fileSize)
				if _, seekErr := f.Seek(-int64(n), io.SeekCurrent); seekErr != nil {
					return [16]byte{}, nil, seekErr
				}
				if _, scanErr := scanForMagicWithOptions(f, magic, opts.MaxJunkScanBytes); scanErr != nil {
					return [16]byte{}, nil, scanErr
				}
				continue
			}
			return [16]byte{}, nil, fmt.Errorf("read body: %w", err)
		}

		if !validatePacketMD5(header, body) {
			slog.Warn("par2: packet MD5 mismatch, scanning for next packet", "length", packetLen)
			if seekErr := rewindToEmbeddedMagic(f, fileSize, header, body); seekErr != nil {
				return [16]byte{}, nil, seekErr
			}
			continue
		}

		var packetType [16]byte
		copy(packetType[:], header[48:64])
		return packetType, body, nil
	}
}

// handlePacket parses the content of a single packet based on its type.
func handlePacket(
	set *ParsedSet,
	ifscPackets *[]ifscData,
	packetType [16]byte,
	header []byte,
	body []byte,
	expectedFiles, seenFileDescs, seenIFSC *int,
) {
	switch packetType {
	case typeMain:
		if len(body) < 8 {
			return
		}
		set.SliceSize = binary.LittleEndian.Uint64(body[0:8])
		copy(set.SetID[:], header[32:48])

		// Main packet body after sliceSize(8) + recoverySetCount(4) is
		// an array of 16-byte FileID entries.
		if len(body) > 12 {
			*expectedFiles = (len(body) - 12) / 16
		}
		set.RecoveryFileIDs = recoveryFileIDs(body)

	case typeFileDesc:
		fd := parseFileDescBody(body)
		if fd == nil {
			return
		}
		set.Files = append(set.Files, *fd)
		*seenFileDescs++

	case typeIFSC:
		idata := parseIFSCBody(body)
		if idata != nil {
			*ifscPackets = append(*ifscPackets, *idata)
			*seenIFSC++
		}

	case typeRecovery:
		set.RecoveryBlocks++

	case typeCreator:
		set.Creator = string(bytes.TrimRight(body, "\x00"))
	}
}

// maxRecoverySetFiles bounds a Main packet's recovery set count, which the
// post supplies. PAR2 allows at most 32768 input slices and each file in the
// recovery set has at least one, so a larger count is not a PAR2 set.
const maxRecoverySetFiles = 32768

// recoveryFileIDs returns the recovery set FileIDs of a Main packet body:
// sliceSize(8), count(4), then count IDs, then the non-recovery set's IDs,
// which par2 does not protect. It returns nil, and logs, when the body cannot
// hold count IDs or count exceeds maxRecoverySetFiles; the set's files are
// then only those it describes.
func recoveryFileIDs(body []byte) [][16]byte {
	if len(body) < 12 {
		slog.Warn("par2: Main packet has no recovery set count", "body_len", len(body))
		return nil
	}
	n := binary.LittleEndian.Uint32(body[8:12])
	if int64(n) > int64(len(body)-12)/16 {
		slog.Warn("par2: Main packet declares more recovery files than it lists",
			"declared", n, "listed", (len(body)-12)/16)
		return nil
	}
	if n > maxRecoverySetFiles {
		slog.Warn("par2: Main packet declares more recovery files than PAR2 allows",
			"declared", n, "limit", maxRecoverySetFiles)
		return nil
	}
	ids := make([][16]byte, n)
	for i := range ids {
		copy(ids[i][:], body[12+16*i:])
	}
	return ids
}

// postProcessSet builds indices and reconstructs CRCs after parsing all packets.
func postProcessSet(set *ParsedSet, ifscPackets []ifscData) {
	// Build FilesByID index.
	for i := range set.Files {
		set.FilesByID[set.Files[i].FileID] = &set.Files[i]
	}

	// Reconstruct per-file CRC32 from IFSC data.
	if set.SliceSize > 0 {
		reconstructCRCs(set, ifscPackets)
	}

	// Build By16k map with deduplication.
	buildBy16k(set)
}

// parseFileDescBody parses the body of a FileDesc packet.
// Body layout: FileID(16) + FullHash(16) + Hash16k(16) + FileSize(8) + FileName(var)
func parseFileDescBody(body []byte) *FileDesc {
	if len(body) < 56 {
		return nil
	}

	fd := &FileDesc{}
	copy(fd.FileID[:], body[0:16])
	copy(fd.FullHash[:], body[16:32])
	copy(fd.Hash16k[:], body[32:48])
	fd.FileSize = binary.LittleEndian.Uint64(body[48:56])
	if len(body) > 56 {
		fd.FileName = correctEncoding(bytes.TrimRight(body[56:], "\x00"))
	}
	return fd
}

// parseIFSCBody parses the body of an IFSC (Input File Slice Checksum) packet.
// Body layout: FileID(16), then repeated pairs of (MD5[16] + CRC32[4]).
func parseIFSCBody(body []byte) *ifscData {
	if len(body) < 16 {
		return nil
	}

	data := &ifscData{}
	copy(data.fileID[:], body[0:16])

	remaining := body[16:]
	const sliceEntrySize = 20 // 16 (MD5) + 4 (CRC32)

	for len(remaining) >= sliceEntrySize {
		var s ifscSlice
		copy(s.md5Hash[:], remaining[0:16])
		s.crc32 = binary.LittleEndian.Uint32(remaining[16:20])
		data.slices = append(data.slices, s)
		remaining = remaining[sliceEntrySize:]
	}

	return data
}

// reconstructCRCs computes the per-file CRC32 from IFSC slice CRCs using
// crc32util.Combine and crc32util.ZeroUnpad for the tail slice.
func reconstructCRCs(set *ParsedSet, ifscPackets []ifscData) {
	for _, idata := range ifscPackets {
		fd, ok := set.FilesByID[idata.fileID]
		if !ok || fd.FileSize == 0 || len(idata.slices) == 0 {
			continue
		}

		var combined uint32
		numSlices := len(idata.slices)

		for i, s := range idata.slices {
			if i == numSlices-1 {
				// Last slice — may be shorter than SliceSize.
				tail := fd.FileSize % set.SliceSize
				if tail == 0 {
					// Last slice is a full slice.
					combined = crc32util.Combine(combined, s.crc32, int64(set.SliceSize)) //nolint:gosec // slice sizes fit int64
				} else {
					// Tail slice: the stored CRC covers SliceSize bytes
					// (zero-padded). Remove the padding contribution.
					unpadded := crc32util.ZeroUnpad(s.crc32, int64(set.SliceSize-tail)) //nolint:gosec // fits int64
					combined = crc32util.Combine(combined, unpadded, int64(tail))       //nolint:gosec // fits int64
				}
			} else {
				combined = crc32util.Combine(combined, s.crc32, int64(set.SliceSize)) //nolint:gosec // slice sizes fit int64
			}
		}

		fd.FileCRC32 = combined
	}
}

// buildBy16k populates set.By16k and sets HasDuplicate on files that share
// the same Hash16k.
func buildBy16k(set *ParsedSet) {
	// First pass: count occurrences.
	counts := make(map[[16]byte]int)
	for i := range set.Files {
		counts[set.Files[i].Hash16k]++
	}

	// Second pass: mark duplicates and populate map.
	for i := range set.Files {
		h := set.Files[i].Hash16k
		if counts[h] > 1 {
			set.Files[i].HasDuplicate = true
		} else {
			set.By16k[h] = &set.Files[i]
		}
	}
}

// ParseFileDescriptions reads path (a .par2 file) and returns all File Description
// packets found within. Optional ParseOptions can be supplied.
func ParseFileDescriptions(path string, opts ...ParseOptions) ([]FileDesc, error) {
	parseOpts := DefaultParseOptions()
	if len(opts) > 0 {
		parseOpts = opts[0]
	}
	return ParseFileDescriptionsWithOptions(path, parseOpts)
}

// ParseFileDescriptionsWithOptions reads path (a .par2 file) using caller-specified ParseOptions
// and returns all File Description packets found within.
func ParseFileDescriptionsWithOptions(path string, opts ParseOptions) ([]FileDesc, error) {
	set, err := ParsePar2SetWithOptions(path, opts)
	if err != nil {
		return nil, err
	}
	return set.Files, nil
}

// HasMagic reports whether the file at path starts with the par2 packet
// signature. It reads at most len(magic) bytes. A file shorter than that is
// false with a nil error; only a failure to open or read is an error. The
// parser tolerates junk before the first packet, so a file it can parse may
// still report false here: the answer is "identifiably par2 at offset 0", a
// cheap pre-filter, not "parseable".
func HasMagic(path string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // path is a job-owned file chosen by the caller
	if err != nil {
		return false, err
	}
	defer f.Close() //nolint:errcheck // read-only file
	buf := make([]byte, len(magic))
	if _, err := io.ReadFull(f, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return bytes.Equal(buf, magic), nil
}

// scanForMagic scans forward using default junk scan limit to find next magic.
func scanForMagic(f *os.File, magic []byte) (bool, error) {
	return scanForMagicWithOptions(f, magic, defaultMaxJunkScan)
}

// scanForMagicWithOptions scans forward up to maxJunkScan bytes to find the next magic.
func scanForMagicWithOptions(f *os.File, magic []byte, maxJunkScan int64) (bool, error) {
	if maxJunkScan < constants.MinPar2JunkScanBytes {
		maxJunkScan = defaultMaxJunkScan
	}
	if _, seekErr := f.Seek(-63, io.SeekCurrent); seekErr != nil {
		return false, seekErr
	}
	scanBuf := make([]byte, maxJunkScan)
	n, readErr := io.ReadFull(f, scanBuf)
	if n == 0 {
		return false, readErr
	}
	scanBuf = scanBuf[:n]

	idx := bytes.Index(scanBuf, magic)
	if idx >= 0 {
		// Seek to where magic starts, then let the loop re-read the header.
		// Current position is (old_pos - 63 + n).
		// We want to be at (old_pos - 63 + idx).
		backtrack := int64(n - idx)
		if _, seekErr := f.Seek(-backtrack, io.SeekCurrent); seekErr != nil {
			return false, seekErr
		}
		return true, nil
	}
	if int64(n) >= maxJunkScan {
		return false, fmt.Errorf("par2 junk scan exceeded limit (%d bytes) without finding header magic; increase 'par2_max_junk_scan_bytes' in Settings -> Post-Processing if file has a large header: %w", maxJunkScan, ErrJunkScanLimitExceeded)
	}
	return false, readErr
}

// validatePacketMD5 verifies the MD5 checksum of the packet body.
// MD5 is a hash of (setID[16] + type[16] + body).
// header[16:32] = packet MD5
// header[32:48] = recovery set ID
// header[48:64] = packet type
func validatePacketMD5(header, body []byte) bool {
	h := md5.New() //nolint:gosec // md5 is used for packet integrity by PAR2 spec
	h.Write(header[32:48])
	h.Write(header[48:64])
	h.Write(body)
	var computedMD5 [16]byte
	copy(computedMD5[:], h.Sum(nil))
	var storedMD5 [16]byte
	copy(storedMD5[:], header[16:32])
	return computedMD5 == storedMD5
}

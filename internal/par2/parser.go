package par2

import (
	"bytes"
	"crypto/md5" //nolint:gosec // md5 is used for packet integrity by PAR2 spec
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

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
}

// ParsedSet aggregates all information parsed from a PAR2 file's packets.
type ParsedSet struct {
	SetID          [16]byte
	SliceSize      uint64
	Files          []FileDesc             // all file descriptions
	FilesByID      map[[16]byte]*FileDesc // keyed by FileID for IFSC lookup
	By16k          map[[16]byte]*FileDesc // keyed by Hash16k (entries removed if HasDuplicate)
	RecoveryBlocks int
	Creator        string
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

var (
	magic        = []byte("PAR2\x00PKT")
	typeMain     = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'M', 'a', 'i', 'n', 0x00, 0x00, 0x00, 0x00}
	typeFileDesc = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'F', 'i', 'l', 'e', 'D', 'e', 's', 'c'}
	typeIFSC     = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'I', 'F', 'S', 'C', 0x00, 0x00, 0x00, 0x00}
	typeRecovery = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'R', 'e', 'c', 'v', 'S', 'l', 'i', 'c'}
	typeCreator  = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'C', 'r', 'e', 'a', 't', 'o', 'r', 0x00}
)

const maxJunkScan = 64 * 1024 // max bytes to scan forward looking for magic

// ParsePar2Set reads path (a .par2 file) and returns the full ParsedSet with all
// packet types parsed.
func ParsePar2Set(path string) (*ParsedSet, error) {
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
	for {
		packetType, body, err := readNextPacket(f, fileSize, header)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return nil, err
		}

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

// readNextPacket reads the next valid packet from the par2 file.
// It skips junk data by scanning for the magic sequence, validates packet length
// and MD5 checksum, and returns the packet type and body bytes.
func readNextPacket(f *os.File, fileSize uint64, header []byte) (packetType [16]byte, body []byte, err error) {
	for {
		_, err := io.ReadFull(f, header)
		if err != nil {
			return [16]byte{}, nil, err
		}

		if !bytes.Equal(header[0:8], magic) {
			// Scan forward up to maxJunkScan bytes to find the next magic.
			found, scanErr := scanForMagic(f, magic)
			if scanErr != nil {
				return [16]byte{}, nil, scanErr
			}
			if !found {
				return [16]byte{}, nil, io.EOF
			}
			continue
		}

		packetLen := binary.LittleEndian.Uint64(header[8:16])
		if packetLen < 64 || packetLen > fileSize {
			return [16]byte{}, nil, fmt.Errorf("invalid packet length: %d", packetLen)
		}

		bodyLen := packetLen - 64
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(f, body); err != nil {
			return [16]byte{}, nil, fmt.Errorf("read body: %w", err)
		}

		if !validatePacketMD5(header, body) {
			// MD5 mismatch — drop this packet silently and continue.
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
// packets found within. This is a backward-compatible wrapper around ParsePar2Set.
func ParseFileDescriptions(path string) ([]FileDesc, error) {
	set, err := ParsePar2Set(path)
	if err != nil {
		return nil, err
	}
	return set.Files, nil
}

// scanForMagic scans forward up to maxJunkScan bytes to find the next magic.
// Put the last 7 bytes back (magic is 8 bytes, so partial overlap is possible)
// by seeking back 7 bytes from current pos.
// Returns true if magic is found, false if not found or on error.
func scanForMagic(f *os.File, magic []byte) (bool, error) {
	if _, seekErr := f.Seek(-56, io.SeekCurrent); seekErr != nil {
		return false, seekErr
	}
	scanBuf := make([]byte, maxJunkScan)
	n, readErr := f.Read(scanBuf)
	if n == 0 {
		return false, readErr
	}
	scanBuf = scanBuf[:n]

	idx := bytes.Index(scanBuf, magic)
	if idx >= 0 {
		// Seek to where magic starts, then let the loop re-read the header.
		// Current position is (old_pos - 56 + n).
		// We want to be at (old_pos - 56 + idx).
		backtrack := int64(n - idx)
		if _, seekErr := f.Seek(-backtrack, io.SeekCurrent); seekErr != nil {
			return false, seekErr
		}
		return true, nil
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

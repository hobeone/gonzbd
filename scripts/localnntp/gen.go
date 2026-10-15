package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hobeone/gonzbd/test/mocknntp"
)

var (
	reRARPart = regexp.MustCompile(`(?i)\.part(\d+)\.rar$`)
	rePar2Vol = regexp.MustCompile(`(?i)\.vol\d+\+(\d+)\.par2$`)
)

// ParseSize parses human-readable byte sizes (e.g., "64K", "128MB", "2G")
// and returns the size in bytes. Values <= 0 or invalid strings return an error.
func ParseSize(s string) (int64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, errors.New("size string is empty")
	}

	upper := strings.ToUpper(trimmed)
	var mult int64 = 1
	numStr := upper

	switch {
	case strings.HasSuffix(upper, "KB"):
		mult = 1 << 10
		numStr = strings.TrimSuffix(upper, "KB")
	case strings.HasSuffix(upper, "K"):
		mult = 1 << 10
		numStr = strings.TrimSuffix(upper, "K")
	case strings.HasSuffix(upper, "MB"):
		mult = 1 << 20
		numStr = strings.TrimSuffix(upper, "MB")
	case strings.HasSuffix(upper, "M"):
		mult = 1 << 20
		numStr = strings.TrimSuffix(upper, "M")
	case strings.HasSuffix(upper, "GB"):
		mult = 1 << 30
		numStr = strings.TrimSuffix(upper, "GB")
	case strings.HasSuffix(upper, "G"):
		mult = 1 << 30
		numStr = strings.TrimSuffix(upper, "G")
	case strings.HasSuffix(upper, "B"):
		mult = 1
		numStr = strings.TrimSuffix(upper, "B")
	}

	n, err := strconv.ParseInt(strings.TrimSpace(numStr), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("size must be positive: %q", s)
	}
	if mult > 1 && n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size overflows int64: %q", s)
	}

	return n * mult, nil
}

// WriteDeterministicPayload streams size deterministic pseudo-random bytes generated
// from seed to path, computing its SHA-256 along the way. Returns the lowercase hex hash.
func WriteDeterministicPayload(path string, size int64, seed uint64) (string, error) {
	if size < 0 {
		return "", errors.New("payload size cannot be negative")
	}

	var seedBytes [32]byte
	binary.LittleEndian.PutUint64(seedBytes[:8], seed)
	chacha := rand.NewChaCha8(seedBytes)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // path is operator-specified
	if err != nil {
		return "", fmt.Errorf("create payload file: %w", err)
	}
	defer func() {
		_ = f.Close() //nolint:errcheck // best-effort close on exit
	}()

	hasher := sha256.New()
	chunkSize := 1 << 20 // 1 MiB
	buf := make([]byte, chunkSize)
	remaining := size

	for remaining > 0 {
		toRead := min(remaining, int64(len(buf)))
		b := buf[:toRead]
		if _, err := io.ReadFull(chacha, b); err != nil {
			return "", fmt.Errorf("generate random bytes: %w", err)
		}
		if _, err := f.Write(b); err != nil {
			return "", fmt.Errorf("write payload: %w", err)
		}
		hasher.Write(b)
		remaining -= toRead
	}

	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync payload: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close payload: %w", err)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// FormatDotStuffedWire takes a yEnc-encoded body and formats it with NNTP dot-stuffing
// and a trailing ".\r\n" terminator, ready to be transmitted directly over the wire.
func FormatDotStuffedWire(yencBody []byte) []byte {
	var b bytes.Buffer
	b.Grow(len(yencBody) + len(yencBody)/50 + 16)
	remaining := yencBody
	for len(remaining) > 0 {
		idx := bytes.IndexByte(remaining, '\n')
		var lineBytes []byte
		if idx < 0 {
			lineBytes = remaining
			remaining = nil
		} else {
			lineBytes = remaining[:idx]
			remaining = remaining[idx+1:]
			if len(lineBytes) > 0 && lineBytes[len(lineBytes)-1] == '\r' {
				lineBytes = lineBytes[:len(lineBytes)-1]
			}
		}

		if len(lineBytes) > 0 && lineBytes[0] == '.' {
			b.WriteByte('.')
		}
		b.Write(lineBytes)
		b.WriteString("\r\n")
	}
	b.WriteString(".\r\n")
	return b.Bytes()
}

// WriteArticlesPack encodes each segment of the given release files into dot-stuffed wire format
// and writes them sequentially into packPath, populating WireOffset and WireLength on each segment in-place.
func WriteArticlesPack(packPath, filesDir string, files []ReleaseFile) error {
	packFile, err := os.OpenFile(packPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // packPath is within OutDir
	if err != nil {
		return fmt.Errorf("create articles packfile: %w", err)
	}
	defer func() {
		_ = packFile.Close() //nolint:errcheck // best-effort packfile close
	}()

	var currentOffset int64
	for fIdx := range files {
		rf := &files[fIdx]
		filePath := filepath.Join(filesDir, rf.DiskName)
		f, err := os.Open(filePath) //nolint:gosec // filePath is within filesDir
		if err != nil {
			return fmt.Errorf("open file %s for pack: %w", rf.DiskName, err)
		}

		totalParts := len(rf.Segments)
		for sIdx := range rf.Segments {
			seg := &rf.Segments[sIdx]
			raw := make([]byte, seg.Length)
			if _, err := io.ReadFull(io.NewSectionReader(f, seg.Offset, int64(seg.Length)), raw); err != nil {
				_ = f.Close() //nolint:errcheck // best-effort file close
				return fmt.Errorf("read segment %s: %w", seg.MessageID, err)
			}

			var yencBody []byte
			if totalParts == 1 {
				yencBody = mocknntp.EncodeYEnc(rf.NZBFilename, raw)
			} else {
				yencBody = mocknntp.EncodeYEncPart(rf.NZBFilename, seg.PartNum, totalParts, rf.TotalSize, seg.Offset, raw)
			}

			wireBytes := FormatDotStuffedWire(yencBody)
			n, err := packFile.Write(wireBytes)
			if err != nil {
				_ = f.Close() //nolint:errcheck // best-effort file close
				return fmt.Errorf("write wire bytes for %s: %w", seg.MessageID, err)
			}

			seg.WireOffset = currentOffset
			seg.WireLength = n
			currentOffset += int64(n)
		}
		_ = f.Close() //nolint:errcheck // best-effort file close
	}

	return nil
}

// SliceFileSegments slices a file on disk into segments of at most partSize bytes,
// calculating SHA-256 for the file and CRC32 for each segment.
func SliceFileSegments(path string, fileIdx int, diskName, nzbFilename string, role FileRole, volNum, partSize int) (ReleaseFile, error) {
	if partSize <= 0 {
		return ReleaseFile{}, errors.New("part size must be positive")
	}

	f, err := os.Open(path) //nolint:gosec // path is internal release file path
	if err != nil {
		return ReleaseFile{}, fmt.Errorf("open file to slice: %w", err)
	}
	defer func() {
		_ = f.Close() //nolint:errcheck // best-effort close
	}()

	stat, err := f.Stat()
	if err != nil {
		return ReleaseFile{}, fmt.Errorf("stat file: %w", err)
	}
	totalSize := stat.Size()

	fileHasher := sha256.New()
	buf := make([]byte, partSize)
	var segments []SegmentMeta
	var offset int64
	partNum := 1

	for {
		n, readErr := io.ReadFull(f, buf)
		if n > 0 {
			segmentBytes := buf[:n]
			fileHasher.Write(segmentBytes)
			crc := crc32.ChecksumIEEE(segmentBytes)

			idKey := fmt.Sprintf("%s:%d", diskName, partNum)
			idSum := sha256.Sum256([]byte(idKey))
			msgID := hex.EncodeToString(idSum[:])[:32] + "@localnntp.test"

			segments = append(segments, SegmentMeta{
				MessageID: msgID,
				PartNum:   partNum,
				Offset:    offset,
				Length:    n,
				CRC32:     crc,
			})

			offset += int64(n)
			partNum++
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				break
			}
			return ReleaseFile{}, fmt.Errorf("read file segment: %w", readErr)
		}
	}

	return ReleaseFile{
		FileIndex:   fileIdx,
		DiskName:    diskName,
		NZBFilename: nzbFilename,
		Role:        role,
		VolumeNum:   volNum,
		TotalSize:   totalSize,
		SHA256:      hex.EncodeToString(fileHasher.Sum(nil)),
		Segments:    segments,
	}, nil
}

func classifyReleaseFile(name string) (role FileRole, vol int) {
	if m := reRARPart.FindStringSubmatch(name); len(m) > 1 {
		v, err := strconv.Atoi(m[1])
		if err != nil {
			// Digit submatch overflowed int; still classify as RAR volume.
			return RoleRARVolume, 0
		}
		return RoleRARVolume, v
	}
	if m := rePar2Vol.FindStringSubmatch(name); len(m) > 1 {
		v, err := strconv.Atoi(m[1])
		if err != nil {
			// Digit submatch overflowed int; still classify as PAR2 volume.
			return RolePar2Volume, 0
		}
		return RolePar2Volume, v
	}
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".rar") {
		return RoleRARVolume, 1
	}
	if strings.HasSuffix(lower, ".par2") {
		return RolePar2Index, 0
	}
	return RoleSidecar, 0
}

// GenConfig specifies options for generating a test release.
type GenConfig struct {
	OutDir         string
	Name           string
	PayloadSize    int64
	Volumes        int
	PartSize       int
	Par2Redundancy int
	Par2Volumes    int
	Seed           uint64
	Compress       bool
	Obfuscate      bool
	IncludeSidecar bool
}

// GenerateRelease creates synthetic release files, packages them with rar and par2,
// generates metadata, and writes the manifest and NZB.
func GenerateRelease(cfg GenConfig) (*ReleaseManifest, error) {
	if cfg.PayloadSize <= 0 {
		return nil, errors.New("payload size must be positive")
	}
	if cfg.Volumes <= 0 {
		return nil, errors.New("volumes must be positive")
	}
	if cfg.PartSize <= 0 {
		return nil, errors.New("part size must be positive")
	}
	if cfg.Par2Redundancy < 0 {
		return nil, errors.New("par2 redundancy cannot be negative")
	}
	if cfg.Name == "" {
		cfg.Name = "release"
	}
	if cfg.Par2Volumes <= 0 {
		cfg.Par2Volumes = 4
	}

	filesDir := filepath.Join(cfg.OutDir, "files")
	if entries, err := os.ReadDir(filesDir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("files dir %s is not empty; use a fresh --dir", filesDir)
	}
	if err := os.MkdirAll(filesDir, 0o750); err != nil {
		return nil, fmt.Errorf("create files dir: %w", err)
	}

	stagingDir, err := os.MkdirTemp("", "localnntp-staging-*")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(stagingDir) //nolint:errcheck // best-effort cleanup of temp dir
	}()

	payloadPath := filepath.Join(stagingDir, "payload.bin")
	sum, err := WriteDeterministicPayload(payloadPath, cfg.PayloadSize, cfg.Seed)
	if err != nil {
		return nil, fmt.Errorf("write payload: %w", err)
	}
	expectedOutputs := []ExpectedOutput{
		{
			Name:   "payload.bin",
			Size:   cfg.PayloadSize,
			SHA256: sum,
		},
	}

	volSize := max((cfg.PayloadSize+int64(cfg.Volumes)-1)/int64(cfg.Volumes), 16384)

	compressOpt := "-m0"
	if cfg.Compress {
		compressOpt = "-m3"
	}

	archiveDest := filepath.Join(filesDir, cfg.Name+".rar")
	rarArgs := []string{
		"a",
		"-ma5",
		compressOpt,
		"-ep",
		fmt.Sprintf("-v%db", volSize),
		archiveDest,
		payloadPath,
	}
	rarCmd := exec.Command("rar", rarArgs...) //nolint:gosec // CLI tool invoking rar with controlled args
	if out, err := rarCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("rar command failed (%w): %s", err, string(out))
	}

	entries, err := os.ReadDir(filesDir)
	if err != nil {
		return nil, fmt.Errorf("read files dir: %w", err)
	}

	var rarFiles []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		role, _ := classifyReleaseFile(e.Name())
		if role == RoleRARVolume {
			rarFiles = append(rarFiles, e.Name())
		}
	}
	if len(rarFiles) == 0 {
		return nil, errors.New("rar produced no volume files")
	}
	slices.Sort(rarFiles)

	if cfg.Par2Redundancy > 0 {
		par2Path := cfg.Name + ".par2"
		par2Args := make([]string, 0, 5+len(rarFiles))
		par2Args = append(par2Args,
			"create",
			"-q",
			fmt.Sprintf("-r%d", cfg.Par2Redundancy),
			fmt.Sprintf("-n%d", cfg.Par2Volumes),
			par2Path,
		)
		par2Args = append(par2Args, rarFiles...)
		par2Cmd := exec.Command("par2", par2Args...) //nolint:gosec // CLI tool invoking par2 with controlled args
		par2Cmd.Dir = filesDir
		if out, err := par2Cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("par2 command failed (%w): %s", err, string(out))
		}
	}

	if cfg.IncludeSidecar {
		nfoPath := filepath.Join(filesDir, cfg.Name+".nfo")
		nfoContent := fmt.Sprintf("Release: %s\nGenerated by localnntp.\nPayload Size: %d\n", cfg.Name, cfg.PayloadSize)
		if err := os.WriteFile(nfoPath, []byte(nfoContent), 0o600); err != nil {
			return nil, fmt.Errorf("write nfo sidecar: %w", err)
		}

		samplePath := filepath.Join(filesDir, "sample.mkv")
		if _, err := WriteDeterministicPayload(samplePath, 16384, cfg.Seed^0x5afe); err != nil {
			return nil, fmt.Errorf("write sample sidecar: %w", err)
		}
	}

	finalEntries, err := os.ReadDir(filesDir)
	if err != nil {
		return nil, fmt.Errorf("read final files dir: %w", err)
	}

	var filenames []string
	for _, e := range finalEntries {
		if !e.IsDir() {
			filenames = append(filenames, e.Name())
		}
	}
	slices.Sort(filenames)

	var releaseFiles []ReleaseFile
	for idx, fname := range filenames {
		filePath := filepath.Join(filesDir, fname)
		role, volNum := classifyReleaseFile(fname)
		nzbFilename := fname
		if role == RoleRARVolume && cfg.Obfuscate {
			h := sha256.Sum256([]byte(fname))
			nzbFilename = hex.EncodeToString(h[:])[:20] + ".bin"
		}
		rf, err := SliceFileSegments(filePath, idx, fname, nzbFilename, role, volNum, cfg.PartSize)
		if err != nil {
			return nil, fmt.Errorf("slice file %s: %w", fname, err)
		}
		releaseFiles = append(releaseFiles, rf)
	}

	packPath := filepath.Join(cfg.OutDir, "articles.pack")
	if err := WriteArticlesPack(packPath, filesDir, releaseFiles); err != nil {
		return nil, fmt.Errorf("write articles pack: %w", err)
	}

	manifest := &ReleaseManifest{
		Name:            cfg.Name,
		PartSize:        cfg.PartSize,
		Volumes:         len(rarFiles),
		ExpectedOutputs: expectedOutputs,
		Files:           releaseFiles,
	}

	manifestPath := filepath.Join(cfg.OutDir, "manifest.json")
	if err := SaveManifest(manifestPath, manifest); err != nil {
		return nil, fmt.Errorf("save manifest: %w", err)
	}

	nzbBytes := BuildNZB(manifest)
	nzbPath := filepath.Join(cfg.OutDir, "release.nzb")
	if err := os.WriteFile(nzbPath, nzbBytes, 0o600); err != nil {
		return nil, fmt.Errorf("save nzb: %w", err)
	}

	return manifest, nil
}

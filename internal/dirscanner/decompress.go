package dirscanner

import (
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	// MaxDecompressSize limits decompressed NZB size per file to guard against zip bombs.
	MaxDecompressSize = 100 * 1024 * 1024 // 100 MiB

	maxNZBsPerZip            = 1000              // sane upper bound for NZBs in one archive
	maxCumulativeBytes int64 = 500 * 1024 * 1024 // 500 MiB total across all NZBs
)

// ArchiveType identifies the format of a file.
type ArchiveType int

const (
	// TypeNZB is a plain NZB file.
	TypeNZB ArchiveType = iota
	// TypeGZ is a gzip-compressed file.
	TypeGZ
	// TypeBZ2 is a bzip2-compressed file.
	TypeBZ2
	// TypeZip is a ZIP archive.
	TypeZip
)

// DetectType returns the archive type based on file extension.
func DetectType(path string) (ArchiveType, error) {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".nzb.gz"):
		return TypeGZ, nil
	case strings.HasSuffix(lower, ".nzb.bz2"):
		return TypeBZ2, nil
	case strings.HasSuffix(lower, ".zip"):
		return TypeZip, nil
	case strings.HasSuffix(lower, ".nzb"):
		return TypeNZB, nil
	default:
		return TypeNZB, fmt.Errorf("unrecognized file type: %s", filepath.Ext(path))
	}
}

// Payload is one NZB extracted from a file.
type Payload struct {
	// Name is what the caller hands to the handler as the NZB's filename.
	// It is empty for a plain, gzip or bzip2 NZB and for the only NZB in a
	// zip whose member name memberBaseName rejected; the caller then uses
	// the name of the file it opened. Within one archive every non-empty
	// Name is distinct (uniqueMemberNames).
	Name string
	Data []byte
}

// ExtractNZBs opens a file and returns one or more NZB payloads.
// For plain NZB, GZ, and BZ2 it returns a single payload.
// For ZIP it may return multiple payloads if multiple .nzb files are found.
// Each decompressed NZB is capped at MaxDecompressSize.
func ExtractNZBs(path string) ([]Payload, error) {
	archType, err := DetectType(path)
	if err != nil {
		return nil, err
	}

	switch archType {
	case TypeNZB:
		return extractPlainNZB(path)
	case TypeGZ:
		return extractGZ(path)
	case TypeBZ2:
		return extractBZ2(path)
	case TypeZip:
		return extractZip(path)
	default:
		return nil, fmt.Errorf("unsupported archive type: %d", archType)
	}
}

func extractPlainNZB(path string) ([]Payload, error) {
	//nolint:gosec // G304: opening path provided by caller/config
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read NZB file: %w", err)
	}
	if len(data) > MaxDecompressSize {
		return nil, fmt.Errorf("NZB file exceeds maximum size (%d > %d)", len(data), MaxDecompressSize)
	}
	return []Payload{{Data: data}}, nil
}

func extractGZ(path string) ([]Payload, error) {
	//nolint:gosec // G304: opening path provided by caller/config
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open gz file: %w", err)
	}
	defer file.Close() //nolint:errcheck // cleanup of opened file

	reader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer reader.Close() //nolint:errcheck // cleanup of gzip reader

	data, err := io.ReadAll(io.LimitReader(reader, int64(MaxDecompressSize)+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read decompressed data: %w", err)
	}

	if len(data) > MaxDecompressSize {
		return nil, fmt.Errorf("decompressed size exceeds maximum (%d > %d)", len(data), MaxDecompressSize)
	}

	return []Payload{{Data: data}}, nil
}

func extractBZ2(path string) ([]Payload, error) {
	//nolint:gosec // G304: opening path provided by caller/config
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open bz2 file: %w", err)
	}
	defer file.Close() //nolint:errcheck // cleanup of opened file

	reader := bzip2.NewReader(file)

	data, err := io.ReadAll(io.LimitReader(reader, int64(MaxDecompressSize)+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read decompressed data: %w", err)
	}

	if len(data) > MaxDecompressSize {
		return nil, fmt.Errorf("decompressed size exceeds maximum (%d > %d)", len(data), MaxDecompressSize)
	}

	return []Payload{{Data: data}}, nil
}

func extractZip(path string) ([]Payload, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open zip file: %w", err)
	}
	defer reader.Close() //nolint:errcheck // cleanup of zip reader

	var result []Payload
	var cumulative int64
	for _, file := range reader.File {
		if !strings.HasSuffix(strings.ToLower(file.Name), ".nzb") {
			continue
		}
		if len(result) >= maxNZBsPerZip {
			return nil, fmt.Errorf("zip contains more than %d NZB files", maxNZBsPerZip)
		}

		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open zip member %s: %w", file.Name, err)
		}

		data, err := io.ReadAll(io.LimitReader(rc, int64(MaxDecompressSize)+1))
		rc.Close() //nolint:errcheck // cleanup of opened zip member

		if err != nil {
			return nil, fmt.Errorf("failed to read zip member %s: %w", file.Name, err)
		}

		if len(data) > MaxDecompressSize {
			return nil, fmt.Errorf("zip member %s exceeds maximum size (%d > %d)", file.Name, len(data), MaxDecompressSize)
		}

		cumulative += int64(len(data))
		if cumulative > maxCumulativeBytes {
			return nil, fmt.Errorf("cumulative decompressed size exceeds %d bytes", maxCumulativeBytes)
		}

		result = append(result, Payload{Name: memberBaseName(file.Name), Data: data})
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no .nzb files found in zip")
	}

	uniqueMemberNames(result, filepath.Base(path))
	return result, nil
}

// uniqueMemberNames is the one place that guarantees no two NZBs of one
// archive reach the handler under the same filename: the app files an NZB
// under its filename and adds a second one with the same name paused as a
// duplicate. A member whose name was rejected, or repeats an earlier
// member's, is named "<zip>[n]" after its 1-based position. A lone NZB keeps
// whatever memberBaseName returned, so the caller falls back to the zip's
// own name.
func uniqueMemberNames(payloads []Payload, zipName string) {
	if len(payloads) < 2 {
		return
	}
	seen := make(map[string]bool, len(payloads))
	for i := range payloads {
		name := payloads[i].Name
		if name == "" || seen[name] {
			// Unique without a check: a member name always ends in
			// ".nzb" and this one ends in "]", and positions differ.
			name = fmt.Sprintf("%s[%d]", zipName, i+1)
		}
		seen[name] = true
		payloads[i].Name = name
	}
}

// maxMemberNameLen keeps the name usable as the admin/nzb/<name>.gz backup
// filename: well under the 255-byte limit once ".gz" is appended.
const maxMemberNameLen = 200

// memberBaseName reduces an untrusted zip member name to its final path
// element, so a name such as "../../x.nzb" or "a\\b\\x.nzb" carries no
// directory component to whoever uses it. "" means no usable name: empty,
// a bare ".nzb", not valid UTF-8 (a legacy-codepage header), containing a
// control character (NUL included), or longer than maxMemberNameLen bytes.
func memberBaseName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	base := name[strings.LastIndex(name, "/")+1:]
	if base == "" || base == "." || base == ".." || strings.EqualFold(base, ".nzb") || len(base) > maxMemberNameLen || !utf8.ValidString(base) {
		return ""
	}
	for _, r := range base {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return base
}

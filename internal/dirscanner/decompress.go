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
)

const (
	// MaxDecompressSize limits decompressed NZB size per file to guard against zip bombs.
	MaxDecompressSize = 100 * 1024 * 1024 // 100 MiB
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

// ExtractNZBs opens a file and returns one or more NZB payloads as byte slices.
// For plain NZB, GZ, and BZ2 it returns a single payload.
// For ZIP it may return multiple payloads if multiple .nzb files are found.
// Each decompressed NZB is capped at MaxDecompressSize.
func ExtractNZBs(path string) ([][]byte, error) {
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

func extractPlainNZB(path string) ([][]byte, error) {
	//nolint:gosec // G304: opening path provided by caller/config
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read NZB file: %w", err)
	}
	if len(data) > MaxDecompressSize {
		return nil, fmt.Errorf("NZB file exceeds maximum size (%d > %d)", len(data), MaxDecompressSize)
	}
	return [][]byte{data}, nil
}

func extractGZ(path string) ([][]byte, error) {
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

	data, err := io.ReadAll(io.LimitReader(reader, MaxDecompressSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read decompressed data: %w", err)
	}

	if len(data) > MaxDecompressSize {
		return nil, fmt.Errorf("decompressed size exceeds maximum (%d > %d)", len(data), MaxDecompressSize)
	}

	return [][]byte{data}, nil
}

func extractBZ2(path string) ([][]byte, error) {
	//nolint:gosec // G304: opening path provided by caller/config
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open bz2 file: %w", err)
	}
	defer file.Close() //nolint:errcheck // cleanup of opened file

	reader := bzip2.NewReader(file)

	data, err := io.ReadAll(io.LimitReader(reader, MaxDecompressSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read decompressed data: %w", err)
	}

	if len(data) > MaxDecompressSize {
		return nil, fmt.Errorf("decompressed size exceeds maximum (%d > %d)", len(data), MaxDecompressSize)
	}

	return [][]byte{data}, nil
}

func extractZip(path string) ([][]byte, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open zip file: %w", err)
	}
	defer reader.Close() //nolint:errcheck // cleanup of zip reader

	var result [][]byte
	for _, file := range reader.File {
		if !strings.HasSuffix(strings.ToLower(file.Name), ".nzb") {
			continue
		}

		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open zip member %s: %w", file.Name, err)
		}

		data, err := io.ReadAll(io.LimitReader(rc, MaxDecompressSize+1))
		rc.Close() //nolint:errcheck // cleanup of opened zip member

		if err != nil {
			return nil, fmt.Errorf("failed to read zip member %s: %w", file.Name, err)
		}

		if len(data) > MaxDecompressSize {
			return nil, fmt.Errorf("zip member %s exceeds maximum size (%d > %d)", file.Name, len(data), MaxDecompressSize)
		}

		result = append(result, data)
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no .nzb files found in zip")
	}

	return result, nil
}

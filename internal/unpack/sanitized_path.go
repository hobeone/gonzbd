package unpack

import (
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"strings"
)

// SanitizedPath represents an archive entry path that has been verified against
// directory traversal, null-byte injection, and empty-path edge cases.
// Constructing it via NewSanitizedPath enforces validation at the producer
// boundary before yielding file headers to consumers.
type SanitizedPath struct {
	rel string
}

// NewSanitizedPath sanitizes a raw archive entry name and returns a verified
// SanitizedPath. Returns an error if the path contains null bytes, directory
// traversal escapes (".."), or becomes empty after sanitization.
func NewSanitizedPath(rawName string, oneFolder bool) (SanitizedPath, error) {
	rel, err := SanitizeArchivePath(rawName, oneFolder)
	if err != nil {
		return SanitizedPath{}, err
	}
	return SanitizedPath{rel: rel}, nil
}

// Rel returns the sanitized path relative to the archive extraction root.
func (p SanitizedPath) Rel() string {
	return p.rel
}

// Abs returns the absolute path joined with outDir (filepath.Join(outDir, p.rel)),
// used for skip-check logging and external reference.
func (p SanitizedPath) Abs(outDir string) string {
	return filepath.Join(outDir, p.rel)
}

// SanitizeArchivePath cleans an archive entry path for safe extraction.
// It prevents directory traversal attacks, rejects null bytes, and optionally
// flattens paths (OneFolder mode).
func SanitizeArchivePath(name string, oneFolder bool) (string, error) {
	// Normalize separators.
	name = strings.ReplaceAll(name, "\\", "/")

	// Reject null bytes.
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("archive path contains null byte: %q", name)
	}

	if oneFolder {
		name = path.Base(name)
	} else {
		// Clean and strip leading traversal components.
		name = path.Clean(name)
		name = strings.TrimPrefix(name, "/")
		// Reject any remaining ".." path component after cleaning.
		// path.Clean resolves internal ".." (a/../b → b), so ".." can
		// only survive at the start after cleaning. This broader check
		// is defense-in-depth against exotic path encodings.
		for component := range strings.SplitSeq(name, "/") {
			if component == ".." {
				return "", fmt.Errorf("archive path escapes output directory: %q", name)
			}
		}
	}

	if name == "" || name == "." {
		return "", fmt.Errorf("archive path is empty after sanitization")
	}
	return name, nil
}

// MemberPath is the per-member name check of the RAR extractors
// (ExtractMember in go_unrar.go, which directunpack also calls). It
// sanitizes name with NewSanitizedPath; a name that fails (a ".." escape, a NUL byte, nothing
// left) is logged and reported on opts.OnLine, and ok is false so the caller
// skips the member. rarengine already strips a leading "../" from RAR member
// names ("../e.txt" arrives as "e.txt"), so this is the backstop for a name
// it lets through.
func MemberPath(name string, opts Options, log *slog.Logger) (SanitizedPath, bool) {
	sp, err := NewSanitizedPath(name, opts.OneFolder)
	if err != nil {
		log.Warn("skipping entry with bad path", "raw_name", name, "err", err)
		if opts.OnLine != nil {
			opts.OnLine("Skipping bad path: " + name)
		}
		return SanitizedPath{}, false
	}
	return sp, true
}

package deobfuscate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/h2non/filetype"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// popularExts is a set of file extensions (with leading dot, lowercase) that
// are considered "known" and not candidates for extension correction.
// Matches Python's POPULAR_EXT list from sabnzbd/utils/file_extension.py.
var popularExts = map[string]bool{
	".3g2": true, ".3gp": true, ".7z": true, ".aac": true, ".avi": true,
	".bmp": true, ".bz2": true, ".cab": true, ".css": true, ".csv": true,
	".dat": true, ".db": true, ".deb": true, ".dll": true, ".dmg": true,
	".doc": true, ".docx": true, ".epub": true, ".exe": true, ".flac": true,
	".flv": true,
	".gif": true, ".gz": true, ".h264": true, ".htm": true, ".html": true,
	".ico": true, ".iso": true, ".jar": true, ".jpg": true, ".jpeg": true,
	".js": true, ".json": true, ".log": true, ".m4a": true, ".m4v": true,
	".mid": true, ".midi": true, ".mkv": true, ".mov": true, ".mp3": true,
	".mp4": true, ".mpa": true, ".mpeg": true, ".mpg": true, ".msi": true,
	".nfo": true, ".ogg": true, ".ogv": true, ".opus": true, ".otf": true,
	".par2": true, ".pdf": true, ".php": true, ".pkg": true, ".png": true,
	".ppt": true, ".pptx": true, ".psd": true, ".py": true, ".rar": true,
	".rpm": true, ".rss": true, ".rtf": true, ".sav": true, ".sql": true,
	".srt": true, ".sub": true, ".svg": true, ".swf": true, ".tar": true,
	".tif": true, ".tiff": true, ".ts": true, ".ttf": true, ".txt": true,
	".vob": true, ".wav": true, ".weba": true, ".webm": true, ".webp": true,
	".wmv": true, ".woff": true, ".woff2": true, ".xls": true, ".xlsx": true,
	".xml": true, ".xz": true, ".zip": true,
}

// collisionSuffixRe matches a popular extension followed by a short numeric
// suffix: ".rar.1", ".mkv.2", ".7z.01", etc. These are created by the OS or
// assembler to avoid filename collisions (e.g. duplicate NZB segments) and
// should NOT be treated as unknown extensions.
var collisionSuffixRe = regexp.MustCompile(`(?i)(\.[a-z0-9]{2,5})\.(\d{1,3})$`)

// rarVolumeRe matches multi-volume RAR and 7z naming conventions that must
// be treated as "known" extensions even though they aren't in popularExts.
// Mirrors SABnzbd's RAR_RE in sabnzbd/filesystem.py.
//
// Cases handled:
//   - .rar, .part01.rar, .part123.rar  (modern multi-volume naming)
//   - .r00..r99                        (legacy first volume continuation)
//   - .s00..v99                        (legacy continuations after r-series)
//   - .001..9999                       (split files / 7z parts)
//
// Without this, FixExtension would magic-sniff a .r00 file, see RAR header
// bytes, and append ".rar" — turning "foo.r00" into "foo.r00.rar" and
// breaking unpacking because the volume name in the rarset metadata is
// "foo.r00" not "foo.r00.rar".
var rarVolumeRe = regexp.MustCompile(`(?i)\.(part\d+\.rar|rar|r\d{2}|s\d{2}|t\d{2}|u\d{2}|v\d{2}|\d{3,4})$`)

// HasPopularExtension returns true if the file's extension is in the popular
// set, has a collision suffix (".rar.1"), or matches a known multi-volume
// archive naming convention (".r00", ".part01.rar", ".001").
func HasPopularExtension(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	if popularExts[ext] {
		return true
	}
	if hasCollisionSuffix(filename) {
		return true
	}
	return rarVolumeRe.MatchString(filename)
}

// hasCollisionSuffix returns true if the filename ends with a popular
// extension followed by a short numeric suffix (e.g. ".rar.1", ".mkv.02").
// This pattern occurs when the filesystem or assembler appends a number to
// avoid overwriting an existing file with the same name.
func hasCollisionSuffix(filename string) bool {
	m := collisionSuffixRe.FindStringSubmatch(filename)
	if m == nil {
		return false
	}
	realExt := strings.ToLower(m[1])
	return popularExts[realExt]
}

// FixExtension inspects the file at path. If its current extension is not
// popular (and not a collision-suffixed popular extension), it reads magic
// bytes to detect the file type. If a better extension can be determined,
// the file is renamed by appending the correct extension.
//
// The log parameter is used for diagnostic output explaining why each file
// is or isn't being renamed. Pass nil for silent operation.
//
// Returns zero-value Rename and nil error if no fix is needed.
func FixExtension(ctx context.Context, log *slog.Logger, root *os.Root, rel, path string) (Rename, error) {
	if log == nil {
		log = slog.Default().With("component", "deobfuscate")
	}
	base := filepath.Base(rel)
	ext := strings.ToLower(filepath.Ext(rel))

	if popularExts[ext] {
		log.Debug("deobfuscate: extension already popular, skipping",
			"file", base, "ext", ext)
		return Rename{}, nil
	}

	if hasCollisionSuffix(rel) {
		m := collisionSuffixRe.FindStringSubmatch(rel)
		log.Info("deobfuscate: file has collision suffix, skipping extension fix",
			"file", base, "real_ext", m[1], "suffix", m[2])
		return Rename{}, nil
	}

	if rarVolumeRe.MatchString(base) {
		log.Debug("deobfuscate: multi-volume archive name, skipping extension fix",
			"file", base, "ext", ext)
		return Rename{}, nil
	}

	log.Debug("deobfuscate: extension not popular, sniffing magic bytes",
		"file", base, "ext", ext)

	f, err := root.Open(rel)
	if err != nil {
		// File too small or unreadable — not an error worth propagating.
		if os.IsNotExist(err) {
			return Rename{}, nil
		}
		return Rename{}, err
	}
	defer f.Close() //nolint:errcheck // read-only close

	kind, err := filetype.MatchReader(f)
	if err != nil {
		return Rename{}, err
	}

	if kind == filetype.Unknown {
		log.Debug("deobfuscate: unknown content type, no extension fix",
			"file", base)
		return Rename{}, nil
	}

	detectedExt := "." + kind.Extension

	// Don't rename if the current extension already matches the detected type.
	if ext == detectedExt {
		log.Debug("deobfuscate: extension matches detected type, no fix needed",
			"file", base, "ext", ext)
		return Rename{}, nil
	}

	newRel := fsutil.GetUniqueRelPath(root, rel+detectedExt)
	newPath := filepath.Join(filepath.Dir(path), filepath.Base(newRel))
	log.Info("deobfuscate: content type detected, appending correct extension",
		"file", base, "current_ext", ext, "detected_ext", detectedExt)
	if err := root.Rename(rel, newRel); err != nil {
		return Rename{}, fmt.Errorf("rename %s → %s: %w", rel, newRel, err)
	}
	return Rename{From: path, To: newPath}, nil
}

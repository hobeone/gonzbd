package deobfuscate

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/h2non/filetype"

	"github.com/hobeone/gonzbd/internal/rarheader"
)

// popularExts is a set of file extensions (with leading dot, lowercase) that
// are considered "known" and not candidates for extension correction.
// Matches Python's POPULAR_EXT list from sabnzbd/utils/file_extension.py.
var popularExts = map[string]bool{
	".3g2": true, ".3gp": true, ".7z": true, ".aac": true, ".avi": true,
	".bmp": true, ".bz2": true, ".cab": true, ".css": true, ".csv": true,
	".dat": true, ".db": true, ".deb": true, ".dll": true, ".dmg": true,
	".doc": true, ".docx": true, ".epub": true, ".exe": true, ".flv": true,
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

// HasPopularExtension returns true if the file's extension is in the popular set.
func HasPopularExtension(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	return popularExts[ext]
}

// FixExtension inspects the file at path. If its current extension is not
// popular, it reads the first 262 bytes and uses github.com/h2non/filetype
// to detect the file type by magic bytes. This detects 100+ formats
// including MKV, AVI, RAR, 7z, FLAC, ISO, and other types that the stdlib
// net/http.DetectContentType misses.
//
// If a better extension can be determined, the file is renamed by appending
// the correct extension. Returns zero-value Rename and nil error if no fix
// is needed.
func FixExtension(path string) (Rename, error) {
	if HasPopularExtension(path) {
		return Rename{}, nil
	}

	kind, err := filetype.MatchFile(path)
	if err != nil {
		// File too small or unreadable — not an error worth propagating.
		if os.IsNotExist(err) {
			return Rename{}, nil
		}
		return Rename{}, err
	}

	if kind == filetype.Unknown {
		// Fallback: check if it's a RAR archive by magic signature.
		// filetype may not recognize all RAR variants.
		isRAR, _ := rarheader.IsRAR(path)
		if isRAR {
			currentExt := strings.ToLower(filepath.Ext(path))
			if currentExt == ".rar" {
				return Rename{}, nil
			}
			newPath := path + ".rar"
			if err := os.Rename(path, newPath); err != nil {
				return Rename{}, err
			}
			return Rename{From: path, To: newPath}, nil
		}
		return Rename{}, nil
	}

	detectedExt := "." + kind.Extension

	// Don't rename if the current extension already matches the detected type.
	currentExt := strings.ToLower(filepath.Ext(path))
	if currentExt == detectedExt {
		return Rename{}, nil
	}

	newPath := path + detectedExt
	if err := os.Rename(path, newPath); err != nil {
		return Rename{}, err
	}
	return Rename{From: path, To: newPath}, nil
}

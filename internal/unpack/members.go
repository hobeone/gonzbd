package unpack

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/bodgit/sevenzip"
	"github.com/hobeone/rarengine"
)

// MemberBaseNames reports which of want, compared by base name, are members of
// archive a. It stops as soon as every name in want has been seen.
//
// It reads headers, and bounds what else it reads:
//   - 7z: the header block sevenzip.OpenReader parses; no member is decoded.
//   - RAR: only a.MainFile, the volume named first. A member whose header
//     starts in a later volume is not seen. rarengine.ListVolume reads each
//     member's packed bytes to skip them and decodes nothing, solid or not, so
//     the cost is at most one volume.
//
// An archive of any other type is an error, as is a header that cannot be
// read — including an encrypted one, since no password is supplied.
func MemberBaseNames(a Archive, want map[string]bool) (map[string]bool, error) {
	switch a.Type {
	case RarArchive:
		return rarMemberBaseNames(a.MainFile, want)
	case SevenZipArchive:
		return sevenZipMemberBaseNames(a.MainFile, want)
	default:
		return nil, fmt.Errorf("member listing: unsupported archive type for %s", a.MainFile)
	}
}

func rarMemberBaseNames(first string, want map[string]bool) (map[string]bool, error) {
	found := make(map[string]bool)
	if len(want) == 0 {
		return found, nil
	}
	f, err := os.Open(first) //nolint:gosec // read-only open of a scanned archive volume
	if err != nil {
		return nil, fmt.Errorf("member listing: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	// A listing, not an extraction: a volume named first may still be a later
	// one, and what is asked is which members start in it. A member refused
	// by name comes with a non-nil header and its refusal, and is still one.
	for fh, err := range rarengine.ListVolume(f) {
		if fh == nil {
			return nil, fmt.Errorf("member listing %s: %w", first, err)
		}
		if base := MemberBaseName(fh.Name); want[base] {
			found[base] = true
			if len(found) == len(want) {
				break
			}
		}
	}
	return found, nil
}

func sevenZipMemberBaseNames(main string, want map[string]bool) (map[string]bool, error) {
	r, err := sevenzip.OpenReader(main)
	if err != nil {
		return nil, fmt.Errorf("member listing %s: %w", main, err)
	}
	defer r.Close() //nolint:errcheck // read-only

	found := make(map[string]bool)
	for _, f := range r.File {
		if base := MemberBaseName(f.Name); want[base] {
			found[base] = true
		}
	}
	return found, nil
}

// MemberBaseName is the base name MemberBaseNames compares, for a name that
// may use either path separator: archive and par2 names are both
// poster-written, and neither format guarantees '/'.
func MemberBaseName(name string) string {
	return path.Base(strings.ReplaceAll(name, `\`, "/"))
}

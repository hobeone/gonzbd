// Package buildinfo resolves what this binary was built from: version,
// commit, commit time, whether the tree was modified, and build time.
//
// Resolve is the one function that computes it; callers read the returned
// Info and never recompute a field. A field no source supplied is "" (false
// for Dirty); only the display methods spell that "unknown".
//
// Per field, a link-time value (the -ldflags -X main.* variables, passed in
// as LinkTime) wins, and the toolchain's embedded VCS settings are the
// fallback:
//
//   - Version and BuildDate have no VCS fallback: the toolchain records no
//     release tag and no build timestamp. An unset Version becomes
//     DevVersion.
//   - Commit falls back to vcs.revision, shortened to ShortSHALen.
//   - CommitTime falls back to vcs.time and Dirty to vcs.modified, but only
//     when vcs.revision describes the same commit as Commit. A link-time
//     commit that is not a prefix of vcs.revision means the settings belong
//     to some other tree.
//
// The toolchain stamps VCS settings only for a build inside a VCS checkout
// with -buildvcs enabled (the default).
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// DevVersion is the Version of a build with no link-time version.
const DevVersion = "dev"

// ShortSHALen is the length a full vcs.revision is shortened to.
const ShortSHALen = 7

// LinkTime holds the raw -ldflags values; "" means not set. Dirty is
// "true" or "false"; any other value counts as not set.
type LinkTime struct {
	Version, Commit, CommitTime, Dirty, BuildDate string
}

// Info is the resolved build metadata.
type Info struct {
	// Version is the link-time version, or DevVersion.
	Version string
	// Commit is a short git SHA, or "".
	Commit string
	// CommitTime is the commit's RFC 3339 timestamp, or "".
	CommitTime string
	// Dirty reports uncommitted changes in the tree the binary was built
	// from. False when that is unknown.
	Dirty bool
	// BuildDate is the RFC 3339 build timestamp, or "".
	BuildDate string
}

// Resolve combines the link-time values with the toolchain's embedded
// build info; bi may be nil.
func Resolve(lt LinkTime, bi *debug.BuildInfo) Info {
	info := Info{
		Version:    lt.Version,
		Commit:     lt.Commit,
		CommitTime: lt.CommitTime,
		BuildDate:  lt.BuildDate,
	}
	if info.Version == "" {
		info.Version = DevVersion
	}
	dirtySet := true
	switch lt.Dirty {
	case "true":
		info.Dirty = true
	case "false":
	default:
		dirtySet = false
	}

	var revision, vcsTime, modified string
	if bi != nil {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.time":
				vcsTime = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
	}
	if revision == "" {
		return info
	}

	if info.Commit == "" {
		info.Commit = shorten(revision)
	} else if !strings.HasPrefix(revision, info.Commit) {
		return info
	}
	if info.CommitTime == "" {
		info.CommitTime = vcsTime
	}
	if !dirtySet {
		info.Dirty = modified == "true"
	}
	return info
}

func shorten(sha string) string {
	if len(sha) > ShortSHALen {
		return sha[:ShortSHALen]
	}
	return sha
}

const unknown = "unknown"

// CommitLabel renders the commit for --version and logs: "abc1234",
// "abc1234 (modified)", or "unknown" when there is no commit.
func (i Info) CommitLabel() string {
	if i.Commit == "" {
		return unknown
	}
	if i.Dirty {
		return i.Commit + " (modified)"
	}
	return i.Commit
}

// BuildDateLabel renders the build time for --version, "unknown" when absent.
func (i Info) BuildDateLabel() string {
	if i.BuildDate == "" {
		return unknown
	}
	return i.BuildDate
}

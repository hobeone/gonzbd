// Package buildinfo resolves what this binary was built from: version,
// commit, commit time, whether the tree was modified, and build time.
//
// Resolve is the one function that computes it; callers read the returned
// Info and never recompute a field.
//
// Precedence, per field:
//
//   - Version and BuildDate come from link-time values only (the
//     -ldflags -X main.* variables). The Go toolchain records neither:
//     its Main.Version is a module version, not a release tag, and it
//     stamps no build timestamp at all.
//   - Commit prefers the link-time value. When that is unset, the
//     toolchain's embedded VCS setting vcs.revision is used, shortened to
//     ShortSHALen characters.
//   - CommitTime and Dirty (vcs.time, vcs.modified) come only from the
//     embedded VCS settings, and only when they describe the same commit
//     as Commit: a link-time commit that is not a prefix of vcs.revision
//     means the settings belong to some other tree.
//
// The toolchain stamps VCS settings only for a build inside a VCS checkout
// with -buildvcs enabled (the default). Without them every VCS-derived
// field keeps its zero value.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Unknown is the value of Commit and BuildDate when no source supplied
// one. Clients treat it as "absent".
const Unknown = "unknown"

// DevVersion is the Version of a build with no link-time version.
const DevVersion = "dev"

// ShortSHALen is the length a full vcs.revision is shortened to. It is
// git's floor for `git rev-parse --short`, which scripts/build.sh and
// scripts/docker-build use for the link-time commit; that command may emit
// more characters in a large repository, so both lengths occur in
// practice and both are unambiguous prefixes of the full revision.
const ShortSHALen = 7

// Info is the resolved build metadata.
type Info struct {
	// Version is the link-time version, or DevVersion.
	Version string
	// Commit is a short git SHA, or Unknown.
	Commit string
	// CommitTime is the commit's RFC 3339 timestamp, or "" when the
	// embedded VCS settings were absent or describe another commit.
	CommitTime string
	// Dirty reports uncommitted changes in the tree the binary was built
	// from. False when that is unknown.
	Dirty bool
	// BuildDate is the link-time RFC 3339 build timestamp, or Unknown.
	BuildDate string
}

// Resolve combines the link-time values with the toolchain's embedded
// build info. bi may be nil. Empty link-time strings count as unset, as do
// Unknown (for commit) and DevVersion (for version), which are the
// defaults of the link-time variables.
func Resolve(version, commit, date string, bi *debug.BuildInfo) Info {
	info := Info{Version: version, Commit: commit, BuildDate: date}
	if info.Version == "" {
		info.Version = DevVersion
	}
	if info.Commit == "" {
		info.Commit = Unknown
	}
	if info.BuildDate == "" {
		info.BuildDate = Unknown
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

	if info.Commit == Unknown {
		info.Commit = shorten(revision)
	} else if !strings.HasPrefix(revision, info.Commit) {
		return info
	}
	info.CommitTime = vcsTime
	info.Dirty = modified == "true"
	return info
}

func shorten(sha string) string {
	if len(sha) > ShortSHALen {
		return sha[:ShortSHALen]
	}
	return sha
}

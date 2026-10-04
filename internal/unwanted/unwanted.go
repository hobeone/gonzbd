// Package unwanted decides whether a filename carries an extension the user
// has asked not to receive, and names the per-job record of that decision.
//
// It is a defence against malware delivered as a download: a post that
// carries a .exe or .scr is refused, paused or cleaned, depending on the
// configured Action. Rules.Unwanted is the one function that decides whether
// a name is unwanted; the ingest check and the archive peek in internal/app
// and the post-unpack removal stage in internal/postproc all call it
// (Rules.Find, which the first two use, calls it), so they cannot disagree
// about a name.
//
// Filenames reach Unwanted from an NZB, which is untrusted. The extension is
// therefore read after normalisation that a crafted name cannot use to slip
// past the comparison (see extension): directory components are dropped,
// trailing dots, whitespace and control characters are trimmed, and the
// comparison is case-insensitive.
//
// What it cannot see is a name it is not given. A post whose subjects are
// obfuscated, or whose payload is inside an archive, carries no unwanted
// extension at ingest. While such a job downloads, the archive peek in
// internal/app is given the member names of each completed RAR volume and the
// names a par2 file declares, and blocks the job early; it is an accelerator.
// The post-unpack stage is the backstop for the files those produce on disk,
// and for the archive kinds the peek does not read.
package unwanted

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
)

// Mode selects how the configured extension list is read.
type Mode string

const (
	// ModeBlacklist treats the listed extensions as unwanted.
	ModeBlacklist Mode = "blacklist"
	// ModeWhitelist treats every extension not listed as unwanted.
	ModeWhitelist Mode = "whitelist"
)

// Valid reports whether m is one of the declared modes.
func (m Mode) Valid() bool {
	return m == ModeBlacklist || m == ModeWhitelist
}

// Action is what happens to a job whose NZB names an unwanted file.
type Action string

const (
	// ActionOff disables the check, both at ingest and after unpack.
	ActionOff Action = "off"
	// ActionPause pauses the job, at add or when a downloaded archive names
	// the file. Resuming it approves it.
	ActionPause Action = "pause"
	// ActionFail files the job in history as Failed: at add without
	// downloading it, or when a downloaded archive names the file.
	ActionFail Action = "fail"
)

// Valid reports whether a is one of the declared actions.
func (a Action) Valid() bool {
	return a == ActionOff || a == ActionPause || a == ActionFail
}

// State is a job's standing against the check. It is persisted with the job
// (dispatch_jobs.unwanted_ext) and with its history entry
// (history.unwanted_ext), as the integers below; the values match SABnzbd's
// nzo.unwanted_ext.
type State uint8

const (
	// StateNone means the ingest check found nothing, or did not run.
	StateNone State = iota
	// StateBlocked means the ingest check found an unwanted file and the
	// job was paused or failed for it.
	StateBlocked
	// StateApproved means the user accepted the job anyway, by resuming it
	// or by retrying it with allow_unwanted. An approved job is not checked
	// again at ingest and its files are not removed after unpack.
	StateApproved
)

// Valid reports whether s is one of the declared states.
func (s State) Valid() bool {
	return s <= StateApproved
}

// Rules is a validated extension list read in one mode, plus the action that
// applies when it matches. The zero value is ActionOff with an empty list.
type Rules struct {
	action   Action
	mode     Mode
	patterns []string
}

// NewRules validates action, mode and extensions and returns the rules they
// describe. Each extension is trimmed of surrounding whitespace and leading
// dots and lowercased; an entry left empty is dropped. An entry may be a
// path.Match pattern, matched against the whole extension ("r[0-9][0-9]"
// matches "r01" and not "r001").
func NewRules(action Action, mode Mode, extensions []string) (Rules, error) {
	var errs []error
	if !action.Valid() {
		errs = append(errs, fmt.Errorf("action %q is not one of %q, %q, %q", action, ActionOff, ActionPause, ActionFail))
	}
	if !mode.Valid() {
		errs = append(errs, fmt.Errorf("mode %q is not one of %q, %q", mode, ModeBlacklist, ModeWhitelist))
	}
	patterns := make([]string, 0, len(extensions))
	for i, ext := range extensions {
		p := strings.ToLower(strings.TrimLeft(strings.TrimSpace(ext), "."))
		if p == "" {
			continue
		}
		// An extension never contains a dot (extension takes the text after
		// the last one), so an entry with a dot ("*.exe") cannot match. A
		// comma is refused as a list written as one entry ("exe,com").
		if strings.ContainsAny(p, ".,") {
			errs = append(errs, fmt.Errorf("extension[%d] %q: an entry cannot contain '.' (it would never match) or ','; list each extension as its own entry", i, ext))
			continue
		}
		// path.Match validates the whole pattern whatever it is matched
		// against, so a pattern accepted here cannot error in Unwanted.
		if _, err := path.Match(p, ""); err != nil {
			errs = append(errs, fmt.Errorf("extension[%d] %q: %w", i, ext, err))
			continue
		}
		patterns = append(patterns, p)
	}
	if err := errors.Join(errs...); err != nil {
		return Rules{}, err
	}
	return Rules{action: action, mode: mode, patterns: patterns}, nil
}

// Action reports what the rules ask for when a name is unwanted. Callers
// check it before calling Unwanted: ActionOff disables every check.
func (r Rules) Action() Action {
	if r.action == "" {
		return ActionOff
	}
	return r.action
}

// Unwanted reports whether name has an extension the rules exclude. A name
// with no extension is never unwanted, in either mode: in whitelist mode that
// would block every obfuscated post, which is SABnzbd's reason for the same
// exception. In whitelist mode with an empty list, every name with an
// extension is unwanted.
func (r Rules) Unwanted(name string) bool {
	ext := extension(name)
	if ext == "" {
		return false
	}
	listed, err := r.listed(ext)
	if err != nil {
		// Fail closed in either mode: a check that cannot decide must not
		// let the name through.
		return true
	}
	if r.mode == ModeWhitelist {
		return !listed
	}
	return listed
}

// Find returns the names in names that Unwanted reports, in their order.
func (r Rules) Find(names []string) []string {
	var out []string
	for _, n := range names {
		if r.Unwanted(n) {
			out = append(out, n)
		}
	}
	return out
}

// listed reports whether ext matches a configured pattern, or the error of a
// pattern that cannot be matched. NewRules validates every pattern, so the
// error is unreachable through it.
func (r Rules) listed(ext string) (bool, error) {
	for _, p := range r.patterns {
		ok, err := path.Match(p, ext)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// extension returns name's extension, lowercased and without its dot, or ""
// when it has none. The name comes from an NZB and is untrusted, so it is
// normalised first:
//
//   - only the last path component counts, split on both '/' and '\', so a
//     directory named "x.exe" does not lend its extension to "x.exe/readme";
//   - trailing dots, whitespace, control characters and Unicode format
//     characters (Cf: zero-width space and joiner, BOM, soft hyphen,
//     bidi overrides) are trimmed, because Windows drops trailing dots and
//     spaces when it opens a file, so "setup.exe. " runs as setup.exe, and
//     a format character renders as nothing, so the name reads as one;
//   - the result is lowercased, so "SETUP.EXE" matches "exe".
//
// A name that is only a dot and an extension (".exe") has that extension,
// unlike Python's splitext: refusing it costs a hidden file, and passing it
// would let one through.
func extension(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimRightFunc(name, func(r rune) bool {
		return r == '.' || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	})
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

package unpack

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"

	"github.com/hobeone/rarengine"
)

// maxSymlinkHops bounds the symlink expansion physicalResolve performs, the
// same way the kernel bounds a path walk (ELOOP at 40).
const maxSymlinkHops = 40

// errLinkRefused marks a link member the extractor declined to create. It is
// not an extraction failure: the caller logs the reason and moves on, so one
// hostile or unrepresentable link does not fail the whole set.
var errLinkRefused = errors.New("link refused")

func refusef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errLinkRefused, fmt.Sprintf(format, args...))
}

// normalizeLinkTarget converts a stored link target to forward slashes and
// rejects what can never be a safe relative target: NUL bytes, an empty
// string, a rooted path, and a Windows drive or UNC prefix. rarengine hands
// symlink and junction targets over exactly as stored, so Windows-authored
// archives arrive with backslashes and with "\??\C:\..." absolute forms.
func normalizeLinkTarget(target string) (string, error) {
	if strings.ContainsRune(target, 0) {
		return "", refusef("target contains a NUL byte")
	}
	t := strings.ReplaceAll(target, "\\", "/")
	switch {
	case t == "":
		return "", refusef("target is empty")
	case strings.HasPrefix(t, "/"):
		return "", refusef("absolute target %q", target)
	case len(t) >= 2 && t[1] == ':' && (t[0]|0x20 >= 'a' && t[0]|0x20 <= 'z'):
		return "", refusef("absolute target %q (drive letter)", target)
	}
	return t, nil
}

// physicalResolve resolves rel, a slash-separated path relative to root, one
// component at a time against what is actually on disk, expanding every
// symlink it meets. It returns the cleaned root-relative result, or an error
// if the walk steps above the root or meets an absolute symlink.
//
// A lexical path.Clean is not enough here. With "a/d -> .." already on disk,
// the target "../../z" of a link at "a/d/f" cleans to "z" lexically (a/d/..
// /.. looks like root) yet resolves two levels above the root for real. This
// walk sees d as the symlink it is.
//
// Components that do not exist yet are taken as plain names. os.Root keeps
// every Lstat/Readlink inside the root regardless; an error it raises for
// any reason other than "not there" is treated as a refusal.
func physicalResolve(root *os.Root, rel string) (string, error) {
	queue := strings.Split(rel, "/")
	var stack []string
	hops := 0
	for len(queue) > 0 {
		comp := queue[0]
		queue = queue[1:]
		switch comp {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return "", refusef("path %q climbs out of the extraction root", rel)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		cand := path.Join(append(slices.Clip(stack), comp)...)
		fi, err := root.Lstat(cand)
		switch {
		case err == nil && fi.Mode()&fs.ModeSymlink != 0:
			hops++
			if hops > maxSymlinkHops {
				return "", refusef("path %q expands too many symlinks", rel)
			}
			dest, rlErr := root.Readlink(cand)
			if rlErr != nil {
				return "", refusef("path %q: cannot read symlink %q: %v", rel, cand, rlErr)
			}
			dest, nErr := normalizeLinkTarget(dest)
			if nErr != nil {
				return "", refusef("path %q passes through symlink %q: %v", rel, cand, nErr)
			}
			queue = append(strings.Split(dest, "/"), queue...)
		case err == nil, errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			stack = append(stack, comp)
		default:
			return "", refusef("path %q: cannot inspect %q: %v", rel, cand, err)
		}
	}
	return path.Join(stack...), nil
}

// resolveSymlinkTarget validates the target of a symlink or junction member
// that would be created at destRel inside root, and returns the string to
// store in the link. The target is attacker-controlled. It is resolved
// against the link's own directory (itself resolved physically, so a parent
// that is already a symlink is accounted for), and refused when it is
// absolute or its resolution leaves root. The stored value is the normalized
// original, not the cleaned one, so a legitimate "../lib/x" survives intact.
func resolveSymlinkTarget(root *os.Root, destRel, rawTarget string) (string, error) {
	target, err := normalizeLinkTarget(rawTarget)
	if err != nil {
		return "", err
	}
	parent, err := physicalResolve(root, path.Dir(destRel))
	if err != nil {
		return "", err
	}
	if _, err := physicalResolve(root, parent+"/"+target); err != nil {
		return "", err
	}
	return target, nil
}

// ExtractedEntryExists reports whether extracting the entry left something at
// destRel that belongs in a result's file list. Regular members always do. A
// link member does only if it was actually created (or already existed): a
// refused or skipped link leaves nothing, and listing it would hand later
// stages a path that is not there.
func ExtractedEntryExists(root *os.Root, destRel string, fh *rarengine.FileHeader) bool {
	if fh.IsDir {
		return false
	}
	if fh.LinkType == rarengine.LinkNone {
		return true
	}
	_, err := root.Lstat(destRel)
	return err == nil
}

// extractLinkEntry handles a link member. It never reads the entry. A refused
// or unresolvable link is logged and returns nil so the caller still closes
// the member and the verdict path runs; only filesystem failures are errors.
func extractLinkEntry(ctx context.Context, root *os.Root, destRel, destPath string, fh *rarengine.FileHeader, opts Options, log *slog.Logger) error {
	var err error
	switch fh.LinkType {
	case rarengine.LinkUnixSymlink, rarengine.LinkWindowsSymlink, rarengine.LinkWindowsJunction:
		err = createSymlinkEntry(root, destRel, destPath, fh, opts, log)
	case rarengine.LinkHardLink, rarengine.LinkFileCopy:
		err = createHardLinkEntry(ctx, root, destRel, destPath, fh, opts, log)
	case rarengine.LinkNone:
		return nil
	default:
		err = refusef("unknown link type %d", fh.LinkType)
	}
	if errors.Is(err, errLinkRefused) {
		log.Warn("go_unrar: skipping link entry", "name", fh.Name, "link_type", fh.LinkType, "err", err)
		if opts.OnLine != nil {
			opts.OnLine("Skipping link: " + fh.Name)
		}
		return nil
	}
	return err
}

// prepareLinkDest makes room for a link at destRel: it creates the parent
// directory, and either reports skip (an entry is already there and
// opts.OverwriteFiles is false) or removes a non-directory entry in the way.
func prepareLinkDest(root *os.Root, destRel, destPath string, fh *rarengine.FileHeader, opts Options, log *slog.Logger) (skip bool, err error) {
	if dir := path.Dir(destRel); dir != "." {
		if err := root.MkdirAll(dir, 0o750); err != nil {
			return false, fmt.Errorf("go_unrar: mkdir for link %s: %w", destRel, err)
		}
	}
	fi, statErr := root.Lstat(destRel)
	if statErr != nil {
		return false, nil //nolint:nilerr // nothing there (or not inspectable): creation below reports any real error
	}
	if !opts.OverwriteFiles {
		log.Info("skipping existing file", "path", destPath)
		if opts.OnLine != nil {
			opts.OnLine("Skipping existing: " + fh.Name)
		}
		return true, nil
	}
	if fi.IsDir() {
		return false, refusef("a directory already exists at %q", destRel)
	}
	if err := root.Remove(destRel); err != nil {
		return false, fmt.Errorf("go_unrar: replace %s: %w", destRel, err)
	}
	return false, nil
}

func createSymlinkEntry(root *os.Root, destRel, destPath string, fh *rarengine.FileHeader, opts Options, log *slog.Logger) error {
	target, err := resolveSymlinkTarget(root, destRel, fh.LinkTarget)
	if err != nil {
		return err
	}
	skip, err := prepareLinkDest(root, destRel, destPath, fh, opts, log)
	if err != nil || skip {
		return err
	}
	if err := root.Symlink(target, destRel); err != nil {
		return fmt.Errorf("go_unrar: create symlink %s: %w", destRel, err)
	}
	return nil
}

// createHardLinkEntry links, or copies, an already-extracted member into
// place. LinkTarget names another archive member and arrives sanitized the
// way Name does; it is passed through SanitizeArchivePath again so the same
// rules (and OneFolder flattening) apply as to every member path here. A
// target that is not yet extracted, or is not a regular file, is skipped.
func createHardLinkEntry(ctx context.Context, root *os.Root, destRel, destPath string, fh *rarengine.FileHeader, opts Options, log *slog.Logger) error {
	targetRel, err := SanitizeArchivePath(fh.LinkTarget, opts.OneFolder)
	if err != nil {
		return refusef("unsafe link target: %v", err)
	}
	fi, err := root.Lstat(targetRel)
	if err != nil {
		return refusef("link target %q is not extracted: %v", targetRel, err)
	}
	if !fi.Mode().IsRegular() {
		return refusef("link target %q is not a regular file", targetRel)
	}
	skip, err := prepareLinkDest(root, destRel, destPath, fh, opts, log)
	if err != nil || skip {
		return err
	}
	if fh.LinkType == rarengine.LinkHardLink {
		linkErr := root.Link(targetRel, destRel)
		if linkErr == nil {
			return nil
		}
		log.Debug("go_unrar: hard link failed, copying instead", "name", fh.Name, "err", linkErr)
	}
	src, err := root.Open(targetRel)
	if err != nil {
		return fmt.Errorf("go_unrar: open link target %s: %w", targetRel, err)
	}
	defer src.Close() //nolint:errcheck // read-only
	mode := fi.Mode().Perm() & 0o666
	_, err = writeEntrySafely(ctx, root, destRel, destPath, src, nil, false, mode, fh.ModificationTime, opts, fh.Name, "go_unrar", log, nil)
	return err
}

// NoteDictionaryLimit logs, when err is rarengine's dictionary-window
// refusal, a line that names the limit and the handoff, so it is not read as
// corruption. It is a no-op for any other error. fh may be nil.
func NoteDictionaryLimit(log *slog.Logger, err error, fh *rarengine.FileHeader) {
	if !errors.Is(err, rarengine.ErrDictionaryTooLarge) {
		return
	}
	var name string
	var dict int64
	if fh != nil {
		name, dict = fh.Name, fh.DictSize
	}
	log.Warn("go_unrar: archive needs a larger dictionary window than the pure-Go engine's 32 MiB limit; "+
		"this is a capacity limit, not corruption, and the external unrar is the fallback",
		"member", name, "declared_dict_bytes", dict)
}

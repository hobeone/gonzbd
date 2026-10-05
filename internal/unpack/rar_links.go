package unpack

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/hobeone/rarengine"

	"github.com/hobeone/gonzbd/internal/fsutil"
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

// isLinkUnsupported reports whether err from creating a link says the target
// filesystem cannot hold it (exFAT, SMB and many FUSE mounts return EPERM or
// ENOTSUP for links) or cannot name it (ENAMETOOLONG). That is a property of
// the destination, not damage to the archive, so it refuses the one member
// instead of failing the set.
func isLinkUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENAMETOOLONG)
}

// validateLinkTarget rejects what can never be a safe relative target: an
// empty string, a NUL byte, and a rooted path. archive is true for a target
// this code is about to store, read from an archive: those may be
// Windows-authored, so a drive-letter prefix is also refused. A target read
// back from disk is a literal path; "C:x" is an ordinary relative name there.
func validateLinkTarget(t string, archive bool) error {
	switch {
	case strings.ContainsRune(t, 0):
		return refusef("target contains a NUL byte")
	case t == "":
		return refusef("target is empty")
	case strings.HasPrefix(t, "/"):
		return refusef("absolute target %q", t)
	case archive && len(t) >= 2 && t[1] == ':' && (t[0]|0x20 >= 'a' && t[0]|0x20 <= 'z'):
		return refusef("absolute target %q (drive letter)", t)
	}
	return nil
}

// normalizeLinkTarget converts a symlink or junction target read from an
// archive to forward slashes and validates it. rarengine hands these over
// exactly as stored, so Windows-authored archives arrive with backslashes and
// with "\??\C:\..." absolute forms. It is for archive targets only: a target
// read back from disk keeps its backslashes, which on Unix are part of a name.
func normalizeLinkTarget(target string) (string, error) {
	if strings.ContainsRune(target, 0) {
		return "", refusef("target contains a NUL byte")
	}
	t := strings.ReplaceAll(target, "\\", "/")
	if err := validateLinkTarget(t, true); err != nil {
		return "", err
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
			// A target already on disk is literal: no backslash rewriting.
			if vErr := validateLinkTarget(dest, false); vErr != nil {
				return "", refusef("path %q passes through symlink %q: %v", rel, cand, vErr)
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
	if err := checkLinkAgainstDisk(root, destRel, target); err != nil {
		return "", err
	}
	return target, nil
}

// checkLinkAgainstDisk resolves the link at destRel with the given (already
// normalized) target against what is on disk now, and refuses it if the
// resolution leaves root. It does not look at whether the link exists.
func checkLinkAgainstDisk(root *os.Root, destRel, target string) error {
	parent, err := physicalResolve(root, path.Dir(destRel))
	if err != nil {
		return err
	}
	_, err = physicalResolve(root, parent+"/"+target)
	return err
}

// verifyCreatedLink re-checks a link that exists on disk against the final
// state of the tree. It is deliberately stricter than the creation-time
// check: besides the physical walk, the lexical join of the link's real parent
// directory (EvalSymlinks) and its target must be inside the real root. That
// refuses a target such as "hop/../x" through a symlink "hop" even when the
// physical walk would stay inside; a conservative false positive is the right
// side to err on once the archive has been extracted.
func verifyCreatedLink(root *os.Root, destRel string) error {
	target, err := root.Readlink(destRel)
	if err != nil {
		return refusef("cannot read back %q: %v", destRel, err)
	}
	if err := validateLinkTarget(target, false); err != nil {
		return err
	}
	if err := checkLinkAgainstDisk(root, destRel, target); err != nil {
		return err
	}
	realRoot, err := filepath.EvalSymlinks(root.Name())
	if err != nil {
		return refusef("cannot resolve extraction root: %v", err)
	}
	realParent, err := filepath.EvalSymlinks(filepath.Join(root.Name(), filepath.FromSlash(path.Dir(destRel))))
	if err != nil {
		return refusef("cannot resolve parent of %q: %v", destRel, err)
	}
	if !fsutil.PathWithin(realRoot, filepath.Join(realParent, filepath.FromSlash(target))) {
		return refusef("target %q of %q resolves outside the extraction root", target, destRel)
	}
	return nil
}

// SymlinkBatch collects the symlink members of one extraction session so they
// can be created after the archive's last member (see Finish). Regular files
// and hard links are written as the archive is read; symlinks wait, because a
// link validated while later members are still to come can be made to escape
// by one of them: a later symlink planted at a component an earlier link's
// target passes through, or (with OverwriteFiles) a regular file replaced by a
// symlink. Creating every link last, then re-checking each against the final
// tree, closes that ordering dependence.
//
// A SymlinkBatch is not safe for concurrent use; each extraction loop owns one.
type SymlinkBatch struct {
	pending []pendingSymlink
}

type pendingSymlink struct {
	destRel, destPath, name, target string
	fh                              *rarengine.FileHeader
}

// NewSymlinkBatch returns an empty batch.
func NewSymlinkBatch() *SymlinkBatch { return &SymlinkBatch{} }

// Len reports how many symlinks are waiting for Finish.
func (b *SymlinkBatch) Len() int { return len(b.pending) }

// Finish creates the recorded symlinks and then re-validates every one it
// created against the final on-disk state, removing (and logging) any that
// escapes the root. It returns the paths, relative to root and slash
// separated, of the links that remain. A link the destination cannot hold
// (see isLinkUnsupported) or that fails validation is skipped with a log line;
// any other filesystem error fails the call.
func (b *SymlinkBatch) Finish(root *os.Root, opts Options, log *slog.Logger) ([]string, error) {
	var created []string
	for _, p := range b.pending {
		target, err := resolveSymlinkTarget(root, p.destRel, p.target)
		var skip bool
		if err == nil {
			skip, err = prepareLinkDest(root, p.destRel, p.destPath, p.fh, opts, log)
		}
		if err == nil && !skip {
			if err = root.Symlink(target, p.destRel); err != nil {
				if isLinkUnsupported(err) {
					err = refusef("the destination cannot hold this link: %v", err)
				} else {
					return created, fmt.Errorf("go_unrar: create symlink %s: %w", p.destRel, err)
				}
			}
		}
		if err != nil {
			if !errors.Is(err, errLinkRefused) {
				return created, err
			}
			noteRefusedLink(log, opts, p.name, p.fh.LinkType, err)
			continue
		}
		if !skip {
			created = append(created, p.destRel)
		}
	}
	b.pending = nil

	// Re-validate until stable: removing a link can change how another one
	// resolves, so one pass is not enough in principle.
	for changed := true; changed; {
		changed = false
		kept := created[:0]
		for _, rel := range created {
			if err := verifyCreatedLink(root, rel); err != nil {
				if rmErr := root.Remove(rel); rmErr != nil {
					return kept, fmt.Errorf("go_unrar: remove escaping symlink %s: %w", rel, rmErr)
				}
				log.Warn("go_unrar: removed symlink that escapes the extraction root once the whole archive was extracted",
					"path", rel, "err", err)
				if opts.OnLine != nil {
					opts.OnLine("Removed unsafe link: " + rel)
				}
				changed = true
				continue
			}
			kept = append(kept, rel)
		}
		created = kept
	}
	return created, nil
}

// ExtractedEntryExists reports whether extracting the entry left something at
// destRel that belongs in a result's file list. Regular members always do. A
// hard link or file copy does only if it was actually created (or already
// existed): a refused or skipped link leaves nothing, and listing it would
// hand later stages a path that is not there. Symlink members never do here:
// they are created later, by SymlinkBatch.Finish, which reports its own.
func ExtractedEntryExists(root *os.Root, destRel string, fh *rarengine.FileHeader) bool {
	if fh.IsDir {
		return false
	}
	switch fh.LinkType {
	case rarengine.LinkNone:
		return true
	case rarengine.LinkUnixSymlink, rarengine.LinkWindowsSymlink, rarengine.LinkWindowsJunction:
		return false
	}
	_, err := root.Lstat(destRel)
	return err == nil
}

func noteRefusedLink(log *slog.Logger, opts Options, name string, lt rarengine.LinkType, err error) {
	log.Warn("go_unrar: skipping link entry", "name", name, "link_type", lt, "err", err)
	if opts.OnLine != nil {
		opts.OnLine("Skipping link: " + name)
	}
}

// extractLinkEntry handles a link member. It never reads the entry. A refused
// or unresolvable link is logged and returns nil so the caller still closes
// the member and the verdict path runs; only filesystem failures are errors.
func extractLinkEntry(ctx context.Context, root *os.Root, destRel, destPath string, fh *rarengine.FileHeader, opts Options, log *slog.Logger) error {
	var err error
	switch fh.LinkType {
	case rarengine.LinkUnixSymlink, rarengine.LinkWindowsSymlink, rarengine.LinkWindowsJunction:
		err = recordSymlinkEntry(root, destRel, destPath, fh, opts, log)
	case rarengine.LinkHardLink, rarengine.LinkFileCopy:
		err = createHardLinkEntry(ctx, root, destRel, destPath, fh, opts, log)
	case rarengine.LinkNone:
		return nil
	default:
		err = refusef("unknown link type %d", fh.LinkType)
	}
	if errors.Is(err, errLinkRefused) {
		noteRefusedLink(log, opts, fh.Name, fh.LinkType, err)
		return nil
	}
	return err
}

// recordSymlinkEntry applies the symlink policy and queues the link. With
// opts.ExtractSymlinks off (the default) the member is skipped. Under
// OneFolder the member paths are flattened but a target is not rewritten, so
// only a bare sibling name still points at the right member; anything else is
// refused. The link is not created here: see SymlinkBatch.
func recordSymlinkEntry(root *os.Root, destRel, destPath string, fh *rarengine.FileHeader, opts Options, log *slog.Logger) error {
	if !opts.ExtractSymlinks {
		log.Warn(fmt.Sprintf("go_unrar: skipping symlink member %s: extract_symlinks is off", fh.Name))
		if opts.OnLine != nil {
			opts.OnLine(fmt.Sprintf("Skipping symlink member %s: extract_symlinks is off", fh.Name))
		}
		return nil
	}
	if opts.Symlinks == nil {
		return refusef("no symlink batch to defer creation to")
	}
	target, err := normalizeLinkTarget(fh.LinkTarget)
	if err != nil {
		return err
	}
	if opts.OneFolder {
		if c := path.Clean(target); c != path.Base(c) || c == ".." || c == "." {
			return refusef("target %q would leave the flattened layout (flat unpack is on)", fh.LinkTarget)
		}
	}
	// Reject the plainly hostile now, for a prompt log line; the decisive
	// check is Finish's, against the final tree.
	if err := checkLinkAgainstDisk(root, destRel, target); err != nil {
		return err
	}
	opts.Symlinks.pending = append(opts.Symlinks.pending, pendingSymlink{destRel: destRel, destPath: destPath, name: fh.Name, target: fh.LinkTarget, fh: fh})
	return nil
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
	if err != nil && isLinkUnsupported(err) {
		return refusef("the destination cannot hold this link: %v", err)
	}
	return err
}

// NoteDictionaryLimit logs, when err is rarengine's dictionary-window
// refusal, a line that names the limit and what happens next, so it is not
// read as corruption. It is a no-op for any other error. fh may be nil.
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
		"this is a capacity limit, not corruption. The external unrar is tried next only when "+
		"go_rar_fallback is on and unrar is installed; otherwise the set fails",
		"member", name, "declared_dict_bytes", dict)
}

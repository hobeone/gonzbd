package unpack

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

const unpackStagePrefix = ".gonzbd-tmp-unpack-"

// CleanupStageDirs removes any leftover .gonzbd-tmp-unpack-* staging
// directories from an earlier interrupted staged extraction in outDir (#768).
func CleanupStageDirs(outDir string) {
	if outDir == "" {
		return
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), unpackStagePrefix) {
			_ = removeStageTree(filepath.Join(outDir, e.Name())) //nolint:errcheck // best effort; a stage left behind is retried by every later CleanupStageDirs
		}
	}
}

// removeStageDir removes a staging directory once its extraction is over. A stage left behind sits inside outDir, which may be
// the job's output, until a later CleanupStageDirs, so a failure is logged.
func removeStageDir(log *slog.Logger, stageDir string) {
	if err := removeStageTree(stageDir); err != nil {
		log.Warn("unpack: failed to remove the staging directory", "dir", stageDir, "err", err)
	}
}

// removeStageTree removes stageDir and everything under it. A directory the
// extractor left without owner write or search permission stops
// os.RemoveAll, so on failure it grants the owner those bits and tries again
// (#862).
func removeStageTree(stageDir string) error {
	err := os.RemoveAll(stageDir)
	if err == nil {
		return nil
	}
	if ownErr := ownStageDirs(stageDir); ownErr != nil {
		return errors.Join(err, ownErr)
	}
	return os.RemoveAll(stageDir)
}

// ownStageDirs gives the owner read, write and search permission on stageDir
// and every directory under it. An external extractor restores the
// directory modes the archive stores, and a directory without owner write
// can have no entry renamed out of it or removed from it (#862). A directory
// is changed before its entries are read, so one without owner read or
// search is walked too.
func ownStageDirs(stageDir string) (err error) {
	root, err := os.OpenRoot(stageDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || !d.IsDir() {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if perm := info.Mode().Perm(); perm&0o700 != 0o700 {
			return root.Chmod(path, perm|0o700)
		}
		return nil
	})
}

// prepareStageDir cleans up any leftover .gonzbd-tmp-unpack-* directories in
// outDir and creates a fresh staging directory inside outDir (#768).
func prepareStageDir(outDir string) (string, error) {
	CleanupStageDirs(outDir)
	return os.MkdirTemp(outDir, unpackStagePrefix+"*")
}

// ExtractStaged runs extract on a fresh staging directory inside outDir and,
// only when extract returns nil, publishes the files it wrote there into outDir
// through publishStagedExtraction, returning their paths relative to outDir.
// The stage is removed on every return, so a failed or abandoned extraction
// leaves nothing in outDir: no file, no directory, and no file it would have
// replaced. A stage a crash leaves behind carries the .gonzbd-tmp-unpack-
// prefix, so the next CleanupStageDirs of outDir removes it.
//
// It is the staging step of every archive extractor of this package (through
// extractInStage) and of DirectUnpack. With opts.OneFolder and without
// opts.OverwriteFiles, a file whose name outDir already holds is published
// under a uniquePath name, the rule memberPath applies inside the stage,
// rather than skipped.
//
// A failed publish is undone by publishStagedExtraction, which also states
// what the undo leaves behind under opts.OverwriteFiles. The returned list is
// nil on every error.
func ExtractStaged(log *slog.Logger, outDir string, opts Options, extract func(stageDir string) error) ([]string, error) {
	stageDir, err := prepareStageDir(outDir)
	if err != nil {
		return nil, err
	}
	defer removeStageDir(log, stageDir)
	if err := extract(stageDir); err != nil {
		return nil, err
	}
	published, err := publishStagedExtraction(log, outDir, stageDir, opts)
	if err != nil {
		return nil, fmt.Errorf("unpack: publish staged files: %w", err)
	}
	return published, nil
}

// extractInStage runs one extraction attempt of an archive extractor through
// ExtractStaged, so the attempt reaches outDir only if it succeeds. extract
// writes into the stage it is given and returns the attempt's Result, whose
// ExtractedFiles it leaves empty: on success extractInStage sets it to what
// the publish moved into outDir, relative to outDir.
//
// Before the attempt it refuses an archive whose declared unpacked size does
// not fit above opts.MinFreeSpace (checkFreeSpace), and it classifies an
// attempt that filled the disk as FailDiskFull (withDiskFull).
//
// Every archive extractor goes through it: `git grep -n 'extractInStag[e]('
// -- 'internal/unpack/*.go' ':!*_test.go'` returns 6 lines, this definition
// and the calls in GoUnRAR, GoSevenZip, GoTar, UnRAR and SevenZip.
// DirectUnpack calls ExtractStaged itself, and applies the free-space bound
// and the disk-full classification through CheckRarVolumesFreeSpace and
// IsDiskFull.
func extractInStage(ctx context.Context, log *slog.Logger, archive Archive, outDir, password string, opts Options, extract func(stageDir string) (Result, error)) (Result, error) {
	if err := checkFreeSpace(ctx, log, archive, outDir, password, opts.MinFreeSpace); err != nil {
		if opts.OnLine != nil {
			opts.OnLine("ERROR: " + err.Error())
		}
		return withDiskFull(Result{}, err)
	}
	var res Result
	published, err := ExtractStaged(log, outDir, opts, func(stageDir string) error {
		var err error
		res, err = extract(stageDir)
		return err
	})
	if err != nil {
		return withDiskFull(res, err)
	}
	res.ExtractedFiles = published
	return res, nil
}

// publishStagedExtraction atomically renames each completed file from stageDir
// into outDir after an extraction into the stage has succeeded, and returns
// the relative paths of the published files (#768). When opts.OverwriteFiles
// is false, a file whose name outDir already holds is skipped, the existing
// file preserved — or, with opts.OneFolder, published under a uniquePath name.
//
// It enforces the "no links in a job" rule (see rar_links.go) on whatever the
// extraction left in the stage: unrar can leave hard links there (and
// symlinks, without -ol-), and 7z can leave symlinks. Only regular files are
// published. A file with a name outside the stage is not published at all;
// of several names for one inode, all inside the stage, only the first in
// walk (lexical) order is, so the surviving name of a hard-linked pair is not
// necessarily the archive's original member.
//
// Under opts.OneFolder a directory in the stage creates nothing in outDir:
// a flat extraction keeps no directories (7z e still creates the archive's).
// Otherwise a directory is created in outDir with 0o750 whatever mode the
// archive stored for it. Either way the stage's own directories are first
// made owner-writable by ownStageDirs, so their files can be renamed out
// (#862).
//
// An error once the walk has begun undoes the publish (undoPublish) and
// returns a nil list; an error before it (opening outDir, ownStageDirs,
// countStagedNames) has published nothing to undo. The undo removes only the
// names that were free before the publish, and the directories it created. A
// name the publish replaced under opts.OverwriteFiles keeps its replacement,
// as it would without the undo: the earlier content is gone at the rename and
// is not restored, and the name holds this failed set's member, which can be
// a file an earlier set of the same job published. The check that a name was
// free is one Lstat before its rename, so it assumes no other writer in
// outDir during the publish.
func publishStagedExtraction(log *slog.Logger, outDir, stageDir string, opts Options) ([]string, error) {
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	if err := ownStageDirs(stageDir); err != nil {
		return nil, err
	}
	staged, err := countStagedNames(stageDir)
	if err != nil {
		return nil, err
	}
	stageBase := filepath.Base(stageDir)
	var published []string
	// added is the part of published whose names were free before the rename,
	// the only files undoPublish removes.
	var added []string
	var created []string
	known := make(map[string]struct{})
	seen := make(map[fileID]struct{})
	err = filepath.WalkDir(stageDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == stageDir {
			return nil
		}
		rel, err := filepath.Rel(stageDir, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if opts.OneFolder {
				return nil
			}
			return mkdirAllTracked(root, rel, known, &created)
		}
		if skip, err := isStagedLink(d, staged, seen); err != nil || skip {
			if skip {
				log.Warn("skipping link in extracted output: extraction creates no links in a job",
					"path", rel, "mode", d.Type())
				if opts.OnLine != nil {
					opts.OnLine("Skipping link: " + rel)
				}
			}
			return err
		}
		// uniquePath returns rel itself when rel is free, and when it finds
		// no free name, so the check below still skips a name it could not
		// replace.
		dest := rel
		if opts.OneFolder && !opts.OverwriteFiles {
			dest = uniquePath(root, rel)
		}
		_, lerr := root.Lstat(dest)
		// free only on a definite not-exist: a name whose Lstat failed any
		// other way may hold a file, so the undo must not remove it.
		free := errors.Is(lerr, fs.ErrNotExist)
		if lerr == nil && !opts.OverwriteFiles {
			log.Info("skipping existing file", "path", filepath.Join(outDir, rel))
			if opts.OnLine != nil {
				opts.OnLine("Skipping existing: " + rel)
			}
			return nil
		}
		if dir := filepath.Dir(dest); dir != "." {
			if err := mkdirAllTracked(root, dir, known, &created); err != nil {
				return err
			}
		}
		if err := root.Rename(filepath.Join(stageBase, rel), dest); err != nil {
			return err
		}
		published = append(published, dest)
		if free {
			added = append(added, dest)
		}
		return nil
	})
	if err != nil {
		return nil, errors.Join(err, undoPublish(root, outDir, added, created))
	}
	return published, nil
}

// mkdirAllTracked is root.MkdirAll(rel, 0o750) that also appends to created
// every level of rel that did not exist before, parents first. known holds
// directories a successful call has already ensured, so a repeat call for one
// costs nothing. A level counts as missing only when Lstat reports it does
// not exist; any other Lstat result (a regular file in the path, say) ends the
// search. The missing levels are recorded even when MkdirAll fails, since it
// may have made some first, so a recorded level can be absent; undoPublish
// ignores a not-exist result.
func mkdirAllTracked(root *os.Root, rel string, known map[string]struct{}, created *[]string) error {
	if _, ok := known[rel]; ok {
		return nil
	}
	var missing []string
	for dir := rel; dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if _, ok := known[dir]; ok {
			break
		}
		if _, err := root.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			break
		}
		missing = append(missing, dir)
	}
	err := root.MkdirAll(rel, 0o750)
	slices.Reverse(missing)
	*created = append(*created, missing...)
	if err != nil {
		return err
	}
	known[rel] = struct{}{}
	for _, dir := range missing {
		known[dir] = struct{}{}
	}
	return nil
}

// undoPublish removes what a failed publish put into outDir: each added file,
// the published files whose names were free before, then each created
// directory, deepest first. A name the publish replaced is not in added and
// is left holding its replacement. A directory is removed non-recursively
// with root.Remove, so one that still holds something this publish did not
// write is reported and left, never deleted; that includes a directory
// holding only NFS silly-rename files, which fsutil offers no non-recursive,
// root-relative removal for. Every removal failure is joined into the result.
func undoPublish(root *os.Root, outDir string, added, created []string) error {
	var errs []error
	for _, p := range added {
		if err := fsutil.RemoveRootAll(root, p, filepath.Join(outDir, p)); err != nil {
			errs = append(errs, err)
		}
	}
	for _, dir := range slices.Backward(created) {
		if err := root.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// fileID names an inode.
type fileID struct{ dev, ino uint64 }

// multiLinked returns the inode of info, whether it has more than one
// name and its link count. multi is false where the platform reports no link
// count.
func multiLinked(info os.FileInfo) (id fileID, nlink uint64, multi bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink <= 1 {
		return fileID{}, 0, false
	}
	return fileID{dev: uint64(st.Dev), ino: st.Ino}, uint64(st.Nlink), true //nolint:unconvert // Dev and Nlink widths vary by platform
}

// countStagedNames counts, for each regular file under stageDir with more
// than one name, how many of its names are under stageDir.
func countStagedNames(stageDir string) (map[fileID]uint64, error) {
	counts := make(map[fileID]uint64)
	err := filepath.WalkDir(stageDir, func(_ string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || !d.Type().IsRegular() {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if id, _, multi := multiLinked(info); multi {
			counts[id]++
		}
		return nil
	})
	return counts, err
}

// isStagedLink reports whether the non-directory stage entry d must not be
// published: anything but a regular file (a symlink above all); a regular
// file with a name outside the stage (fewer of its names in staged, the
// counts from countStagedNames, than its link count), which no name of may
// reach the job; or a regular file whose inode was already met under another
// name in seen. A first name is recorded in seen.
func isStagedLink(d os.DirEntry, staged map[fileID]uint64, seen map[fileID]struct{}) (bool, error) {
	if !d.Type().IsRegular() {
		return true, nil
	}
	info, err := d.Info()
	if err != nil {
		return false, err
	}
	id, nlink, multi := multiLinked(info)
	if !multi {
		return false, nil
	}
	if staged[id] < nlink {
		return true, nil
	}
	if _, dup := seen[id]; dup {
		return true, nil
	}
	seen[id] = struct{}{}
	return false, nil
}

// snapshotDir returns a set of every non-directory entry (relative paths)
// under dir: regular files, and also symlinks or other special files. UnRAR
// reads it to tell whether an unrar that corrected a filename and exited
// non-zero left anything in its stage.
func snapshotDir(dir string) (map[string]struct{}, error) {
	files := make(map[string]struct{})
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		files[rel] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

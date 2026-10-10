package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/postproc"
)

// errRetryDirConflict refuses a retry whose failed attempt's bytes cannot be
// put back where the retry writes without touching something else: the
// directory the retry would write to already exists (restoreFailedDir), or
// another job holds its name (Dispatcher.ReserveName, in retryHistoryJob).
var errRetryDirConflict = errors.New("cannot restore the failed download directory")

// restoreFailedDir moves a failed job's download directory back from the
// _FAILED_ name the finalize stage gave it (postproc.FailedDir) to
// downloadDir/name, the path the retry writes to and post-processing reads.
//
// It acts only when recordedPath, the history entry's path, is exactly that
// _FAILED_ sibling; any other recorded path means no rename happened and the
// bytes are already at downloadDir/name. It returns the path it moved from, or
// "" when it moved nothing, so that a retry which then aborts can move the
// directory back with undoRestoreFailedDir.
//
// It refuses with errRetryDirConflict, moving nothing, when downloadDir/name
// already exists: it is a directory that is not this job's, and the retry would
// write into it. It also refuses when the _FAILED_ directory is gone but
// downloadDir/name exists, since nothing says whose that directory is. Where
// neither exists there is nothing to move: the bytes are gone, or the download
// root is not mounted, and nothing here tells the two apart.
//
// It does not check whether a job holds name: its caller must have reserved it
// (Dispatcher.ReserveName) first, which refuses a holder before anything moves.
func restoreFailedDir(recordedPath, downloadDir, name string) (string, error) {
	jobDir := filepath.Join(downloadDir, name)
	failedDir := postproc.FailedDir(jobDir)
	if recordedPath != failedDir {
		return "", nil
	}
	srcInfo, srcErr := os.Lstat(failedDir)
	_, dstErr := os.Lstat(jobDir)
	if srcErr != nil && !errors.Is(srcErr, fs.ErrNotExist) {
		return "", fmt.Errorf("stat %s: %w", failedDir, srcErr)
	}
	if dstErr != nil && !errors.Is(dstErr, fs.ErrNotExist) {
		return "", fmt.Errorf("stat %s: %w", jobDir, dstErr)
	}
	srcExists, dstExists := srcErr == nil, dstErr == nil
	switch {
	case !srcExists && !dstExists:
		return "", nil
	case !srcExists:
		return "", fmt.Errorf("%w: %s is gone and %s exists, and may belong to another job",
			errRetryDirConflict, failedDir, jobDir)
	case !srcInfo.IsDir():
		return "", fmt.Errorf("%w: %s is not a directory", errRetryDirConflict, failedDir)
	case dstExists:
		return "", fmt.Errorf("%w: %s already exists; move or remove it, then retry",
			errRetryDirConflict, jobDir)
	}
	if err := renameWithin(downloadDir, failedDir, jobDir); err != nil {
		return "", err
	}
	return failedDir, nil
}

// undoRestoreFailedDir moves the directory restoreFailedDir moved back to the
// _FAILED_ path the history entry still records, for a retry that aborted
// after the restore. from is restoreFailedDir's result; "" is a no-op.
func undoRestoreFailedDir(from, downloadDir, name string) error {
	if from == "" {
		return nil
	}
	return renameWithin(downloadDir, filepath.Join(downloadDir, name), from)
}

// renameWithin renames oldPath to newPath through an os.Root anchored at base,
// so neither end can resolve outside it.
func renameWithin(base, oldPath, newPath string) error {
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	oldRel, err := filepath.Rel(base, oldPath)
	if err != nil {
		return err
	}
	newRel, err := filepath.Rel(base, newPath)
	if err != nil {
		return err
	}
	if err := root.Rename(oldRel, newRel); err != nil {
		return fmt.Errorf("rename %s to %s: %w", oldPath, newPath, err)
	}
	return nil
}

// queuedName reports whether a job the dispatcher holds is named name.
func (app *Application) queuedName(name string) bool {
	if app.dispatcher == nil {
		return false
	}
	for _, row := range app.dispatcher.List() {
		if row.Header.Name == name {
			return true
		}
	}
	return false
}

// checkRecordedUnderCurrentBase refuses, with errRetryDirConflict, a retry
// whose history entry records a path that is neither directly under
// downloadDir nor inside completeDir. Such an entry was recorded under another
// base, so its bytes are not where the retry would write and the retry would
// resume from retained progress whose files are missing. Both sides of the
// download_dir comparison are cleaned because the configured value is not
// (a trailing slash or a "./" prefix is legal there).
func checkRecordedUnderCurrentBase(recordedPath, downloadDir, completeDir string) error {
	if recordedPath == "" || filepath.Dir(filepath.Clean(recordedPath)) == filepath.Clean(downloadDir) {
		return nil
	}
	if completeDir != "" {
		if rel, err := filepath.Rel(filepath.Clean(completeDir), filepath.Clean(recordedPath)); err == nil && filepath.IsLocal(rel) {
			return nil
		}
	}
	return fmt.Errorf("%w: its files are at %s, not under the current download_dir or complete_dir",
		errRetryDirConflict, recordedPath)
}

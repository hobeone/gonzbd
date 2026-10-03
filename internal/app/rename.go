package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
)

// ErrInvalidJobName reports a rename refused because the requested name,
// once spaces and dots are trimmed, leaves nothing to name a directory with.
var ErrInvalidJobName = errors.New("invalid job name")

// jobNameTaken reports whether name is unavailable as a job's directory
// name: another queued job has it, or something exists at that name in the
// download directory, the complete directory, or a category directory under
// it. AddJob and RenameJob both pass it to uniqueName.
func (app *Application) jobNameTaken(snap *config.Config, name string) bool {
	if app.queuedName(name) {
		return true
	}
	gen := &snap.General
	// Lstat, not Stat, for the reason given on fsutil.GetUniqueRelPath:
	// this decides whether a job directory name is available to create, and
	// a dangling symlink at that name reads as absent under Stat. The
	// MkdirAll that follows would then resolve the link rather than make the
	// directory we chose.
	if _, err := os.Lstat(filepath.Join(gen.DownloadDir, name)); err == nil {
		return true
	}
	if _, err := os.Lstat(filepath.Join(gen.CompleteDir, name)); err == nil {
		return true
	}
	for _, cat := range snap.Categories {
		if cat.Dir == "" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(gen.CompleteDir, cat.Dir, name)); err == nil {
			return true
		}
	}
	return false
}

// RenameJob renames a queued job and returns the name it was given. The name
// is the job's download directory (DownloadDir/<name>), so it is chosen as
// an ingest name is: sanitised with fsutil.SanitizeFolderName, then made
// unique with uniqueName and jobNameTaken, as AddJob does. A name that trims
// to nothing is refused with ErrInvalidJobName. Dispatcher.SetName then
// refuses anything that is still not one path component or that another
// job took meanwhile.
func (app *Application) RenameJob(id, name string) (string, error) {
	if app.dispatcher == nil {
		return "", errors.New("app: rename: dispatcher not wired")
	}
	if strings.Trim(name, " .") == "" {
		return "", fmt.Errorf("app: rename %s to %q: %w", id, name, ErrInvalidJobName)
	}
	row, ok := app.dispatcher.Row(id)
	if !ok {
		return "", app.dispatcher.SetName(id, name) // reports ErrNotFound
	}
	snap := app.config.Snapshot()
	name = fsutil.SanitizeFolderName(name, snap.Downloads.SanitizeOptions())
	if name == row.Header.Name {
		return name, nil
	}
	name = uniqueName(name, func(n string) bool { return app.jobNameTaken(snap, n) })
	if err := app.dispatcher.SetName(id, name); err != nil {
		return "", fmt.Errorf("app: rename %s: %w", id, err)
	}
	return name, nil
}

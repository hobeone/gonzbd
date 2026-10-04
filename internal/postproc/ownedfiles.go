package postproc

import (
	"io/fs"
	"os"
	"path/filepath"
)

// markOwned records paths as owned by job. Entries in paths may be relative
// to job.DownloadDir (as produced by some extractors' before/after
// directory-snapshot diff) or already absolute; both forms are normalized to
// absolute before insertion. A no-op when job.OwnedFiles is nil (tracking
// disabled — see Job.OwnedFiles doc comment).
func markOwned(job *Job, paths []string) {
	if job.OwnedFiles == nil {
		return
	}
	for _, p := range paths {
		abs := p
		if !filepath.IsAbs(p) {
			abs = filepath.Join(job.DownloadDir, p)
		}
		job.OwnedFiles[abs] = struct{}{}
	}
}

// markRenamed moves ownership from an old absolute path to a new one,
// tracking in-place renames performed by par2-name-recovery and
// deobfuscation. A no-op when job.OwnedFiles is nil.
func markRenamed(job *Job, from, to string) {
	if job.OwnedFiles == nil {
		return
	}
	delete(job.OwnedFiles, from)
	job.OwnedFiles[to] = struct{}{}
}

// snapshotOwnedFiles returns the set of absolute paths of every regular file
// currently under dir. Used to seed Job.OwnedFiles at the start of
// processJob. dir (job.DownloadDir) is named after the job. No other
// registered job has that name (Dispatcher.nameHolderLocked, at registration
// and at rename), and AddJob and RenameJob choose only a name with nothing on
// disk under it (jobNameTaken). So the files present at that moment are this
// job's own: its download's, or a retried attempt's — unless something
// outside gonzbd created that directory after the name was chosen.
func snapshotOwnedFiles(dir string) (map[string]struct{}, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck // read-only close

	owned := make(map[string]struct{})
	walkErr := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path == "." {
			return nil
		}
		owned[filepath.Join(dir, path)] = struct{}{}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return owned, nil
}

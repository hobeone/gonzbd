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
// processJob: since dir (job.DownloadDir) is exclusive to this job for its
// entire lifetime (see the job-name uniqueness invariant enforced by
// queue.Add / app.AddJob), every file present at that moment was produced by
// this job's own download — nothing else can have written there.
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

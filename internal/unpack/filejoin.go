package unpack

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

const joinBufSize = 16 * 1024 * 1024 // 16 MiB write buffer (SABnzbd uses 24 MiB)

// FileJoin concatenates split parts into a single output file.
//
// archive.Type must be SplitArchive and archive.Parts must be sorted in
// ascending numeric order (.001, .002, …).  The joined file is written to
// outDir/<archive.Name>.  The function validates part contiguity before
// writing; a gap in the sequence (e.g. .001, .003) results in an error.
//
// If opts.KeepOriginals is false, the caller is responsible for deleting
// archive.Parts after a successful join; FileJoin itself never deletes files.
//
// ctx is checked between parts; cancellation stops the join and removes the
// partial output file.
func FileJoin(ctx context.Context, log *slog.Logger, archive Archive, outDir string, _ Options) (Result, error) {
	log = log.With("component", "filejoin")
	if archive.Type != SplitArchive {
		return Result{Err: fmt.Errorf("filejoin: archive type is not SplitArchive")},
			fmt.Errorf("filejoin: archive type is not SplitArchive")
	}
	if len(archive.Parts) == 0 {
		return Result{Err: fmt.Errorf("filejoin: no parts in archive")},
			fmt.Errorf("filejoin: no parts in archive")
	}

	outPath := filepath.Join(outDir, archive.Name)
	// outRel is just the archive name (a filename, no directory component),
	// used for all root-relative operations.
	outRel := archive.Name

	log.Info("filejoin: starting join",
		"name", archive.Name,
		"parts", len(archive.Parts),
		"outPath", outPath,
	)

	// Open the output directory as an os.Root. All writes go through this
	// rooted handle so the join output cannot escape outDir via "..", an
	// absolute path, or a symlinked path component.
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return Result{Err: err}, fmt.Errorf("filejoin: open root: %w", err)
	}
	defer root.Close() //nolint:errcheck // close after all writes complete

	// If a regular file already exists at the final name, treat the join as a
	// successful no-op (§8.1). Because FileJoin writes to a .gonzbd-tmp-*
	// sibling and renames into place only after flush, fsync, and close, an
	// interrupted FileJoin never leaves a partial file at outRel. An existing
	// regular file at outRel is preserved rather than overwritten: it may be
	// the output of a previous join whose archive cleanup was interrupted
	// (including after .001 was already unlinked, which is why this check runs
	// before sortedNumericParts), a target recreated from the parts by par2
	// repair before unpack ran (where overwriting with a raw concatenation of
	// a damaged part would discard the repair without re-verifying), or a file
	// delivered by an earlier extraction pass or the post itself.
	if info, err := root.Lstat(outRel); err == nil && info.Mode().IsRegular() {
		log.Info("filejoin: output already exists, skipping join", "outPath", outPath)
		return Result{ExtractedFiles: []string{outPath}}, nil
	}

	// Validate contiguity after checking whether outRel already exists, so a
	// crash during archive cleanup after .001 was already removed does not fail
	// the rerun of an already-completed join.
	parts, err := sortedNumericParts(archive.Parts)
	if err != nil {
		return Result{Err: err}, fmt.Errorf("filejoin: %w", err)
	}

	outFile, tmpRel, err := fsutil.RootedCreateTempPerm(ctx, root, outRel, 0o666)
	if err != nil {
		return Result{Err: err}, fmt.Errorf("filejoin: create output: %w", err)
	}

	var published bool
	defer func() {
		if !published {
			_ = outFile.Close()     //nolint:errcheck // best-effort cleanup
			_ = root.Remove(tmpRel) //nolint:errcheck // best-effort cleanup
		}
	}()

	bw := bufio.NewWriterSize(outFile, joinBufSize)

	totalParts := len(parts)
	for i, part := range parts {
		// Honour context cancellation between parts.
		if err := ctx.Err(); err != nil {
			return Result{Err: err}, fmt.Errorf("filejoin: cancelled: %w", err)
		}

		if err := copyPart(bw, part); err != nil {
			return Result{Err: err}, fmt.Errorf("filejoin: copy %s: %w", part, err)
		}

		// N2: Report progress after each part (matches SABnzbd's
		// percentage reporting during file_join).
		pct := float64(i+1) / float64(totalParts) * 100
		log.Info("filejoin: progress",
			"name", archive.Name,
			"part", i+1,
			"total", totalParts,
			"pct", fmt.Sprintf("%.0f%%", pct),
		)
	}

	if err := bw.Flush(); err != nil {
		return Result{Err: err}, fmt.Errorf("filejoin: flush: %w", err)
	}

	if err := syncAndPublishJoin(outFile, root, tmpRel, outRel); err != nil {
		return Result{Err: err}, err
	}
	published = true

	log.Info("filejoin: join complete", "outPath", outPath, "parts", len(parts))
	return Result{ExtractedFiles: []string{outPath}}, nil
}

// syncAndPublishJoin fsyncs outFile, closes it, atomically renames tmpRel to
// outRel inside root, and fsyncs the parent directory so both the joined
// file's data blocks and its directory entry survive a power loss.
func syncAndPublishJoin(outFile *os.File, root *os.Root, tmpRel, outRel string) error {
	if err := outFile.Sync(); err != nil {
		return fmt.Errorf("filejoin: sync output: %w", err)
	}
	if err := outFile.Close(); err != nil {
		return fmt.Errorf("filejoin: close output: %w", err)
	}
	if err := root.Rename(tmpRel, outRel); err != nil {
		return fmt.Errorf("filejoin: publish output: %w", err)
	}
	if dirFile, err := root.Open(filepath.Dir(outRel)); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

// copyPart opens part and copies its contents into w.
func copyPart(w io.Writer, part string) error {
	f, err := os.Open(part) //nolint:gosec // part is caller-supplied, not constructed from user input
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close() //nolint:errcheck // read-only; close error is harmless
	}()

	if _, err := io.Copy(w, f); err != nil {
		return err
	}
	return nil
}

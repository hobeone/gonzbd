package unpack

import (
	"bufio"
	"context"
	"errors"
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

	// Validate contiguity.
	if _, err := sortedNumericParts(archive.Parts); err != nil {
		return Result{Err: err}, fmt.Errorf("filejoin: %w", err)
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

	// O_EXCL atomically refuses to create the file if it already exists,
	// avoiding the TOCTOU race of a separate Stat check.
	outFile, err := fsutil.RootedOpenFile(root, outRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Output already exists — treat as successful no-op (§8.1).
			// This handles re-runs after a crash where the join completed
			// but source cleanup didn't finish.
			log.Info("filejoin: output already exists, skipping join", "outPath", outPath)
			return Result{ExtractedFiles: []string{outPath}}, nil
		}
		return Result{Err: err}, fmt.Errorf("filejoin: create output: %w", err)
	}

	bw := bufio.NewWriterSize(outFile, joinBufSize)

	cleanup := func() {
		_ = outFile.Close()     //nolint:errcheck // best-effort cleanup
		_ = root.Remove(outRel) //nolint:errcheck // best-effort cleanup
	}

	totalParts := len(archive.Parts)
	for i, part := range archive.Parts {
		// Honour context cancellation between parts.
		if err := ctx.Err(); err != nil {
			cleanup()
			return Result{Err: err}, fmt.Errorf("filejoin: cancelled: %w", err)
		}

		if err := copyPart(bw, part); err != nil {
			cleanup()
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
		cleanup()
		return Result{Err: err}, fmt.Errorf("filejoin: flush: %w", err)
	}

	if err := outFile.Close(); err != nil {
		_ = root.Remove(outRel) //nolint:errcheck // best-effort cleanup
		return Result{Err: err}, fmt.Errorf("filejoin: close output: %w", err)
	}

	log.Info("filejoin: join complete", "outPath", outPath, "parts", len(archive.Parts))
	return Result{ExtractedFiles: []string{outPath}}, nil
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

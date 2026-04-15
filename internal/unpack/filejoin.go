package unpack

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

const joinBufSize = 1024 * 1024 // 1 MiB write buffer

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
func FileJoin(ctx context.Context, archive Archive, outDir string, _ Options) (Result, error) {
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

	// Refuse to overwrite an existing file.
	if _, err := os.Stat(outPath); err == nil {
		return Result{Err: fmt.Errorf("filejoin: output file already exists: %s", outPath)},
			fmt.Errorf("filejoin: output file already exists: %s", outPath)
	}

	slog.Info("filejoin: starting join",
		"name", archive.Name,
		"parts", len(archive.Parts),
		"outPath", outPath,
	)

	outFile, err := os.Create(outPath) //nolint:gosec // outPath is constructed from caller-supplied outDir and archive.Name
	if err != nil {
		return Result{Err: err}, fmt.Errorf("filejoin: create output: %w", err)
	}

	bw := bufio.NewWriterSize(outFile, joinBufSize)

	cleanup := func() {
		_ = outFile.Close()    //nolint:errcheck // best-effort cleanup
		_ = os.Remove(outPath) //nolint:errcheck // best-effort cleanup
	}

	for _, part := range archive.Parts {
		// Honour context cancellation between parts.
		if err := ctx.Err(); err != nil {
			cleanup()
			return Result{Err: err}, fmt.Errorf("filejoin: cancelled: %w", err)
		}

		if err := copyPart(bw, part); err != nil {
			cleanup()
			return Result{Err: err}, fmt.Errorf("filejoin: copy %s: %w", part, err)
		}
	}

	if err := bw.Flush(); err != nil {
		cleanup()
		return Result{Err: err}, fmt.Errorf("filejoin: flush: %w", err)
	}

	if err := outFile.Close(); err != nil {
		_ = os.Remove(outPath) //nolint:errcheck // best-effort cleanup
		return Result{Err: err}, fmt.Errorf("filejoin: close output: %w", err)
	}

	slog.Info("filejoin: join complete", "outPath", outPath, "parts", len(archive.Parts))
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

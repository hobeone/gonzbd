package unpack

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
)

// DirectUnpackInput configures a single direct-unpack run.
//
// Direct unpack extracts a multi-volume RAR set concurrently with the
// download: volume 1 is extracted while volumes 2..N are still being
// assembled. When unrar reaches the end of a volume it prints a continue
// prompt and blocks on stdin; DirectUnpack waits for the next volume to
// appear on disk (via the supplied VolumeWaiter) before writing a reply.
type DirectUnpackInput struct {
	// SetName is the archive set name (e.g. "movie" for "movie.part01.rar").
	// Used purely for log output; may be empty.
	SetName string

	// FirstVolumePath is the absolute path of the first RAR volume.
	// It must already be fully assembled on disk before Run is called;
	// DirectUnpack starts unrar with this path as its input argument.
	FirstVolumePath string

	// TotalVolumes is the count of volumes in the set, learned from NZB
	// metadata at call time. DirectUnpack uses it to reject unexpected
	// prompts after the final volume has been processed.
	TotalVolumes int

	// OutDir is the target extraction directory. It must exist.
	OutDir string

	// Password, if non-empty, is passed to unrar as -p<pw>. An empty
	// password triggers -p- which disables unrar's interactive prompt
	// for an unexpectedly encrypted archive.
	Password string

	// OneFolder selects unrar's 'e' mode (flatten paths) instead of the
	// default 'x' mode (preserve archive directory structure).
	OneFolder bool

	// UnrarBin optionally overrides the unrar binary path. Empty means
	// "unrar" resolved via $PATH.
	UnrarBin string
}

// VolumeWaiter blocks until the (1-based) RAR volume with index volumeIdx
// is fully assembled on disk, then returns its absolute path.
//
// It must respect ctx cancellation. Returning a non-nil error signals to
// DirectUnpack that the volume will never arrive (e.g. the download was
// cancelled, or the assembler failed); DirectUnpack will abort and return
// the error so the caller can fall back to standard unpack.
type VolumeWaiter func(ctx context.Context, volumeIdx int) (string, error)

// DirectUnpackResult is the outcome of a direct-unpack run.
type DirectUnpackResult struct {
	// Success is true iff unrar printed "All OK" before exiting.
	Success bool

	// ExtractedFiles lists files unrar reported extracting, parsed from
	// "Extracting  <path>  OK" lines. Best-effort; paths are as unrar
	// printed them (may be relative to OutDir).
	ExtractedFiles []string

	// Output is the combined stdout+stderr log, newline-joined.
	Output string

	// ExitCode is unrar's exit status. Meaningful only after DirectUnpack
	// returns; zero on clean exit, non-zero on unrar failure, -1 when
	// unrar could not be reaped (e.g. killed by ctx cancellation).
	ExitCode int
}

// DirectUnpack extracts a multi-volume RAR set that is still being
// downloaded, calling wait to block until each required volume arrives
// on disk. It returns when unrar exits, a fatal error is detected, or
// ctx is cancelled.
func DirectUnpack(ctx context.Context, input DirectUnpackInput, wait VolumeWaiter) (DirectUnpackResult, error) {
	if wait == nil {
		return DirectUnpackResult{}, errors.New("direct unpack: wait is nil")
	}
	if input.FirstVolumePath == "" {
		return DirectUnpackResult{}, errors.New("direct unpack: FirstVolumePath is required")
	}
	if input.OutDir == "" {
		return DirectUnpackResult{}, errors.New("direct unpack: OutDir is required")
	}
	if input.TotalVolumes <= 0 {
		return DirectUnpackResult{}, errors.New("direct unpack: TotalVolumes must be positive")
	}

	bin := input.UnrarBin
	if bin == "" {
		bin = "unrar"
	}

	mode := "x"
	if input.OneFolder {
		mode = "e"
	}

	outDir := input.OutDir
	if !strings.HasSuffix(outDir, "/") && !strings.HasSuffix(outDir, "\\") {
		outDir += "/"
	}

	args := []string{mode, "-y", "-idp", "-o+"}
	if input.Password != "" {
		args = append(args, "-p"+input.Password)
	} else {
		args = append(args, "-p-")
	}
	args = append(args, input.FirstVolumePath, outDir)

	//nolint:gosec // G204: bin is either "unrar" or a caller-supplied override; args are structured
	cmd := exec.CommandContext(ctx, bin, args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return DirectUnpackResult{}, fmt.Errorf("direct unpack: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return DirectUnpackResult{}, fmt.Errorf("direct unpack: stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return DirectUnpackResult{}, fmt.Errorf("direct unpack: start %s: %w", bin, err)
	}

	result, loopErr := directUnpackLoop(ctx, stdout, stdin, input.TotalVolumes, wait)

	// Close stdin so unrar doesn't block if the loop exited while it was
	// waiting for input. Errors are expected (may already be closed by the
	// abort path) and unactionable.
	_ = stdin.Close() //nolint:errcheck // best-effort: stdin may already be closed

	// Drain any remaining stdout to EOF so cmd.Wait doesn't deadlock on a
	// non-empty pipe. Errors here are unactionable.
	_, _ = io.Copy(io.Discard, stdout) //nolint:errcheck // draining; errors unactionable

	waitErr := cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case errors.As(waitErr, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	case waitErr != nil:
		result.ExitCode = -1
	}

	if loopErr != nil {
		return result, loopErr
	}
	return result, nil
}

// Sentinel byte sequences for unrar's interactive prompts. These end in a
// trailing space without a newline, so detection has to be per-byte.
var (
	promptContinue = []byte("[C]ontinue, [Q]uit ")
	promptRetry    = []byte("[R]etry, [A]bort ")
)

// fatalErrorSubstrings match lines that indicate unrar cannot recover.
// Ported from sabnzbd/directunpacker.py run() — these are the exact
// strings the Python assembler treats as abort-triggering.
var fatalErrorSubstrings = []string{
	"ERROR: ",
	"Cannot create",
	"in the encrypted file",
	"CRC failed",
	"checksum failed",
	"not enough space on the disk",
	"password is incorrect",
	"Incorrect password",
	"Write error",
	"checksum error",
	"Cannot open",
	"start extraction from a previous volume",
	"Unexpected end of archive",
}

// extractedRe matches unrar's per-file extraction report. The tool prints
// "Extracting  <path>  OK" (or "Extracting from <archive>"; the latter is
// intentionally excluded by requiring the trailing "OK").
var extractedRe = regexp.MustCompile(`^Extracting\s+(.+?)\s+OK\s*$`)

// directUnpackLoop is the parsing core, factored out for direct testing.
// It reads bytes from stdout, handles unrar's prompt protocol by writing
// to stdin, and returns when the stream closes or a fatal condition is hit.
func directUnpackLoop(
	ctx context.Context,
	stdout io.Reader,
	stdin io.Writer,
	totalVolumes int,
	wait VolumeWaiter,
) (DirectUnpackResult, error) {
	br := bufio.NewReader(stdout)
	var linebuf []byte
	var logLines []string
	var extracted []string
	curVolume := 1
	result := DirectUnpackResult{}

	finalize := func() DirectUnpackResult {
		if len(linebuf) > 0 {
			logLines = append(logLines, strings.TrimRight(string(linebuf), "\r\n"))
		}
		result.ExtractedFiles = extracted
		result.Output = strings.Join(logLines, "\n")
		return result
	}

	for {
		b, err := br.ReadByte()
		if errors.Is(err, io.EOF) {
			return finalize(), nil
		}
		if err != nil {
			return finalize(), fmt.Errorf("direct unpack: read stdout: %w", err)
		}
		linebuf = append(linebuf, b)

		if b == '\n' {
			line := strings.TrimRight(string(linebuf), "\r\n")
			linebuf = linebuf[:0]
			if line == "" {
				continue
			}
			logLines = append(logLines, line)

			if containsAny(line, fatalErrorSubstrings) {
				_, _ = stdin.Write([]byte("Q\n")) //nolint:errcheck // best-effort abort
				return finalize(), fmt.Errorf("direct unpack: unrar fatal: %s", line)
			}
			if strings.HasPrefix(line, "All OK") {
				result.Success = true
				continue
			}
			if m := extractedRe.FindStringSubmatch(line); m != nil {
				extracted = append(extracted, m[1])
			}
			continue
		}

		// Prompt detection: the prompts have no newline, so check after
		// every byte that we're not at a line break.
		if bytes.HasSuffix(linebuf, promptContinue) {
			linebuf = linebuf[:0]
			needed := curVolume + 1
			if needed > totalVolumes {
				// Defensive: unrar shouldn't prompt after the last volume,
				// but if it does, tell it to quit.
				_, _ = stdin.Write([]byte("Q\n")) //nolint:errcheck // best-effort
				continue
			}
			if _, werr := wait(ctx, needed); werr != nil {
				_, _ = stdin.Write([]byte("Q\n")) //nolint:errcheck // best-effort
				return finalize(), fmt.Errorf("direct unpack: wait for volume %d: %w", needed, werr)
			}
			if _, werr := stdin.Write([]byte("C\n")); werr != nil {
				return finalize(), fmt.Errorf("direct unpack: write continue: %w", werr)
			}
			curVolume = needed
			continue
		}
		if bytes.HasSuffix(linebuf, promptRetry) {
			linebuf = linebuf[:0]
			_, _ = stdin.Write([]byte("A\n")) //nolint:errcheck // best-effort
			return finalize(), errors.New("direct unpack: unrar retry/abort prompt")
		}

		// Only check ctx occasionally; ReadByte will also observe pipe
		// closure when exec.CommandContext kills the process on cancel.
		if err := ctx.Err(); err != nil {
			return finalize(), fmt.Errorf("direct unpack: %w", err)
		}
	}
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

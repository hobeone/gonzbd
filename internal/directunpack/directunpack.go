package directunpack

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/hobeone/gonzbd/internal/cmdutil"
)

// SuccessSet records the outcome of a successfully extracted RAR set.
type SuccessSet struct {
	// RarParts lists absolute paths of the RAR volume files consumed.
	RarParts []string
	// ExtractedFiles lists absolute paths of files created by extraction.
	ExtractedFiles []string
}

// Options configures a DirectUnpacker instance.
type Options struct {
	// UnrarCommand overrides the unrar binary path. Empty defaults to "unrar".
	UnrarCommand string
	// Password is the archive password. Empty means no password.
	Password string
	// OneFolder extracts all files flat (unrar 'e' instead of 'x').
	OneFolder bool
	// OverwriteFiles allows extraction to clobber existing files.
	OverwriteFiles bool
	// IgnoreUnrarDates discards in-archive timestamps.
	IgnoreUnrarDates bool
	// ExtraArgs holds additional unrar flags.
	ExtraArgs []string
	// CmdCfg controls nice/ionice wrapping.
	CmdCfg cmdutil.CmdConfig
	// HasProblem is true when unrar is non-original or too old.
	HasProblem bool

	// OnLine is called for each line of unrar output. May be nil.
	OnLine func(string)
}

// maxDuplicatePrompts is the number of duplicate [C]ontinue prompts
// allowed before aborting. Prevents infinite loops on obfuscated names
// where volume ordering can't be determined.
const maxDuplicatePrompts = 10

// errorPatterns are strings in unrar output that indicate extraction failure.
// Matches SABnzbd's directunpacker.py error detection.
var errorPatterns = []string{
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

// DirectUnpacker manages an interactive unrar subprocess that extracts
// RAR archives as volumes complete during download. It is safe for
// concurrent use by the assembler (calling Add) and the app (calling
// Abort, Wait, Results).
type DirectUnpacker struct {
	log *slog.Logger
	mu  sync.Mutex

	jobID       string
	downloadDir string // where assembled RAR volumes appear
	extractDir  string // where unrar writes output

	// Volume tracking.
	killed        bool
	curSetname    string
	curVolume     int                       // 1-based volume currently being extracted
	totalVolumes  map[string]int            // setname → max volume number
	completedVols map[string]map[int]string // setname → vol → absolute filepath
	successSets   map[string]SuccessSet     // completed extractions
	nextSets      []string                  // archive sets waiting to start

	// All files in the job (for initial volume scan).
	allFilenames []string

	// Subprocess.
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	// Coordination.
	volumeReady chan struct{} // signaled when a new RAR volume finishes
	done        chan struct{} // closed when run() exits
	started     bool          // whether the goroutine has been started

	// Config.
	opts Options
}

// New constructs a DirectUnpacker for the given job.
func New(log *slog.Logger, jobID, downloadDir, extractDir string, opts Options) *DirectUnpacker {
	return &DirectUnpacker{
		log:           log,
		jobID:         jobID,
		downloadDir:   downloadDir,
		extractDir:    extractDir,
		totalVolumes:  make(map[string]int),
		completedVols: make(map[string]map[int]string),
		successSets:   make(map[string]SuccessSet),
		volumeReady:   make(chan struct{}, 1),
		done:          make(chan struct{}),
		opts:          opts,
	}
}

// SetAllFilenames provides the complete list of filenames in the job so
// that totalVolumes can be computed on the first Add() call.
func (d *DirectUnpacker) SetAllFilenames(names []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allFilenames = names
}

// Add is called when a RAR volume file has been fully assembled on disk.
// filename is the base name (e.g. "movie.part01.rar") and path is the
// absolute path to the file.
//
// On the first call, it scans all job filenames to build the volume map.
// If this is vol 1 of the first set, it starts the unrar subprocess.
func (d *DirectUnpacker) Add(ctx context.Context, filename, path string) {
	setname, vol := AnalyzeRarFilename(filename)
	if setname == "" {
		return // not a RAR volume
	}

	d.mu.Lock()

	if d.killed {
		d.mu.Unlock()
		return
	}

	// First call: build total volume counts from all job filenames.
	if len(d.totalVolumes) == 0 && len(d.allFilenames) > 0 {
		d.buildVolumeMap()
	}

	// Record this completed volume.
	if d.completedVols[setname] == nil {
		d.completedVols[setname] = make(map[int]string)
	}
	d.completedVols[setname][vol] = path

	d.log.Info("volume completed",
		"set", setname, "vol", vol, "path", filepath.Base(path),
		"total_for_set", d.totalVolumes[setname])

	needStart := false
	if !d.started {
		// First RAR volume: determine if we can start.
		if vol == 1 {
			d.curSetname = setname
			d.curVolume = 1
			d.started = true
			needStart = true
		} else {
			// Vol 1 hasn't arrived yet — queue the set for later.
			if !d.setQueued(setname) {
				d.nextSets = append(d.nextSets, setname)
			}
		}
	} else if setname != d.curSetname && !d.setQueued(setname) {
		// Different set than what we're currently extracting — queue it.
		d.nextSets = append(d.nextSets, setname)
	}

	d.mu.Unlock()

	if needStart {
		go d.run(ctx)
	}

	// Signal the reader goroutine that a new volume is available.
	select {
	case d.volumeReady <- struct{}{}:
	default:
	}
}

// setQueued returns true if setname is already in nextSets. Must be called
// with mu held.
func (d *DirectUnpacker) setQueued(setname string) bool {
	if slices.Contains(d.nextSets, setname) {
		return true
	}
	return setname == d.curSetname
}

// buildVolumeMap populates totalVolumes from allFilenames. Must be called
// with mu held.
func (d *DirectUnpacker) buildVolumeMap() {
	for _, name := range d.allFilenames {
		setname, vol := AnalyzeRarFilename(name)
		if setname == "" {
			continue
		}
		if vol > d.totalVolumes[setname] {
			d.totalVolumes[setname] = vol
		}
	}
	d.log.Info("volume map built", "sets", len(d.totalVolumes))
	for s, maxVol := range d.totalVolumes {
		d.log.Debug("volume map entry", "set", s, "max_vol", maxVol)
	}
}

// Abort stops the DirectUnpacker, kills the subprocess, and cleans up
// any extracted files. Safe to call multiple times and concurrently.
func (d *DirectUnpacker) Abort() {
	d.mu.Lock()
	if d.killed {
		d.mu.Unlock()
		return
	}
	d.killed = true
	d.log.Info("aborting directunpack", "job", d.jobID)

	// Kill the subprocess.
	if d.stdin != nil {
		// Try graceful quit first.
		_, _ = d.stdin.Write([]byte("Q\n"))
	}
	if d.cmd != nil && d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}

	// Clear results so fallback unpack runs.
	d.successSets = make(map[string]SuccessSet)
	d.nextSets = nil
	d.mu.Unlock()

	// Signal the reader goroutine to unblock.
	select {
	case d.volumeReady <- struct{}{}:
	default:
	}

	// Wait for the goroutine to exit.
	<-d.done
}

// Wait blocks until the DirectUnpacker goroutine has finished (either
// successfully or via abort). Safe to call when not started.
func (d *DirectUnpacker) Wait() {
	d.mu.Lock()
	started := d.started
	d.mu.Unlock()
	if !started {
		return
	}
	<-d.done
}

// Done returns a channel that is closed when the DirectUnpacker finishes.
func (d *DirectUnpacker) Done() <-chan struct{} {
	return d.done
}

// Results returns a copy of the successfully extracted sets. The caller
// should check this after Wait() returns. An empty map means no sets
// were successfully extracted (abort/error occurred).
func (d *DirectUnpacker) Results() map[string]SuccessSet {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]SuccessSet, len(d.successSets))
	for k, v := range d.successSets {
		out[k] = SuccessSet{
			RarParts:       append([]string(nil), v.RarParts...),
			ExtractedFiles: append([]string(nil), v.ExtractedFiles...),
		}
	}
	return out
}

// run is the main goroutine that manages the interactive unrar subprocess.
func (d *DirectUnpacker) run(ctx context.Context) {
	defer close(d.done)

	for {
		d.mu.Lock()
		if d.killed {
			d.mu.Unlock()
			return
		}

		setname := d.curSetname
		volPath := d.completedVols[setname][d.curVolume]
		d.mu.Unlock()

		if volPath == "" {
			// Vol 1 not on disk yet — wait.
			if err := d.waitForVolume(ctx, setname, 1); err != nil {
				return
			}
			d.mu.Lock()
			volPath = d.completedVols[setname][1]
			d.mu.Unlock()
		}

		d.log.Info("starting unrar for set", "set", setname, "volume", volPath)

		if err := d.createUnrarInstance(ctx, volPath); err != nil {
			d.log.Error("failed to create unrar instance", "err", err)
			d.mu.Lock()
			d.killed = true
			d.mu.Unlock()
			return
		}

		d.readUnrarOutput(ctx)

		// Check if we should start the next set.
		d.mu.Lock()
		if d.killed || len(d.nextSets) == 0 {
			d.mu.Unlock()
			return
		}

		// Pop next set.
		nextSet := d.nextSets[0]
		d.nextSets = d.nextSets[1:]
		d.curSetname = nextSet
		d.curVolume = 1
		d.mu.Unlock()

		d.log.Info("starting next archive set", "set", nextSet)
	}
}

// createUnrarInstance builds and starts the unrar subprocess.
func (d *DirectUnpacker) createUnrarInstance(ctx context.Context, rarFile string) error {
	bin := d.opts.UnrarCommand
	if bin == "" {
		bin = "unrar"
	}

	mode := "x"
	if d.opts.OneFolder {
		mode = "e"
	}

	pwFlag := "-p-" // suppress interactive prompt
	if d.opts.Password != "" {
		pwFlag = "-p" + d.opts.Password
	}

	args := []string{
		mode,
		"-vp",  // volume pause — prompts [C]ontinue, [Q]uit at each volume boundary
		"-idp", // disable progress display
	}

	if !d.opts.HasProblem {
		args = append(args, "-scf", "-ai")
	}

	args = append(args, pwFlag)

	if d.opts.OverwriteFiles {
		args = append(args, "-o+")
	} else {
		args = append(args, "-o+") // DirectUnpack always overwrites (matches SABnzbd)
	}

	if d.opts.IgnoreUnrarDates && !d.opts.HasProblem {
		args = append(args, "-tsm-")
	}

	args = append(args, d.opts.ExtraArgs...)
	args = append(args, rarFile, d.extractDir+"/")

	cmd := cmdutil.BuildCommand(ctx, d.opts.CmdCfg, bin, args...)
	cmd.Stderr = nil // we read stdout only

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}

	// Merge stderr into stdout so error messages appear in our reader.
	cmd.Stderr = cmd.Stdout

	d.log.Info("starting unrar",
		"binary", bin,
		"args", args,
	)

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("start unrar: %w", err)
	}

	d.mu.Lock()
	d.cmd = cmd
	d.stdin = stdin
	d.stdout = stdout
	d.mu.Unlock()

	return nil
}

// readUnrarOutput reads the subprocess stdout char-by-char, matching
// interactive prompts and error strings.
func (d *DirectUnpacker) readUnrarOutput(ctx context.Context) {
	buf := make([]byte, 1)
	var line []byte
	var duplicateCount int
	var lastPromptLine string

	// Collect extracted file paths and consumed rar parts for this set.
	var extractedFiles []string
	var rarParts []string

	d.mu.Lock()
	setname := d.curSetname
	d.mu.Unlock()

	for {
		n, err := d.stdout.Read(buf)
		if n == 0 || err != nil {
			break // EOF or error — subprocess exited
		}
		line = append(line, buf[0])

		lineStr := string(line)

		// Check for the volume-continue prompt (no trailing newline).
		if bytes.HasSuffix(line, []byte("[C]ontinue, [Q]uit ")) {
			// Duplicate prompt detection.
			if lineStr == lastPromptLine {
				duplicateCount++
				if duplicateCount >= maxDuplicatePrompts {
					d.log.Warn("too many duplicate prompts, aborting",
						"set", setname, "count", duplicateCount)
					_, _ = d.stdin.Write([]byte("Q\n"))
					d.mu.Lock()
					d.killed = true
					d.mu.Unlock()
					break
				}
			} else {
				duplicateCount = 0
				lastPromptLine = lineStr
			}

			if err := d.waitForNextVolume(ctx); err != nil {
				d.log.Info("wait for next volume cancelled", "err", err)
				_, _ = d.stdin.Write([]byte("Q\n"))
				d.mu.Lock()
				d.killed = true
				d.mu.Unlock()
				break
			}

			d.mu.Lock()
			d.curVolume++
			d.mu.Unlock()

			_, _ = d.stdin.Write([]byte("C\n"))
			line = line[:0]
			continue
		}

		// Check for retry/abort prompt.
		if bytes.HasSuffix(line, []byte("[R]etry, [A]bort ")) {
			d.log.Warn("unrar retry/abort prompt, aborting", "set", setname)
			_, _ = d.stdin.Write([]byte("A\n"))
			d.mu.Lock()
			d.killed = true
			d.mu.Unlock()
			break
		}

		// Process complete lines.
		if buf[0] == '\n' {
			trimmed := strings.TrimRight(string(line), "\r\n")

			// Check for error patterns.
			for _, pattern := range errorPatterns {
				if strings.Contains(trimmed, pattern) {
					d.log.Warn("unrar error detected", "set", setname, "line", trimmed)
					_, _ = d.stdin.Write([]byte("Q\n"))
					d.mu.Lock()
					d.killed = true
					d.mu.Unlock()
					goto done
				}
			}

			// Track "All OK" — set completed successfully.
			if strings.Contains(trimmed, "All OK") {
				d.log.Info("set extraction complete", "set", setname)

				// Collect all rar parts for this set.
				d.mu.Lock()
				for _, path := range d.completedVols[setname] {
					rarParts = append(rarParts, path)
				}
				d.successSets[setname] = SuccessSet{
					RarParts:       rarParts,
					ExtractedFiles: extractedFiles,
				}
				d.mu.Unlock()
				break
			}

			// Track extracted files (lines starting with "Extracting  " or "...         ").
			if after, ok := strings.CutPrefix(trimmed, "Extracting  "); ok {
				fname := after
				fname = strings.TrimSpace(fname)
				if fname != "" {
					extractedFiles = append(extractedFiles, filepath.Join(d.extractDir, fname))
				}
			}

			// Track "Extracting from" to know which rar file is active.
			if after, ok := strings.CutPrefix(trimmed, "Extracting from "); ok {
				rarFile := after
				rarFile = strings.TrimSpace(rarFile)
				if rarFile != "" {
					rarParts = append(rarParts, rarFile)
				}
			}

			// Forward line to callback.
			if d.opts.OnLine != nil {
				d.opts.OnLine(trimmed)
			}

			line = line[:0]
			continue
		}
	}
done:

	// Wait for the subprocess to exit.
	if d.cmd != nil {
		_ = d.cmd.Wait()
	}
}

// waitForNextVolume blocks until curVolume+1 is available or ctx is done.
func (d *DirectUnpacker) waitForNextVolume(ctx context.Context) error {
	d.mu.Lock()
	nextVol := d.curVolume + 1
	setname := d.curSetname
	d.mu.Unlock()

	return d.waitForVolume(ctx, setname, nextVol)
}

// waitForVolume blocks until the specified volume is in completedVols.
func (d *DirectUnpacker) waitForVolume(ctx context.Context, setname string, vol int) error {
	for {
		d.mu.Lock()
		if d.killed {
			d.mu.Unlock()
			return fmt.Errorf("killed")
		}
		if vols, ok := d.completedVols[setname]; ok {
			if _, found := vols[vol]; found {
				d.mu.Unlock()
				return nil
			}
		}
		d.mu.Unlock()

		select {
		case <-d.volumeReady:
			continue
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// unrarBin returns the configured unrar binary, defaulting to "unrar".
func (d *DirectUnpacker) unrarBin() string {
	if d.opts.UnrarCommand != "" {
		return d.opts.UnrarCommand
	}
	return "unrar"
}

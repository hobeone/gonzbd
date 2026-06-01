package directunpack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/hobeone/gonzbd/internal/unpack"
	"github.com/hobeone/rarengine"
)

// SuccessSet records the outcome of a successfully extracted RAR set.
type SuccessSet struct {
	// RarParts lists absolute paths of the RAR volume files consumed.
	RarParts []string
	// ExtractedFiles lists absolute paths of files created by extraction.
	ExtractedFiles []string
}

// FailedSet records a set that DirectUnpack attempted but failed to extract.
type FailedSet struct {
	// Reason is a human-readable description of why extraction failed.
	Reason string
}

// Options configures a DirectUnpacker instance.
type Options struct {
	// Password is the archive password. Empty means no password.
	Password string
	// OneFolder extracts all files flat (no directory structure).
	OneFolder bool
	// OverwriteFiles allows extraction to clobber existing files.
	OverwriteFiles bool
	// IgnoreUnrarDates discards in-archive timestamps.
	IgnoreUnrarDates bool

	// OnLine is called for each line of extraction output. May be nil.
	OnLine func(string)
}

// DirectUnpacker manages pure-Go RAR extraction as volumes complete during
// download. It uses rarengine with a blocking volume channel.
//
// It is safe for concurrent use by the assembler (calling Add) and the
// app (calling Abort, Wait, Results).
type DirectUnpacker struct {
	log *slog.Logger
	mu  sync.Mutex

	jobID       string
	downloadDir string // where assembled RAR volumes appear
	extractDir  string // where extraction writes output

	// Volume tracking.
	killed        bool
	curSetname    string
	totalVolumes  map[string]int            // setname → max volume number
	completedVols map[string]map[int]string // setname → vol → absolute filepath
	successSets   map[string]SuccessSet     // completed extractions
	failedSets    map[string]FailedSet      // sets that failed
	nextSets      []string                  // archive sets waiting to start

	// All files in the job (for initial volume scan).
	allFilenames []string

	// Active volumes coordination.
	activeSet string

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
		failedSets:    make(map[string]FailedSet),
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
// If this is vol 1 of the first set, it starts extraction.
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

// Abort stops the DirectUnpacker. Results are cleared.
// Safe to call multiple times and concurrently.
func (d *DirectUnpacker) Abort() {
	d.mu.Lock()
	if d.killed {
		d.mu.Unlock()
		return
	}
	d.killed = true
	d.log.Info("aborting directunpack", "job", d.jobID)

	// Record failures for the current and any queued sets.
	if d.curSetname != "" {
		d.recordFailure(d.curSetname, "aborted")
	}
	for _, s := range d.nextSets {
		d.recordFailure(s, "aborted (not started)")
	}

	// Clear results.
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
// were successfully extracted.
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

// Failures returns a copy of the failed sets. The caller should check
// this after Wait() returns alongside Results().
func (d *DirectUnpacker) Failures() map[string]FailedSet {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]FailedSet, len(d.failedSets))
	maps.Copy(out, d.failedSets)
	return out
}

// recordFailure adds a failure entry for the current set. Must be called
// with mu held.
func (d *DirectUnpacker) recordFailure(setname, reason string) {
	d.failedSets[setname] = FailedSet{Reason: reason}
}

// run is the main goroutine that manages extraction.
func (d *DirectUnpacker) run(ctx context.Context) {
	defer close(d.done)

	for {
		d.mu.Lock()
		if d.killed {
			d.mu.Unlock()
			return
		}
		setname := d.curSetname
		d.mu.Unlock()

		if err := d.extractSet(ctx, setname); err != nil {
			d.log.Error("extraction failed", "set", setname, "err", err)
			d.mu.Lock()
			if !d.killed {
				d.recordFailure(setname, err.Error())
			}
			d.mu.Unlock()
		}

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
		d.mu.Unlock()

		d.log.Info("starting next archive set", "set", nextSet)
	}
}

// extractSet extracts one complete RAR set using rarengine with sequential
// volume channel streaming. The deferred panic recovery catches rarengine
// panics and must remain in this function — it only protects the call stack
// of the goroutine that called extractSet, not the feeder goroutine.
func (d *DirectUnpacker) extractSet(ctx context.Context, setname string) (retErr error) {
	defer func() {
		if p := recover(); p != nil {
			retErr = fmt.Errorf("directunpack: rarengine panic: %v", p)
		}
	}()

	d.mu.Lock()
	maxVol := d.totalVolumes[setname]
	d.mu.Unlock()

	if maxVol == 0 {
		maxVol = 100 // fallback safe bound
	}

	volumesChan, feedErrChan := d.startVolumeFeed(ctx, setname, maxVol)

	sd := rarengine.NewStreamDecompressor(volumesChan)
	if d.opts.Password != "" {
		sd.SetPassword(d.opts.Password)
	}

	extractedFiles, err := d.extractEntries(ctx, sd)
	if err != nil {
		return err
	}

	select {
	case feedErr := <-feedErrChan:
		return feedErr
	default:
	}

	// Record success.
	d.mu.Lock()
	var rarParts []string
	for _, p := range d.completedVols[setname] {
		rarParts = append(rarParts, p)
	}
	d.successSets[setname] = SuccessSet{
		RarParts:       rarParts,
		ExtractedFiles: extractedFiles,
	}
	d.mu.Unlock()

	d.log.Info("set extraction complete", "set", setname,
		"parts", len(rarParts), "files", len(extractedFiles))

	return nil
}

// startVolumeFeed spawns a goroutine that opens each completed RAR volume in
// order and sends it on the returned channel. The goroutine closes volumesChan
// on completion or error. Any error is delivered on the returned buffered
// errChan; the caller must drain volumesChan to let the goroutine exit cleanly.
func (d *DirectUnpacker) startVolumeFeed(ctx context.Context, setname string, maxVol int) (<-chan io.ReadCloser, <-chan error) {
	volumesChan := make(chan io.ReadCloser)
	feedErrChan := make(chan error, 1)

	go func() {
		defer close(volumesChan)
		for vol := 1; vol <= maxVol; vol++ {
			if err := d.waitForVolume(ctx, setname, vol); err != nil {
				feedErrChan <- err
				return
			}
			d.mu.Lock()
			volPath := d.completedVols[setname][vol]
			d.mu.Unlock()

			f, err := os.Open(volPath)
			if err != nil {
				feedErrChan <- err
				return
			}

			select {
			case volumesChan <- f:
			case <-ctx.Done():
				_ = f.Close()
				feedErrChan <- ctx.Err()
				return
			}
		}
	}()

	return volumesChan, feedErrChan
}

// extractEntries drives the rarengine decompressor header-by-header, writing
// each archive entry to disk. Returns the list of extracted file paths on
// success, or the first error encountered.
func (d *DirectUnpacker) extractEntries(ctx context.Context, sd *rarengine.StreamDecompressor) ([]string, error) {
	var extractedFiles []string

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		d.mu.Lock()
		killed := d.killed
		d.mu.Unlock()
		if killed {
			return nil, fmt.Errorf("killed")
		}

		fh, err := sd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, rarengine.ErrNoNextVolume) {
				break
			}
			return nil, fmt.Errorf("directunpack: read header: %w", err)
		}

		destRel, sanitizeErr := unpack.SanitizeArchivePath(fh.Name, d.opts.OneFolder)
		if sanitizeErr != nil {
			d.log.Warn("directunpack: skipping entry with bad path", "raw_name", fh.Name, "err", sanitizeErr)
			_, _ = io.Copy(io.Discard, sd)
			continue
		}

		destPath := filepath.Join(d.extractDir, destRel)
		unpackOpts := unpack.Options{
			Password:         d.opts.Password,
			OneFolder:        d.opts.OneFolder,
			OverwriteFiles:   d.opts.OverwriteFiles,
			IgnoreUnrarDates: d.opts.IgnoreUnrarDates,
			OnLine:           d.opts.OnLine,
		}
		if err := unpack.ExtractEntryRarengine(ctx, d.extractDir, destPath, fh, sd, unpackOpts, d.log); err != nil {
			return nil, fmt.Errorf("directunpack: extract %s: %w", fh.Name, err)
		}

		if !fh.IsDir {
			extractedFiles = append(extractedFiles, destPath)
		}
		if d.opts.OnLine != nil {
			d.opts.OnLine("Extracting  " + fh.Name)
		}
	}

	return extractedFiles, nil
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

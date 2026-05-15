package directunpack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sync"

	rardecode "github.com/nwaples/rardecode/v2"

	"github.com/hobeone/gonzbd/internal/unpack"
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
// download. It uses rardecode with a blocking WaitFS so the library
// naturally pauses at volume boundaries until the next volume arrives.
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

	// WaitFS coordination: when extractSet() is running, activeWFS is
	// the WaitFS for the current set, and activeSet is the setname.
	// Add() forwards new volumes to activeWFS for live streaming.
	activeWFS *WaitFS
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

	// Forward to active WaitFS if this volume belongs to the current extraction set.
	wfs := d.activeWFS
	activeSet := d.activeSet

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

	// Forward to WaitFS outside the lock.
	if wfs != nil && setname == activeSet {
		wfs.AddVolume(filename, path)
	}

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

// Abort stops the DirectUnpacker. Results are cleared so the fallback
// unpack stage re-extracts everything.
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

	// Close the active WaitFS to unblock extraction.
	if d.activeWFS != nil {
		d.activeWFS.Close()
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

// run is the main goroutine that manages extraction using rardecode + WaitFS.
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

// extractSet extracts one complete RAR set using rardecode with WaitFS.
// It waits for volume 1 before starting, then rardecode's Open() calls
// block inside WaitFS until subsequent volumes arrive.
func (d *DirectUnpacker) extractSet(ctx context.Context, setname string) (retErr error) {
	// Top-level recover: rardecode panics on some malformed archives.
	defer func() {
		if p := recover(); p != nil {
			retErr = fmt.Errorf("directunpack: rardecode panic: %v", p)
		}
	}()

	// Wait for vol 1 before starting rardecode.
	if err := d.waitForVolume(ctx, setname, 1); err != nil {
		return fmt.Errorf("cancelled waiting for volume 1: %w", err)
	}

	d.mu.Lock()
	vol1 := d.completedVols[setname][1]
	d.mu.Unlock()

	// Build WaitFS: pre-populate with all already-completed volumes for this set.
	wfs := NewWaitFS(ctx)
	d.mu.Lock()
	for _, path := range d.completedVols[setname] {
		wfs.AddVolume(filepath.Base(path), path)
	}
	// Install as active WaitFS so Add() forwards new volumes.
	d.activeWFS = wfs
	d.activeSet = setname
	d.mu.Unlock()

	// Clear active WaitFS when done.
	defer func() {
		d.mu.Lock()
		d.activeWFS = nil
		d.activeSet = ""
		d.mu.Unlock()
		wfs.Close()
	}()

	d.log.Info("starting extraction for set", "set", setname, "volume", vol1)

	rdOpts := []rardecode.Option{
		rardecode.FileSystem(wfs),
		rardecode.MaxDictionarySize(512 << 20), // cap at 512 MiB
	}
	if d.opts.Password != "" {
		rdOpts = append(rdOpts, rardecode.Password(d.opts.Password))
	}

	r, err := unpack.SafeOpenReader(vol1, rdOpts...)
	if err != nil {
		return fmt.Errorf("directunpack: open set %q: %w", setname, err)
	}
	defer r.Close()

	var extractedFiles []string

	unpackOpts := unpack.Options{
		Password:         d.opts.Password,
		OneFolder:        d.opts.OneFolder,
		OverwriteFiles:   d.opts.OverwriteFiles,
		IgnoreUnrarDates: d.opts.IgnoreUnrarDates,
		OnLine:           d.opts.OnLine,
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		d.mu.Lock()
		killed := d.killed
		d.mu.Unlock()
		if killed {
			return fmt.Errorf("killed")
		}

		hdr, err := unpack.SafeNext(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("directunpack: read header: %w", err)
		}

		destRel, sanitizeErr := unpack.SanitizeArchivePath(hdr.Name, d.opts.OneFolder)
		if sanitizeErr != nil {
			d.log.Warn("directunpack: skipping entry with bad path",
				"raw_name", hdr.Name, "err", sanitizeErr)
			// Drain the reader for this entry to advance to the next.
			_, _ = io.Copy(io.Discard, r)
			continue
		}

		destPath := filepath.Join(d.extractDir, destRel)

		if err := unpack.ExtractEntry(ctx, d.extractDir, destPath, hdr, r, unpackOpts, d.log); err != nil {
			// Check for fs.ErrNotExist — this means WaitFS was closed
			// (abort/context cancel) and rardecode got an error trying
			// to read from the (now-closed) underlying file.
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("directunpack: volume not available (aborted?): %w", err)
			}
			return fmt.Errorf("directunpack: extract %s: %w", hdr.Name, err)
		}

		if !hdr.IsDir {
			extractedFiles = append(extractedFiles, destPath)
		}

		if d.opts.OnLine != nil {
			d.opts.OnLine("Extracting  " + hdr.Name)
		}
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

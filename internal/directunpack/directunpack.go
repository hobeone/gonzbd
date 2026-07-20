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

	"github.com/hobeone/rarengine"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/rarheader"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// errNotRAR is returned by extractSet when the first volume of a set isn't
// a RAR archive rarengine can read (RAR3 or RAR5). It is handled specially
// by run(): the set is recorded as skipped (not failed) and logged at Info
// level, since the normal unpack stage's external unrar handles other
// formats correctly.
var errNotRAR = errors.New("not a RAR3/RAR5 archive")

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

// SkippedSet records a set that DirectUnpack did not attempt because it
// isn't a RAR3 or RAR5 archive. rarengine (the pure-Go decompressor used by
// DirectUnpack) only supports RAR3/RAR5; other formats (e.g. legacy RAR2,
// or non-RAR archives misidentified by filename) are handled correctly by
// the normal unpack stage's external unrar fallback. This is expected, not
// an error.
type SkippedSet struct {
	// Reason is a human-readable description of why the set was skipped.
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

	// OnStatusChange is called when the unpacker's status changes.
	OnStatusChange func()
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
	skippedSets   map[string]SkippedSet     // sets skipped (not RAR5)
	corruptSets   map[string]string         // setname → reason; volumes assembled from a failed/incomplete download
	nextSets      []string                  // archive sets waiting to start

	// All files in the job (for initial volume scan).
	allFilenames []string

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
		skippedSets:   make(map[string]SkippedSet),
		corruptSets:   make(map[string]string),
		volumeReady:   make(chan struct{}, 1),
		done:          make(chan struct{}),
		opts:          opts,
	}
}

// Status represents the real-time state of a DirectUnpacker instance.
//
// WARNING: every exported field's json tag here is part of the public API
// response (internal/api serves it directly in queueSlot.DirectUnpack). Do
// not remove or rename a field without checking internal/api's
// field-contract test — see issue #96.
type Status struct {
	Active           bool              `json:"active"`
	CurrentSet       string            `json:"current_set,omitempty"`
	CompletedVolumes int               `json:"completed_volumes,omitempty"`
	TotalVolumes     int               `json:"total_volumes,omitempty"`
	SuccessSets      []string          `json:"success_sets,omitempty"`
	FailedSets       []string          `json:"failed_sets,omitempty"`
	FailedReasons    map[string]string `json:"failed_reasons,omitempty"`
}

// Status returns a thread-safe snapshot of the current direct unpack state.
func (d *DirectUnpacker) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()

	successNames := make([]string, 0, len(d.successSets))
	for k := range d.successSets {
		successNames = append(successNames, k)
	}
	slices.Sort(successNames)

	failedNames := make([]string, 0, len(d.failedSets))
	for k := range d.failedSets {
		failedNames = append(failedNames, k)
	}
	slices.Sort(failedNames)

	var active bool
	select {
	case <-d.done:
		active = false
	default:
		active = !d.killed && d.curSetname != ""
	}

	failedReasons := make(map[string]string)
	for k, v := range d.failedSets {
		failedReasons[k] = v.Reason
	}

	status := Status{
		Active:        active,
		SuccessSets:   successNames,
		FailedSets:    failedNames,
		FailedReasons: failedReasons,
	}

	if status.Active {
		status.CurrentSet = d.curSetname
		status.CompletedVolumes = len(d.completedVols[d.curSetname])
		status.TotalVolumes = d.totalVolumes[d.curSetname]
	}

	return status
}

func (d *DirectUnpacker) notifyChange() {
	if d.opts.OnStatusChange != nil {
		d.opts.OnStatusChange()
	}
}

// SetAllFilenames provides the complete list of filenames in the job so
// that totalVolumes can be computed on the first Add() call.
func (d *DirectUnpacker) SetAllFilenames(names []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allFilenames = names
}

// MarkCorrupt records that a volume belonging to setname was assembled from
// a download with missing or failed articles. The on-disk RAR file may still
// be structurally readable (rarengine can walk its entries), but its content
// is wrong since the source data never fully arrived. Once marked, the set
// can never be reported as successfully extracted: extraction either aborts
// early (if still waiting on volumes) or is downgraded to a failure after
// the fact (if extraction had already consumed the volume in question).
//
// The caller must call MarkCorrupt before the corresponding Add() for the
// affected volume, so the corrupt flag is visible before extraction can
// possibly consume that volume's data.
func (d *DirectUnpacker) MarkCorrupt(setname, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.corruptSets[setname] = reason

	// Wake any waitForVolume loop blocked on this set so it can re-check and
	// abort promptly instead of waiting for a volume that may never arrive
	// (or arrive much later) on the corrupt path.
	select {
	case d.volumeReady <- struct{}{}:
	default:
	}
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
		} else if !d.setQueued(setname) {
			// Vol 1 hasn't arrived yet — queue the set for later.
			d.nextSets = append(d.nextSets, setname)
		}
	} else if setname != d.curSetname && !d.setQueued(setname) {
		// Different set than what we're currently extracting — queue it.
		d.nextSets = append(d.nextSets, setname)
	}

	d.mu.Unlock()

	d.notifyChange()

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

	// If run() never started, it will never close d.done. Close it here so
	// Done()/Wait() observers unblock, and skip the wait below. run()'s
	// `defer close(d.done)` only fires when started==true (set under the lock
	// before `go d.run` in Add), so this close and that one are mutually
	// exclusive — d.done is never double-closed.
	started := d.started
	if !started {
		close(d.done)
	}
	d.mu.Unlock()

	if !started {
		return
	}

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

// Skipped returns a copy of the sets that were not attempted because they
// aren't RAR5. The caller should check this after Wait() returns alongside
// Results() and Failures().
func (d *DirectUnpacker) Skipped() map[string]SkippedSet {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]SkippedSet, len(d.skippedSets))
	maps.Copy(out, d.skippedSets)
	return out
}

// recordFailure adds a failure entry for the current set. Must be called
// with mu held.
func (d *DirectUnpacker) recordFailure(setname, reason string) {
	d.failedSets[setname] = FailedSet{Reason: reason}
}

// recordSkipped adds a skipped entry for the current set. Must be called
// with mu held.
func (d *DirectUnpacker) recordSkipped(setname, reason string) {
	d.skippedSets[setname] = SkippedSet{Reason: reason}
}

// run is the main goroutine that manages extraction.
func (d *DirectUnpacker) run(ctx context.Context) {
	defer func() {
		close(d.done)
		d.notifyChange()
	}()

	for {
		d.mu.Lock()
		if d.killed {
			d.mu.Unlock()
			return
		}
		setname := d.curSetname
		d.mu.Unlock()

		if err := d.extractSet(ctx, setname); err != nil {
			d.mu.Lock()
			killed := d.killed
			if !killed {
				if errors.Is(err, errNotRAR) {
					d.recordSkipped(setname, "not RAR3/RAR5; handled by normal unpack")
				} else {
					d.recordFailure(setname, err.Error())
				}
			}
			d.mu.Unlock()
			if !killed {
				if errors.Is(err, errNotRAR) {
					d.log.Info("skipping set, not a RAR3/RAR5 archive", "set", setname)
				} else {
					d.log.Error("extraction failed", "set", setname, "err", err)
				}
				d.notifyChange()
			}
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

		d.notifyChange()

		d.log.Info("starting next archive set", "set", nextSet)
	}
}

// extractSet extracts one complete RAR set using rarengine with sequential
// volume channel streaming. The deferred panic recovery catches rarengine
// panics and must remain in this function — it only protects the call stack
// of the goroutine that called extractSet, not the feeder goroutine.
func (d *DirectUnpacker) extractSet(ctx context.Context, setname string) error {
	return cmdutil.SafeEngineRun("directunpack: rarengine panic", func() error {
		// Derive a cancellable context so any early return from extractSet unblocks
		// the volume feeder goroutine (which only watches this ctx). Without this,
		// an early error from extractEntries leaks the feeder and its open *os.File
		// until the job-level context is cancelled.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		if err := d.waitForVolume(ctx, setname, 1); err != nil {
			return err
		}
		d.mu.Lock()
		maxVol := d.totalVolumes[setname]
		vol1Path := d.completedVols[setname][1]
		d.mu.Unlock()

		if maxVol == 0 {
			maxVol = 100 // fallback safe bound
		}

		// rarengine supports RAR3 and RAR5. Check the first volume's magic bytes
		// before streaming so other formats are skipped immediately rather than
		// failing on the first header read.
		ver, err := rarheader.Version(vol1Path)
		if err != nil {
			return errNotRAR
		}
		d.log.Info("starting extraction", "set", setname, "rar_version", ver)

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

		d.mu.Lock()
		// Backstop: a volume needed for this set may have been marked corrupt
		// (failed/missing download articles) after waitForVolume's own check —
		// e.g. all volumes had already arrived and extraction never went back
		// through waitForVolume again. Never record success once any volume of
		// this set is known to be assembled from incomplete data.
		if reason, corrupt := d.corruptSets[setname]; corrupt {
			d.mu.Unlock()
			return fmt.Errorf("directunpack: %s", reason)
		}

		// Record success.
		var rarParts []string
		for _, p := range d.completedVols[setname] {
			rarParts = append(rarParts, p)
		}
		d.successSets[setname] = SuccessSet{
			RarParts:       rarParts,
			ExtractedFiles: extractedFiles,
		}
		d.mu.Unlock()

		d.notifyChange()

		d.log.Info("set extraction complete", "set", setname,
			"parts", len(rarParts), "files", len(extractedFiles))

		return nil
	})
}

// startVolumeFeed spawns a goroutine that opens each completed RAR volume in
// order and sends it on the returned channel. The goroutine closes volumesChan
// on completion or error. Any error is delivered on the returned buffered
// errChan; the caller must drain volumesChan to let the goroutine exit cleanly.
func (d *DirectUnpacker) startVolumeFeed(ctx context.Context, setname string, maxVol int) (volumes <-chan io.ReadCloser, errs <-chan error) {
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

			f, err := os.Open(volPath) //nolint:gosec // volPath is a job-owned directory path set by AddVolume
			if err != nil {
				feedErrChan <- err
				return
			}

			d.log.Info("unpacking volume", "set", setname, "vol", vol, "path", filepath.Base(volPath))

			select {
			case volumesChan <- f:
			case <-ctx.Done():
				_ = f.Close() // best-effort cleanup on cancellation
				feedErrChan <- ctx.Err()
				return
			}
		}
	}()

	return volumesChan, feedErrChan
}

// extractEntries drives the rarengine decompressor header-by-header, writing
// each archive entry to disk. It opens an os.Root anchored at d.extractDir
// once for the entire session; all entry writes go through that rooted handle
// so they cannot escape d.extractDir via "..", an absolute path, or a
// symlinked path component. Returns the list of extracted file paths on
// success, or the first error encountered.
func (d *DirectUnpacker) extractEntries(ctx context.Context, sd *rarengine.StreamDecompressor) ([]string, error) {
	root, err := os.OpenRoot(d.extractDir)
	if err != nil {
		return nil, fmt.Errorf("directunpack: open root %s: %w", d.extractDir, err)
	}
	// Close after all entries are processed. Not deferred inside the loop to
	// avoid the deferInLoop lint finding; we close explicitly here.
	defer root.Close() //nolint:errcheck // read-only close after all writes are complete

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

		sp, sanitizeErr := unpack.NewSanitizedPath(fh.Name, d.opts.OneFolder)
		if sanitizeErr != nil {
			d.log.Warn("directunpack: skipping entry with bad path", "raw_name", fh.Name, "err", sanitizeErr)
			_, _ = io.Copy(io.Discard, sd) // drain stream to skip bad entry
			continue
		}

		destRel := sp.Rel()
		destPath := sp.Abs(d.extractDir)
		unpackOpts := unpack.Options{
			OneFolder:        d.opts.OneFolder,
			OverwriteFiles:   d.opts.OverwriteFiles,
			IgnoreUnrarDates: d.opts.IgnoreUnrarDates,
			OnLine:           d.opts.OnLine,
		}
		if err := unpack.ExtractEntryRarengine(ctx, root, d.extractDir, destRel, destPath, fh, sd, unpackOpts, d.log); err != nil {
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
		if reason, corrupt := d.corruptSets[setname]; corrupt {
			d.mu.Unlock()
			return fmt.Errorf("directunpack: %s", reason)
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

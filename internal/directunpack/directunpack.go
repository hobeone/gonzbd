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
	"runtime"
	"slices"
	"sync"

	"github.com/hobeone/rarengine"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/rarheader"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// errNotRAR is returned by extractSet when the first volume of a set isn't
// a RAR archive rarengine can read (RAR5 only). It is handled specially
// by run(): the set is recorded as skipped (not failed) and logged at Info
// level, and the set is left to the post-processing unpack stage. That stage
// can extract RAR3 only through an external unrar binary it finds; its
// pure-Go path is rarengine too.
var errNotRAR = errors.New("not a RAR5 archive")

// errNotStarted is the outcome Abort settles for a set still queued: one that
// never became current, so nothing of it was extracted.
var errNotStarted = errors.New("aborted before the set started")

// volumeMissing is the error a set fails with when the input has ended (Wait
// or Abort) without its volume vol being added: waitForVolume's, for a set
// run took, and Wait's for each set queued on an unpacker whose run never
// started, which is missing volume 1. Its text is the set's failure reason.
func volumeMissing(setname string, vol int) error {
	return fmt.Errorf("volume %d of set %s never arrived", vol, setname)
}

// SuccessSet records the outcome of a successfully extracted RAR set.
type SuccessSet struct {
	// RarParts lists absolute paths of the RAR volume files consumed.
	RarParts []string
	// ExtractedFiles lists absolute paths of the files the set published into
	// the extraction directory.
	ExtractedFiles []string
}

// FailedSet records a set that DirectUnpack attempted but failed to extract.
type FailedSet struct {
	// Reason is a human-readable description of why extraction failed.
	Reason string
	// Kind classifies the failure as post-processing's extractors classify
	// theirs: unpack.FailDiskFull for a set refused for lack of space or one
	// that filled the disk (unpack.IsDiskFull), otherwise unpack.FailUnknown.
	Kind unpack.FailReason
}

// SkippedSet records a set that DirectUnpack did not attempt because it
// isn't a RAR5 archive. rarengine (the pure-Go decompressor used by
// DirectUnpack) only supports RAR5; other formats (e.g. RAR3, legacy RAR2,
// or non-RAR archives misidentified by filename) are left to the
// post-processing unpack stage, which extracts RAR3 only when it finds an
// external unrar binary. This is expected, not an error.
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
	// DecodeWorkers is the configured rar_decode_workers value (see
	// unpack.Options.DecodeWorkers). <= 0 means auto.
	DecodeWorkers int
	// MinFreeSpace is downloads.min_free_space: the bytes a set's extraction
	// must leave free on the extract dir's filesystem
	// (unpack.CheckRarVolumesFreeSpace).
	MinFreeSpace int64

	// OnLine is called for each line of extraction output. May be nil.
	OnLine func(string)

	// OnStatusChange is called when the unpacker's status changes.
	OnStatusChange func()
}

// DirectUnpacker manages pure-Go RAR extraction as volumes complete during
// download. It uses rarengine with a blocking volume channel.
//
// It is safe for concurrent use by the app's file-completion path (calling
// MarkCorrupt and Add, through directUnpackOrchestrator.maybeStart) and the
// rest of the app (calling Abort, Wait, Results).
type DirectUnpacker struct {
	log *slog.Logger
	mu  sync.Mutex

	jobID       string
	downloadDir string // where assembled RAR volumes appear
	extractDir  string // where a set's staging directory is made and its members published

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
	queued      chan struct{} // signaled when a set is queued or input ends; awaitNextSet waits on it
	done        chan struct{} // closed when run exits, or by Wait or Abort when run never started
	started     bool          // whether the goroutine has been started
	inputEnded  bool          // Wait or Abort was called: no more volumes will be added
	finished    bool          // done is closed or run is exiting: Add admits nothing more
	idle        bool          // awaitNextSet is waiting on queued with nothing queued

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
		queued:        make(chan struct{}, 1),
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
//
// A volume added once the unpacker has finished (Wait or Abort has returned,
// or run is exiting) is refused: no run is left to extract its set.
func (d *DirectUnpacker) Add(ctx context.Context, filename, path string) {
	setname, vol := AnalyzeRarFilename(filename)
	if setname == "" {
		return // not a RAR volume
	}

	d.mu.Lock()

	if d.killed || d.finished {
		late := !d.killed
		d.mu.Unlock()
		if late {
			d.log.Warn("directunpack: volume added after the unpacker finished; left to post-processing",
				"set", setname, "vol", vol)
		}
		return
	}

	// First call: build total volume counts from all job filenames.
	var builtVolumes map[string]int
	if len(d.totalVolumes) == 0 && len(d.allFilenames) > 0 {
		d.buildVolumeMap()
		builtVolumes = make(map[string]int, len(d.totalVolumes))
		maps.Copy(builtVolumes, d.totalVolumes)
	}

	// Record this completed volume.
	if d.completedVols[setname] == nil {
		d.completedVols[setname] = make(map[int]string)
	}
	d.completedVols[setname][vol] = path
	totalForSet := d.totalVolumes[setname]

	needStart, needWake := false, false
	switch {
	case !d.started && vol == 1:
		// The first set to reach volume 1 starts extraction, leaving the
		// queue if a later volume of it arrived first.
		d.makeCurrentLocked(setname)
		d.started = true
		needStart = true
	case !d.setAdmitted(setname):
		d.nextSets = append(d.nextSets, setname)
		needWake = true
	}

	d.mu.Unlock()
	// --- No lock held below this line ---
	if builtVolumes != nil {
		d.log.Info("volume map built", "sets", len(builtVolumes))
		for s, maxVol := range builtVolumes {
			d.log.Debug("volume map entry", "set", s, "max_vol", maxVol)
		}
	}
	d.log.Info("volume completed",
		"set", setname, "vol", vol, "path", filepath.Base(path),
		"total_for_set", totalForSet)

	d.notifyChange()

	if needStart {
		go d.run(ctx)
	}
	if needWake {
		signal(d.queued)
	}

	// Signal the reader goroutine that a new volume is available.
	signal(d.volumeReady)
}

// signal leaves one pending wake-up on ch, a channel with a buffer of one,
// without blocking.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// setAdmitted reports whether setname has already entered the queue: it is in
// nextSets, is curSetname, or has an outcome. A set is admitted once, so it is
// extracted at most once and gets at most one outcome. Must be called with mu
// held.
func (d *DirectUnpacker) setAdmitted(setname string) bool {
	if slices.Contains(d.nextSets, setname) || setname == d.curSetname {
		return true
	}
	_, succeeded := d.successSets[setname]
	_, failed := d.failedSets[setname]
	_, skipped := d.skippedSets[setname]
	return succeeded || failed || skipped
}

// makeCurrentLocked moves setname from the queue to curSetname: the transition
// for both the first set, which Add starts, and every later one, which run
// pops. Must be called with mu held.
func (d *DirectUnpacker) makeCurrentLocked(setname string) {
	d.nextSets = slices.DeleteFunc(d.nextSets, func(s string) bool { return s == setname })
	d.curSetname = setname
}

// buildVolumeMap populates totalVolumes from allFilenames. Must be called
// with mu held. Pure mutation only — no logging, so callers stay free to log
// a summary after releasing mu (see Add).
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
}

// Abort stops the DirectUnpacker and returns once run has exited. Each queued
// set is recorded as failed, "aborted (not started)". The set being extracted
// gets its outcome from run, through settleLocked: a success if it was
// published before run saw the abort, otherwise an "aborted" failure. Sets
// that finished before Abort keep their outcomes, so a set Results reports was
// published into the extract dir, whether or not Abort was called.
// Safe to call multiple times and concurrently.
func (d *DirectUnpacker) Abort() {
	d.mu.Lock()
	if d.killed {
		d.mu.Unlock()
		return
	}
	d.killed = true
	d.inputEnded = true

	for _, s := range d.nextSets {
		d.settleLocked(s, SuccessSet{}, errNotStarted)
	}
	d.nextSets = nil

	started := d.started
	if !started {
		d.finishUnstartedLocked()
	}
	d.mu.Unlock()
	// --- No lock held below this line ---
	d.log.Info("aborting directunpack", "job", d.jobID)

	if !started {
		return
	}

	// Wake run, whether it is waiting for a volume or for a set.
	signal(d.volumeReady)
	signal(d.queued)

	// Wait for the goroutine to exit.
	<-d.done
}

// Wait declares that every volume has been added, and blocks until each set
// the unpacker admitted has an outcome and run has exited. A volume added
// once the unpacker has finished is refused (Add). A set missing any volume,
// volume 1 or a later one, when Wait is called is recorded as failed, "volume
// N of set S never arrived": waitForVolume returns that for a volume not yet
// added once the input has ended, so Wait does not wait for a volume.
//
// When run never started, no set's volume 1 was added, so each queued set is
// recorded as failed here, and Wait returns at once.
func (d *DirectUnpacker) Wait() {
	d.mu.Lock()
	d.inputEnded = true
	if !d.started {
		for _, s := range d.nextSets {
			d.settleLocked(s, SuccessSet{}, volumeMissing(s, 1))
		}
		d.nextSets = nil
		d.finishUnstartedLocked()
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	// Wake run waiting for a set or for volume 1, or the feeder waiting for
	// a later volume.
	signal(d.queued)
	signal(d.volumeReady)
	<-d.done
}

// finishUnstartedLocked closes done for an unpacker whose run never started,
// which would otherwise never be closed, and sets finished so Add starts no
// run to close it again. run is started where started is set, in Add, under
// mu and after Add's finished check (`git grep -n 'd[.]started = true' --
// internal/directunpack/directunpack.go` returns 1 line), so the two closes
// are exclusive. Must be called with mu held and started false.
func (d *DirectUnpacker) finishUnstartedLocked() {
	if !d.finished {
		d.finished = true
		close(d.done)
	}
}

// Done returns a channel that is closed when run exits, or by Wait or Abort
// when run was never started. An unpacker that is never started, waited for
// or aborted never closes it.
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

// CorruptSets returns a copy of the sets marked corrupt and the reason for
// each. A set is marked when a volume completed with permanently failed
// articles, so its on-disk bytes have gaps; extraction consults this before
// trusting a volume. Mirrors Results/Failures/Skipped.
func (d *DirectUnpacker) CorruptSets() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.corruptSets)
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

// settleLocked records setname's outcome from what extracting it returned: a
// success when err is nil, a skip for errNotRAR, and otherwise a failure, whose
// reason is "aborted" once Abort has set killed. It is the one writer of
// successSets, failedSets and skippedSets after New makes them — `git grep -nE
// '(successSets|failedSets|skippedSets)\[[a-z]+\] =' -- internal/directunpack/directunpack.go`
// returns 5 lines, all in this function. A published set is a success
// whether or not killed is set, since its members are in the extract dir. Must
// be called with mu held.
func (d *DirectUnpacker) settleLocked(setname string, res SuccessSet, err error) {
	switch {
	case err == nil:
		d.successSets[setname] = res
	case errors.Is(err, errNotRAR):
		d.skippedSets[setname] = SkippedSet{Reason: "not RAR5; handled by normal unpack"}
	case errors.Is(err, errNotStarted):
		d.failedSets[setname] = FailedSet{Reason: "aborted (not started)"}
	case d.killed:
		d.failedSets[setname] = FailedSet{Reason: "aborted"}
	default:
		kind := unpack.FailUnknown
		if unpack.IsDiskFull(err) {
			kind = unpack.FailDiskFull
		}
		d.failedSets[setname] = FailedSet{Reason: err.Error(), Kind: kind}
	}
}

// run is the main goroutine that manages extraction.
func (d *DirectUnpacker) run(ctx context.Context) {
	defer func() {
		close(d.done)
		d.notifyChange()
	}()

	for {
		// No killed check here: the current set is owed an outcome even when
		// Abort lands before its extraction starts, and extractSet returns
		// at once when killed (waitForVolume).
		d.mu.Lock()
		setname := d.curSetname
		d.mu.Unlock()

		res, err := d.extractSet(ctx, setname)
		d.mu.Lock()
		d.settleLocked(setname, res, err)
		d.curSetname = ""
		killed := d.killed
		d.mu.Unlock()
		switch {
		case err == nil:
			d.log.Info("set extraction complete", "set", setname,
				"parts", len(res.RarParts), "files", len(res.ExtractedFiles))
		case errors.Is(err, errNotRAR):
			d.log.Info("skipping set, not a RAR5 archive", "set", setname)
		case killed:
			d.log.Info("set extraction aborted", "set", setname, "err", err)
		default:
			d.log.Error("extraction failed", "set", setname, "err", err)
		}
		d.notifyChange()

		nextSet, ok := d.awaitNextSet(ctx)
		if !ok {
			return
		}
		d.notifyChange()

		d.log.Info("starting next archive set", "set", nextSet)
	}
}

// awaitNextSet makes the next queued set current and returns it, waiting
// while the queue is empty and more volumes may still be added. It returns
// false when run is to exit: the unpacker was aborted (Abort settled the
// queue), or the queue is empty and input has ended or ctx is done. It sets
// finished under the lock it decided under, so Add queues no set after the
// decision that run would never take.
func (d *DirectUnpacker) awaitNextSet(ctx context.Context) (string, bool) {
	for {
		d.mu.Lock()
		d.idle = false
		switch {
		case d.killed:
			d.finished = true
			d.mu.Unlock()
			return "", false
		case len(d.nextSets) > 0:
			next := d.nextSets[0]
			d.makeCurrentLocked(next)
			d.mu.Unlock()
			return next, true
		case d.inputEnded || ctx.Err() != nil:
			d.finished = true
			d.mu.Unlock()
			return "", false
		}
		d.idle = true
		d.mu.Unlock()

		select {
		case <-d.queued:
		case <-ctx.Done():
		}
	}
}

// extractSet extracts one complete RAR set using rarengine with sequential
// volume channel streaming, and returns what it published. It records no
// outcome; run passes its result to settleLocked. The cmdutil.SafeEngineRun
// wrapper recovers rarengine panics and must remain in this function — it only
// protects the call stack of the goroutine that called extractSet, not the
// feeder goroutine.
func (d *DirectUnpacker) extractSet(ctx context.Context, setname string) (res SuccessSet, err error) {
	err = cmdutil.SafeEngineRun("directunpack: rarengine panic", func() error {
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

		// rarengine only supports RAR5 archives. Check the first volume's magic
		// bytes before streaming so non-RAR5 formats (including RAR3) are
		// skipped immediately and left to stage_unpack.
		ver, err := rarheader.Version(vol1Path)
		if err != nil || ver != 5 {
			return errNotRAR
		}
		d.log.Info("starting extraction", "set", setname, "rar_version", ver)

		// The free-space bound post-processing applies to an archive, over
		// the volumes on disk now: later volumes may still be downloading.
		if err := unpack.CheckRarVolumesFreeSpace(ctx, d.log, setname, d.leadingVolumes(setname),
			d.extractDir, d.opts.MinFreeSpace); err != nil {
			return err
		}

		volumesChan, feedErrChan := d.startVolumeFeed(ctx, setname, maxVol)

		r := rarengine.NewReader(volumesChan)
		r.SetWorkers(unpack.DecodeWorkers(d.opts.DecodeWorkers, runtime.NumCPU()))
		defer r.Close() //nolint:errcheck // close open and queued volumes
		stop := context.AfterFunc(ctx, func() { _ = r.Close() })
		defer stop()

		if d.opts.Password != "" {
			r.SetPasswords([]string{d.opts.Password})
		}

		// The set is extracted into a staging directory inside extractDir, and
		// its members reach extractDir only if every check in this callback
		// passes; a set that fails or is aborted inside it leaves nothing there.
		// A publish that fails part way leaves what it already renamed
		// (unpack.ExtractStaged).
		published, err := unpack.ExtractStaged(d.log, d.extractDir, d.unpackOptions(), func(stageDir string) error {
			extractErr := d.extractEntries(ctx, r, stageDir)
			// A feed error is preferred over the reader's: the feeder sends
			// it before it closes the volume stream, so a reader that failed
			// on the stream ending early failed because of it.
			select {
			case feedErr := <-feedErrChan:
				return feedErr
			default:
			}
			if extractErr != nil {
				return extractErr
			}

			// Backstop: a volume needed for this set may have been marked
			// corrupt (failed/missing download articles) after waitForVolume's
			// own check — e.g. all volumes had already arrived and extraction
			// never went back through waitForVolume again. Nothing of a set is
			// published once any of its volumes is known to be assembled from
			// incomplete data.
			d.mu.Lock()
			defer d.mu.Unlock()
			if reason, corrupt := d.corruptSets[setname]; corrupt {
				return fmt.Errorf("directunpack: %s", reason)
			}
			return nil
		})
		if err != nil {
			return err
		}
		extractedFiles := make([]string, 0, len(published))
		for _, rel := range published {
			extractedFiles = append(extractedFiles, filepath.Join(d.extractDir, rel))
		}

		d.mu.Lock()
		var rarParts []string
		for _, p := range d.completedVols[setname] {
			rarParts = append(rarParts, p)
		}
		d.mu.Unlock()
		res = SuccessSet{RarParts: rarParts, ExtractedFiles: extractedFiles}
		return nil
	})
	return res, err
}

// leadingVolumes returns the paths of setname's volumes added so far that
// form an unbroken run from volume 1, in volume order.
func (d *DirectUnpacker) leadingVolumes(setname string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var vols []string
	for vol := 1; ; vol++ {
		p, ok := d.completedVols[setname][vol]
		if !ok {
			return vols
		}
		vols = append(vols, p)
	}
}

// startVolumeFeed spawns a goroutine that opens each completed RAR volume in
// order and sends it on the returned channel. The goroutine closes volumesChan
// on completion or error. Any error is delivered on the returned buffered
// errChan. The goroutine exits after sending the last volume, on an error, or
// when ctx is cancelled; extractSet cancels ctx on every return.
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

			f, err := os.Open(volPath) //nolint:gosec // volPath is a job-owned directory path set by Add
			if err != nil {
				feedErrChan <- err
				return
			}

			d.log.Info("unpacking volume", "set", setname, "vol", vol, "path", filepath.Base(volPath))

			select {
			case volumesChan <- f:
			case <-ctx.Done():
				_ = f.Close() //nolint:errcheck // a volume never handed to the reader; the cancellation is what is reported
				feedErrChan <- ctx.Err()
				return
			}
		}
	}()

	return volumesChan, feedErrChan
}

// extractEntries drives the rarengine Reader entry-by-entry, writing each
// archive entry into stageDir. It opens an os.Root anchored at stageDir once
// for the entire session; all entry writes go through that rooted handle so
// they cannot escape stageDir via "..", an absolute path, or a symlinked path
// component. It returns the first error encountered.
func (d *DirectUnpacker) extractEntries(ctx context.Context, r *rarengine.Reader, stageDir string) error {
	root, err := os.OpenRoot(stageDir)
	if err != nil {
		return fmt.Errorf("directunpack: open root %s: %w", stageDir, err)
	}
	// Close after all entries are processed. Not deferred inside the loop to
	// avoid the deferInLoop lint finding; we close explicitly here.
	defer root.Close() //nolint:errcheck // read-only close after all writes are complete

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

		entry, err := r.NextEntry()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, rarengine.ErrNoNextVolume) {
				break
			}
			if errors.Is(err, rarengine.ErrReaderClosed) && ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("directunpack: read header: %w", err)
		}

		if err := d.extractMember(ctx, root, entry.Header, entry); err != nil {
			_ = r.Close()     //nolint:errcheck // read-side archive reader; the extraction error is the one returned
			_ = entry.Close() //nolint:errcheck // read-side archive member; the extraction error is the one returned
			if ctx.Err() != nil {
				return ctx.Err()
			}
			unpack.NoteDictionaryLimit(d.log, err, entry.Header)
			return err
		}
	}

	// The same post-extraction gate post-processing applies to its own
	// results: nothing under the job directory, the stage included, may
	// resolve outside it. DirectUnpack's results are otherwise accepted as
	// they stand.
	if cErr := fsutil.CheckContainment(d.extractDir); cErr != nil {
		return fmt.Errorf("directunpack: containment check: %w", cErr)
	}

	return nil
}

// unpackOptions is this unpacker's options in the form unpack.ExtractMember
// and unpack.ExtractStaged take.
func (d *DirectUnpacker) unpackOptions() unpack.Options {
	return unpack.Options{
		OneFolder:        d.opts.OneFolder,
		OverwriteFiles:   d.opts.OverwriteFiles,
		IgnoreUnrarDates: d.opts.IgnoreUnrarDates,
		DecodeWorkers:    d.opts.DecodeWorkers,
		OnLine:           d.opts.OnLine,
	}
}

// extractMember extracts one member into root's directory through
// unpack.ExtractMember, the member step of unpack's in-process RAR extractor,
// with this unpacker's options. On an error the entry may still be open; the
// caller closes it.
func (d *DirectUnpacker) extractMember(ctx context.Context, root *os.Root, fh *rarengine.FileHeader, entry io.ReadCloser) error {
	if _, _, err := unpack.ExtractMember(ctx, root, root.Name(), fh, entry, d.unpackOptions(), d.log); err != nil {
		return fmt.Errorf("directunpack: extract %s: %w", fh.Name, err)
	}
	return nil
}

// waitForVolume blocks until the specified volume is in completedVols, and
// returns an error instead once the unpacker is killed, the set is marked
// corrupt, the input has ended without the volume (volumeMissing: no volume
// is added after Wait or Abort), or ctx is done.
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
		if d.inputEnded {
			d.mu.Unlock()
			return volumeMissing(setname, vol)
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

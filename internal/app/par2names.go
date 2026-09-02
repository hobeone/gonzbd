package app

import (
	"errors"
	"log/slog"
	"path"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/queue"
)

// resolvedName is the name a file currently has on disk, as the rest of the
// system understands it: the manifest's subject unless a resolved filename has
// been recorded, which is what both par2 call sites already prefer.
func resolvedName(m *queue.Manifest, p *queue.JobProgress, fi int) string {
	name := m.FileSubject(fi)
	if fn := p.FileFilename(fi); fn != "" {
		name = fn
	}
	return name
}

// resolvedNameFor reduces a rename target to the form JobProgress.Filename can
// actually hold.
//
// The field cannot hold a path. pipeline.go's registerFile feeds it back
// through fsutil.JoinSafe on a resume or retry, and fsutil.SanitizeFilename
// lists "/" and "\" among its illegal characters and rewrites them to "_", so
// storing "Screens/shot.jpg" would send a later retry to write
// "Screens_shot.jpg" while the file sits at "Screens/shot.jpg".
//
// The basename is stored instead of nothing. Skipping the record entirely was
// worse than it looked: the field kept the OBFUSCATED name, which no longer
// names anything on disk either, and every later reader matched against it.
// par2.VerifyCRCs indexes the par2 manifest by basename (verifycrc.go builds
// "basename → entry"), so "shot.jpg" matches the entry "Screens/shot.jpg"
// exactly, while the obfuscated string matched nothing and was counted
// Unverified — marking a healthy job damaged and forcing a par2 repair over it.
//
// The two resume readers both do jobFilePath(job, name) and stat the result
// (resume_startup.go), so they look for "shot.jpg" at the top level and do not
// find it. That is not a regression: they did not find the obfuscated name
// either, since relocation had already moved it. Both cases fall through to a
// re-download, which quickcheck then relocates again.
//
// One notion of "separator", used both to detect a path and to reduce it:
// "/", which is what the par2 format specifies and what relocateFile splits
// on via filepath.FromSlash. A backslash is NOT treated as one, and that is
// deliberate rather than an oversight — on the platforms this runs on,
// "Screens\shot.jpg" is a single legal filename, and rewriting it to a
// separator would turn a poster-controlled string into a directory component.
// An earlier form tested for both characters but only stripped one, so a
// backslash name was refused a record and then not reduced either.
func resolvedNameFor(to string) string {
	return path.Base(filepath.ToSlash(to))
}

// applyPar2Names identifies the delivered files against the par2 index and,
// where identification says a file is not at the path par2 records, renames it
// and updates the resolved name that records where it is.
//
// # What this function owns, and what it does not
//
// It pairs the on-disk rename with the JobProgress.Filename update so that
// neither can happen alone ON THIS PATH. That is narrower than "the owner of
// a file moved", which an earlier draft of this comment claimed, and the
// difference is load-bearing rather than pedantic.
//
// `git grep -n 'queue\.SetFileFilename(' -- '*.go' ':!*_test.go' ':!*.spec'`
// returns 2 lines: pipeline.go's registerFile and this function. (The pattern
// is anchored on the receiver, and escaped, so that it cannot match this
// sentence — a citation that counts itself reads as one higher than the
// population it describes.) So the field has two writers, not one:
// registerFile establishes it at first write and this corrects it. They are
// disjoint in time, not in scope.
//
// More importantly, this is NOT the only code that renames a par2-described
// file on disk. postproc's stage_quickcheck calls the same
// par2.QuickCheckWithOptions and moves files itself, with no queue handle to
// record anything through (postproc.Job carries a *queue.Job snapshot, not a
// *queue.Queue). It compensates in-memory instead — see the rename remap in
// its verifyJobCRCs — rather than by writing the field. Any claim here that
// on-disk state and the recorded name "cannot drift" would be false for that
// path, and stating it would send the next reader looking for a guarantee
// nothing provides.
//
// The manifest is NOT touched. It records what the NZB said, which is still
// true and is what a retry re-derives from; JobProgress.Filename records what
// is on disk, which is what changed.
//
// Running before post-processing is what makes the rename cheap: the
// quickcheck stage prefers JobProgress.Filename, so it sees corrected names
// with no change to it, and snapshotOwnedFiles seeds Job.OwnedFiles from disk
// at post-processing start, so a rename that has already happened is captured
// by the seed rather than needing to be tracked into it.
//
// Failures are logged and skipped rather than returned. A rename that does not
// happen leaves the file where it was and its resolved name describing that,
// which is consistent; the caller's verdict is then simply less informed, and
// the conservative branch it takes is to fetch recovery volumes.
func (app *Application) applyPar2Names(
	jobID, dir string,
	sets []par2.Set,
	m *queue.Manifest,
	p *queue.JobProgress,
	log *slog.Logger,
	parseOpts par2.ParseOptions,
) int {
	renames, err := par2.QuickCheckWithOptions(dir, sets, log, parseOpts)
	if err != nil {
		log.Warn("on-demand par2: could not apply par2 names", "job", jobID, "err", err)
		return 0
	}
	if len(renames) == 0 {
		return 0
	}

	// Built once, from the same expression every reader of a resolved name
	// uses, so a file renamed by an earlier entry in this loop is found under
	// its new name rather than its old one.
	idxByName := make(map[string]int, m.NumFiles())
	for fi := range m.NumFiles() {
		idxByName[resolvedName(m, p, fi)] = fi
	}

	applied := 0
	for _, r := range renames {
		to := resolvedNameFor(r.To)
		fi, ok := idxByName[r.From]
		if !ok {
			// par2 relocated something the manifest does not describe under
			// that name — an extracted file, or a name already corrected by a
			// previous run. The move happened and is correct; there is simply
			// no queue row whose resolved name needs updating.
			log.Debug("on-demand par2: renamed a file the manifest does not name",
				"job", jobID, "from", r.From, "to", r.To)
			continue
		}
		if err := app.queue.SetFileFilename(jobID, fi, to); err != nil &&
			!errors.Is(err, queue.ErrNotFound) && !errors.Is(err, queue.ErrJobNotResident) {
			log.Warn("on-demand par2: renamed on disk but could not record the new name",
				"job", jobID, "from", r.From, "to", to, "err", err)
			continue
		}
		delete(idxByName, r.From)
		idxByName[to] = fi
		applied++
	}

	log.Info("on-demand par2: applied par2 names",
		"job", jobID, "renamed", len(renames), "recorded", applied)
	return applied
}

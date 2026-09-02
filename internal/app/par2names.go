package app

import (
	"errors"
	"log/slog"

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

// applyPar2Names identifies the delivered files against the par2 index and,
// where identification says a file is not at the path par2 records, renames it
// and updates the resolved name that records where it is.
//
// # This function is the owner of "a file moved"
//
// The on-disk rename and the JobProgress.Filename update are one operation
// here, deliberately. Queue.SetFileFilename had exactly one caller before this
// — pipeline.go's registerFile, at first write — and adding a second
// independent writer of that field would be an owner-model violation: the
// field means "where this file is", so a rename that does not update it makes
// the field a comment rather than an invariant. Renaming through here is the
// only way to change either, so they cannot drift.
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
		if err := app.queue.SetFileFilename(jobID, fi, r.To); err != nil &&
			!errors.Is(err, queue.ErrNotFound) && !errors.Is(err, queue.ErrJobNotResident) {
			log.Warn("on-demand par2: renamed on disk but could not record the new name",
				"job", jobID, "from", r.From, "to", r.To, "err", err)
			continue
		}
		delete(idxByName, r.From)
		idxByName[r.To] = fi
		applied++
	}

	log.Info("on-demand par2: applied par2 names",
		"job", jobID, "renamed", len(renames), "recorded", applied)
	return applied
}

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
// been recorded, which is what every par2 call site already prefers.
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
// worse than it looked: the field kept the pre-rename name, which no longer
// named anything on disk either, and every later reader matched against it.
//
// The two resume readers both do jobFilePath(job, name) and stat the result
// (resume_startup.go), so they look for "shot.jpg" at the top level and do not
// find it. That is not a regression: they did not find the pre-rename name
// either, since relocation had already moved it. Both cases fall through to a
// re-download, which the next assessment relocates again.
//
// One notion of "separator", used both to detect a path and to reduce it: "/",
// which is what the par2 format specifies and what relocateFile splits on via
// filepath.FromSlash. A backslash is NOT treated as one, and that is
// deliberate rather than an oversight — on the platforms this runs on,
// "Screens\shot.jpg" is a single legal filename, and rewriting it to a
// separator would turn a poster-controlled string into a directory component.
func resolvedNameFor(to string) string {
	return path.Base(filepath.ToSlash(to))
}

// recordPar2Names applies the renames an assessment reported and records where
// each file went, so the queue's resolved names keep describing the disk.
//
// # What this owns, and what it does not
//
// It pairs the on-disk move with the JobProgress.Filename update, so on this
// path neither can happen alone. It does NOT own "a file moved" in general:
// postproc's stage_quickcheck applies renames from its own assessment and has
// no queue handle to record through (postproc.Job carries a *queue.Job
// snapshot, not a *queue.Queue). It does not need one — nothing downstream of
// it re-derives a verdict from these names — but a claim here that on-disk
// state and the recorded name cannot drift would be false for that path.
//
// `git grep -n 'queue\.SetFileFilename(' -- '*.go' ':!*_test.go' ':!*.spec'`
// returns 2 lines: pipeline.go's registerFile and this function. (The pattern
// is anchored on the receiver, and escaped, so that it cannot match this
// sentence — a citation that counts itself reads as one higher than the
// population it describes.) So the field has two writers: registerFile
// establishes it at first write and this corrects it. They are disjoint in
// time, not in scope.
//
// It takes the assessment rather than computing one. That is what keeps the
// verdict and the moves derived from the same pre-rename observation — see
// par2.ApplyRenames — and it is why this function no longer decides anything.
//
// The manifest is NOT touched. It records what the NZB said, which is still
// true and is what a retry re-derives from; JobProgress.Filename records what
// is on disk, which is what changed.
//
// Failures are logged and skipped rather than returned. The verdict has
// already been taken, so a rename that does not happen costs post-processing a
// corrected name, not correctness.
func (app *Application) recordPar2Names(
	jobID, dir string,
	a par2.Assessment,
	m *queue.Manifest,
	p *queue.JobProgress,
	log *slog.Logger,
) int {
	applied := par2.ApplyRenames(dir, a, log)
	if len(applied) == 0 {
		return 0
	}

	// Built from the same expression every reader of a resolved name uses, so
	// a file renamed by an earlier entry in this loop is found under its new
	// name rather than its old one.
	idxByName := make(map[string]int, m.NumFiles())
	for fi := range m.NumFiles() {
		idxByName[resolvedName(m, p, fi)] = fi
	}

	recorded := 0
	for _, r := range applied {
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
		recorded++
	}

	log.Info("on-demand par2: applied par2 names",
		"job", jobID, "renamed", len(applied), "recorded", recorded)
	return recorded
}

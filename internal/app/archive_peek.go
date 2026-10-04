package app

import (
	"errors"
	"fmt"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/rarheader"
	"github.com/hobeone/gonzbd/internal/unpack"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// errRAR3Skipped marks a RAR3 volume the peek does not list: RAR3 names come
// only from the external unrar, which the completion path never forks.
var errRAR3Skipped = errors.New("rar3 volume: the early check lists RAR5 only")

// archiveMemberNames returns the filenames the file at path declares: the
// members a RAR5 volume lists, or the files a par2 file protects. The kind is
// read from the file's magic bytes, never its name, so an obfuscated name
// does not hide an archive. A file that is neither returns no names and no
// error.
//
// RAR names come from rarheader.InspectRar5, the pure-Go route, which guards
// the engine against panics itself. A RAR3 volume, or a RAR5 volume the engine
// cannot list, is an error and is skipped: rarheader.Inspect would fall back to
// forking `unrar vt`, which has no timeout and would run on the single
// completion goroutine. The par2 parse is guarded by cmdutil.SafeEngineRun. RAR
// members are returned as declared. The names a par2 file declares are
// returned except those unpack.Classify recognises as archive volumes and
// those job.IsPar2File recognises, which the pipeline consumes before the
// post-unpack check, so the peek and that backstop judge the same population.
// Rules.Find judges what is returned.
func archiveMemberNames(path string, par2Opts par2.ParseOptions) (names []string, kind string, err error) {
	ver, err := rarheader.Version(path)
	switch {
	case err == nil && ver == 5:
		info, err := rarheader.InspectRar5(path)
		if err != nil {
			return nil, "rar", fmt.Errorf("list rar members: %w", err)
		}
		return info.Filenames, "rar", nil
	case err == nil:
		return nil, "rar", errRAR3Skipped
	case !errors.Is(err, rarheader.ErrNotRAR):
		return nil, "", fmt.Errorf("read signature: %w", err)
	}
	isPar2, err := par2.HasMagic(path)
	if err != nil {
		return nil, "", fmt.Errorf("read signature: %w", err)
	}
	if !isPar2 {
		return nil, "", nil
	}
	var files []par2.FileDesc
	if err := cmdutil.SafeEngineRun("archive peek: par2 parse panic", func() error {
		var perr error
		files, perr = par2.ParseFileDescriptionsWithOptions(path, par2Opts)
		return perr
	}); err != nil {
		return nil, "par2", fmt.Errorf("parse par2 file: %w", err)
	}
	names = make([]string, 0, len(files))
	for _, f := range files {
		// A par2 set declares its archive volumes and par2 files too. The
		// unpack and par2 stages consume those before unwanted_cleanup runs,
		// so that stage never judges them; judging them here would block, in
		// whitelist mode, a job the backstop lets through.
		if unpack.Classify(f.FileName) != unpack.UnknownArchive || job.IsPar2File(f.FileName) {
			continue
		}
		names = append(names, f.FileName)
	}
	return names, "par2", nil
}

// peekArchiveForUnwanted reads the headers of the file that just completed
// and, when they name a file with an unwanted extension, blocks the job while
// its download is still running. It is an accelerator for the post-unpack
// removal in internal/postproc, which stays the backstop: it sees only what a
// completed RAR5 volume or par2 file declares, and no RAR3, 7z, zip, nested or
// header-encrypted archive.
//
// It runs from completeFinalizedFile, ahead of the DirectUnpack feed and of
// MarkFileComplete, so a flagged volume is never fed to an unpacker
// (`git grep -n 'app\.completeFinalizedFile(' -- 'internal/app/*.go' ':!*_test.go'`
// returns 3 lines: handleFileComplete, stall re-evaluation and the startup
// repair of a stranded finalize).
//
// It does nothing, and reports nothing to the job, when the job is not
// resident or its file cannot be located, and also when:
//   - the job is already Blocked or Approved (Dispatcher.UnwantedState), or the
//     rules cannot be read or are ActionOff;
//   - any article of the file failed. The volume then has holes and its headers
//     cannot be trusted either way; par2 and the backstop deal with it, so one
//     bad article costs only its own file (Standing Design Rule 3);
//   - the headers cannot be read: an unlistable or corrupt volume, a missing
//     unrar, an engine panic. That is logged and never fails the job.
//
// Dispatcher.BlockUnwanted is the owner of the transition and decides under its
// own lock, so concurrent completions that find hits produce one move and only
// the call that made it acts: it aborts the job's DirectUnpacker (a running
// one would otherwise extract the flagged member), and under ActionFail files
// the job through maybeFinalize with the ingest check's message. No lock is
// held across the header read or the file I/O.
func (app *Application) peekArchiveForUnwanted(j *job.Job, fc FileComplete) {
	if app.dispatcher == nil {
		return
	}
	jobID := fc.JobID
	if st, ok := app.dispatcher.UnwantedState(jobID); !ok || st != unwanted.StateNone {
		return
	}
	rules, err := app.config.GetDownloads().UnwantedRules()
	if err != nil {
		app.log.Warn("archive peek skipped: cannot read the unwanted-extension rules",
			"job", jobID, "fileidx", fc.FileIdx, "err", err)
		return
	}
	if rules.Action() == unwanted.ActionOff {
		return
	}
	m, err := j.Manifest()
	if err != nil || fc.FileIdx < 0 || fc.FileIdx >= m.NumFiles() {
		return
	}
	p := j.Progress()
	if p == nil || hasFailedArticle(m, p, fc.FileIdx) {
		return
	}
	// The pipeline's resolved path when it has one. The startup repair of a
	// stranded finalize runs before the pipeline has resolved any, and then
	// the path comes from the filename the job recorded, as
	// resume_startup.go's sweep does; with neither, the file is not guessed at.
	path := ""
	if info, err := app.pipeline.resolveFileInfo(jobID, fc.FileIdx); err == nil {
		path = info.Path
	} else if name := p.FileFilename(fc.FileIdx); name != "" {
		path = app.pipeline.jobFilePath(j.Name(), name)
	}
	if path == "" {
		return
	}
	pp := app.config.GetPostProc()
	names, kind, err := archiveMemberNames(path, par2.ParseOptionsFromConfig(&pp))
	if err != nil {
		app.log.Debug("archive peek: could not list the file's names; leaving it to par2 and post-unpack removal",
			"job", jobID, "fileidx", fc.FileIdx, "err", err)
		return
	}
	found := rules.Find(names)
	if len(found) == 0 {
		return
	}

	app.blockForUnwanted(j, fc.FileIdx, kind, rules.Action(), found)
}

// blockForUnwanted is the acting half of the peek: it asks the dispatcher to
// block the job and, only if this call made the move, applies the action. A
// completion that read StateNone and lost the race to another reaches here
// too and, finding the move made, does nothing.
func (app *Application) blockForUnwanted(j *job.Job, fileIdx int, kind string, action unwanted.Action, found []string) {
	jobID := j.ID()
	moved, err := app.dispatcher.BlockUnwanted(j, action == unwanted.ActionPause)
	if err != nil && !moved {
		app.log.Warn("archive peek: could not block the job",
			"job", jobID, "fileidx", fileIdx, "err", err)
		return
	}
	if err != nil {
		app.log.Warn("archive peek: blocked the job but could not pause it",
			"job", jobID, "fileidx", fileIdx, "err", err)
	}
	if !moved {
		return
	}
	app.log.Warn("downloaded archive names files with unwanted extensions",
		"job", jobID, "source", kind, "fileidx", fileIdx,
		"action", action, "files", found)
	app.duOrch.abortJob(jobID)
	if action == unwanted.ActionFail {
		app.finalizeRegistered(j, unwantedFailMessage(found), true)
	}
	app.emit(Event{Type: "queue_updated", NzoID: jobID})
}

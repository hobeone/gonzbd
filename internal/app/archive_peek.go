package app

import (
	"fmt"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/rarheader"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// archiveMemberNames returns the filenames the file at path declares: the
// members a RAR volume lists, or the files a par2 file protects. The kind is
// read from the file's magic bytes, never its name, so an obfuscated name
// does not hide an archive. A file that is neither returns no names and no
// error.
//
// The RAR names come from rarheader.Inspect, which guards the pure-Go engine
// against panics and runs the external unrar for RAR3 and for a RAR5 volume
// the engine cannot list; the par2 parse is guarded here the same way
// (cmdutil.SafeEngineRun). Names are returned as declared, for Rules.Find to
// judge.
func archiveMemberNames(path string, par2Opts par2.ParseOptions) (names []string, kind string, err error) {
	isRAR, err := rarheader.IsRAR(path)
	if err != nil {
		return nil, "", fmt.Errorf("read signature: %w", err)
	}
	if isRAR {
		info, err := rarheader.Inspect(path)
		if err != nil {
			return nil, "rar", fmt.Errorf("list rar members: %w", err)
		}
		return info.Filenames, "rar", nil
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
		names = append(names, f.FileName)
	}
	return names, "par2", nil
}

// peekArchiveForUnwanted reads the headers of the file that just completed
// and, when they name a file with an unwanted extension, blocks the job while
// its download is still running. It is an accelerator for the post-unpack
// removal in internal/postproc, which stays the backstop: it sees only what a
// completed RAR volume or par2 file declares, and no 7z, zip, nested or
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

	app.blockForUnwanted(jobID, fc.FileIdx, kind, rules.Action(), found)
}

// blockForUnwanted is the acting half of the peek: it asks the dispatcher to
// block the job and, only if this call made the move, applies the action. A
// completion that read StateNone and lost the race to another reaches here
// too and, finding the move made, does nothing.
func (app *Application) blockForUnwanted(jobID string, fileIdx int, kind string, action unwanted.Action, found []string) {
	moved, err := app.dispatcher.BlockUnwanted(jobID, action == unwanted.ActionPause)
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
		app.maybeFinalize(jobID, unwantedFailMessage(found))
	}
	app.emit(Event{Type: "queue_updated", NzoID: jobID})
}

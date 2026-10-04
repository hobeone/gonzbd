package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// ErrUnwantedRefused reports a retry refused because the job names a file with
// an unwanted extension (in the NZB, in a downloaded RAR5 volume, or in a par2
// file), the configured action is fail, and the job has not been approved.
// Retrying with allow_unwanted approves it.
var ErrUnwantedRefused = errors.New("refused: the job names a file with an unwanted extension")

// unwantedFailPrefix is the message a job the check fails carries into
// history, SABnzbd's wording. unwantedFailMessage appends the names.
const unwantedFailPrefix = "Aborted, unwanted extension detected"

// unwantedNamesShown bounds how many offending names the fail message lists.
// The message is shown in a history row, and a post can name hundreds of
// files.
const unwantedNamesShown = 3

// unwantedFailMessage renders the history failure message for names, the
// offending files in NZB order.
func unwantedFailMessage(names []string) string {
	shown := names[:min(len(names), unwantedNamesShown)]
	msg := unwantedFailPrefix + ": " + strings.Join(shown, ", ")
	if more := len(names) - len(shown); more > 0 {
		msg += fmt.Sprintf(" and %d more", more)
	}
	return msg
}

// fileOwedUnwantedFailure files j as failed when persisted state says the
// archive peek owes it a filing: the job is Blocked and its intent is
// IntentRun. Under the pause action BlockUnwanted sets IntentPause in the
// same d.mu span as the move, and the ingest check pauses what it blocks, so
// IntentRun on a Blocked job comes only from the fail action's move, which
// leaves the filing to this function. The enumeration is
// `git grep -nE '\.Unwanted\s*=[^=]' -- '*.go' ':!*_test.go'` (the writers of
// Header.Unwanted: screenUnwanted, BlockUnwanted, resume) and
// `git grep -n 'SetIntent(job.IntentRun)' -- '*.go'` (resume, which approves
// or refuses a Blocked job).
//
// Being derived from state, the filing survives what a message in a local
// variable did not: a completion whose mark failed and is redelivered, and a
// restart (reconcileBeforeFirstTick). It is the one place that files such a
// job. failMsg is the peek's message naming the files; the names are not
// persisted, so a call with "" (a redelivery or the startup sweep) files with
// the prefix alone. Filing is idempotent through the post-processing
// admission, and defers to the Assessing worker for a job that is complete.
func (app *Application) fileOwedUnwantedFailure(j *job.Job, failMsg string) {
	if app.dispatcher == nil {
		return
	}
	row, ok := app.dispatcher.RowJob(j)
	if !ok || row.Header.Unwanted != unwanted.StateBlocked || j.Intent() != job.IntentRun {
		return
	}
	if failMsg == "" {
		failMsg = unwantedFailPrefix
	}
	app.enqueuePostProc(j, row.Header, failMsg, true)
}

// screenUnwanted applies the unwanted-extension check to j, a job about to
// be registered, and records the result in hdr.Unwanted, overwriting what
// the header carried in. priorBlock says the job's history entry was filed
// Blocked: such an entry is held to it even when the NZB's own names are
// clean, so a job that failed for what its archives named is retried only
// with approval. AddJob calls it for every
// ingest source, and retryHistoryJob for every retry
// (`git grep -n 'app\.screenUnwanted(' -- 'internal/app/*.go' ':!*_test.go'`
// returns 2 lines). Once the job is registered, Dispatcher.BlockUnwanted
// and ResumeJobByUser write it instead (peekArchiveForUnwanted calls the
// first).
//
// approved says the job comes already approved — a retry of an approved
// entry, or one the user asked to run with allow_unwanted — and such a job
// is not checked. Otherwise the check reads the filenames the NZB names
// (Manifest.FileSubject) against the live rules. Under ActionPause a
// blocked job's intent is set to pause; under ActionFail too, so it does not
// launch before the caller files it, and the failure message is returned.
//
// It fails closed: if the rules or the job's file list cannot be read, it
// returns an error and the caller must refuse the job rather than add it
// unchecked.
func (app *Application) screenUnwanted(j *job.Job, hdr *dispatch.Header, approved, priorBlock bool) (failMsg string, err error) {
	if approved {
		hdr.Unwanted = unwanted.StateApproved
		return "", nil
	}
	// An entry filed Blocked was refused for what its downloaded archives
	// named, which the NZB's own names need not show, so the retry is held to
	// it as well.
	hdr.Unwanted = unwanted.StateNone
	rules, err := app.config.GetDownloads().UnwantedRules()
	if err != nil {
		return "", fmt.Errorf("unwanted-extension check: %w", err)
	}
	if rules.Action() == unwanted.ActionOff {
		return "", nil
	}
	m, err := j.Manifest()
	if err != nil {
		return "", fmt.Errorf("unwanted-extension check: read the job's file list: %w", err)
	}
	names := make([]string, m.NumFiles())
	for i := range names {
		names[i] = m.FileSubject(i)
	}
	found := rules.Find(names)
	if len(found) == 0 && !priorBlock {
		return "", nil
	}
	failText := unwantedFailPrefix + " in an earlier attempt"
	if len(found) > 0 {
		failText = unwantedFailMessage(found)
	}
	hdr.Unwanted = unwanted.StateBlocked
	if err := j.SetIntent(job.IntentPause); err != nil {
		return "", fmt.Errorf("unwanted-extension check: pause the job: %w", err)
	}
	app.log.Warn("NZB names files with unwanted extensions",
		"job", j.ID(), "name", hdr.Name, "action", rules.Action(), "files", found)
	if rules.Action() == unwanted.ActionFail {
		return failText, nil
	}
	return "", nil
}

package app

import (
	"errors"
	"log/slog"

	"github.com/hobeone/gonzbd/internal/dispatch"
)

// unlessGone returns err, or nil when err is dispatch.ErrNotFound: the job is
// no longer registered under its ID, or not as the instance the call named.
// A call made on behalf of a job that was removed meanwhile gets that answer
// in normal operation. Any other refusal from the dispatcher (a failed Park
// or Settle, a latched cancel intent) is returned.
//
// dispatch.ErrStaleReport is not dropped here. Its one return is in
// Dispatcher.handoff, the body of AdvanceFrom and YieldedFrom, and PauseJob's
// yieldPaused drops it itself; so the Yielded, FinishedJob, PauseJob,
// ResumeJob and Cancel results routed here cannot carry it. AdvanceFrom's
// refusals are classified by appRunner.logAdvance.
func unlessGone(err error) error {
	if errors.Is(err, dispatch.ErrNotFound) {
		return nil
	}
	return err
}

// warnUnlessGone logs err at Warn unless unlessGone drops it.
func warnUnlessGone(log *slog.Logger, msg, id string, err error) {
	if err = unlessGone(err); err == nil {
		return
	}
	log.Warn(msg, "job", id, "err", err)
}

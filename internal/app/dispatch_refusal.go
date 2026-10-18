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
// dispatch.ErrStaleReport is not dropped here, because no result routed here
// carries it. It is built once, in Dispatcher.handoff, the body of
// AdvanceFrom and YieldedFrom: `git grep -n -E 'Errorf\(.*ErrStaleReport' --
// '*.go' ':!*_test.go'` returns 1 line. AdvanceFrom's refusals are classified
// by appRunner.logAdvance instead, and YieldedFrom's one caller, the
// dispatcher's yieldPaused, drops it itself.
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

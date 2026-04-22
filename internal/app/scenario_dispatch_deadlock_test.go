package app_test

import (
	"testing"
)

// TestDispatch_NoDeadlockWhenCompletionsFull was the A.4 red scenario for
// the B.2 dispatcher deadlock. The actual regression guard lives at the
// layer where the bug lived:
//
//	internal/downloader/dispatch_deadlock_test.go:
//	  TestDispatchPass_ExhaustedEmitsDoNotBlockQueueWriters
//
// That test pins CompletionsBuffer=1, lets exhausted emits fill the
// channel with no consumer, and asserts a queue write-lock-requiring
// call still completes promptly. Pre-B.2 it deadlocks.
//
// The app-level harness cannot reproduce the deadlock cleanly — by the
// time the pipeline consumer is wired up, the very thing that would
// block the dispatcher (full completions + queue-writing consumer) is
// the normal operating mode, not a pathological state.
func TestDispatch_NoDeadlockWhenCompletionsFull(t *testing.T) {
	t.Skip("covered by TestDispatchPass_ExhaustedEmitsDoNotBlockQueueWriters in internal/downloader")
}

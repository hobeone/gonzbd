package postproc

import (
	"errors"
	"testing"
)

// TestJob_FailReason pins the one derivation of a failed job's reason: it is
// empty exactly when Failed is false, a flag with no recorded cause falls
// back to a generic phrase, and a cause the stage returned is kept only for
// the flag that stage newly set.
func TestJob_FailReason(t *testing.T) {
	t.Parallel()
	boom := errors.New("need 1 more recovery blocks")

	t.Run("not failed", func(t *testing.T) {
		t.Parallel()
		j := &Job{}
		if got := j.FailReason(); got != "" || j.Failed() {
			t.Errorf("FailReason = %q, Failed = %v, want empty and false", got, j.Failed())
		}
	})
	t.Run("flags without a cause", func(t *testing.T) {
		t.Parallel()
		j := &Job{ParError: true, UnpackError: true}
		if got, want := j.FailReason(), "repair failed; unpack failed"; got != want {
			t.Errorf("FailReason = %q, want %q", got, want)
		}
	})
	t.Run("fail message alone", func(t *testing.T) {
		t.Parallel()
		j := &Job{FailMsg: "Aborted: too many missing articles, beyond repair"}
		if got, want := j.FailReason(), j.FailMsg; got != want {
			t.Errorf("FailReason = %q, want %q", got, want)
		}
	})
	t.Run("cause kept for the newly set flag only", func(t *testing.T) {
		t.Parallel()
		j := &Job{ParError: true}
		j.recordStageFailure(true, false, boom) // a later stage errors; ParError was already set
		if got, want := j.FailReason(), "repair failed"; got != want {
			t.Errorf("FailReason = %q, want %q", got, want)
		}
		j.ParError = false
		j.recordStageFailure(false, false, nil)
		j.ParError = true
		j.recordStageFailure(false, false, boom)
		if got := j.FailReason(); got != boom.Error() {
			t.Errorf("FailReason = %q, want %q", got, boom.Error())
		}
	})
}

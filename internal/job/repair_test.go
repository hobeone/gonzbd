package job_test

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

func TestJob_RepairState_UnknownWhenNoProgress(t *testing.T) {
	t.Parallel()

	j := job.New("job-1", "Test Job", job.Policy{})
	if got := j.RepairState(); got != job.RepairUnknown {
		t.Errorf("RepairState() = %v, want %v", got, job.RepairUnknown)
	}
}

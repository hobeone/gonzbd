package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/job"
)

const earlyAbortReason = "Aborted: 80%+ of resolved articles failed (DMCA'd or expired)"

// hopelessFiles is a job whose one recovery volume (100 B) covers the loss of
// part02 (50 B) and not the loss of part01 (1000 B).
var hopelessFiles = []failMsgFile{
	{subject: "movie.part01.rar", bytes: 1000},
	{subject: "movie.part02.rar", bytes: 50},
	{subject: "movie.vol01+02.par2", bytes: 100},
}

const (
	failRepairable = 1 // part02: within the recovery volume's capacity
	failBeyond     = 0 // part01: beyond it, but not every byte of the job
)

// TestHopelessReason pins the one producer of a hopeless job's reason: it is
// never "", it is failMsgForJob's verdict whenever there is one, and it names
// its trigger only when there is not.
func TestHopelessReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		fail    int
		trigger hopelessTrigger
		want    string // a substring of the reason
		notWant string
	}{
		{"early abort, not hopeless", failRepairable, hopelessEarlyAbort, earlyAbortReason, "exceeds repair capacity"},
		{"health gate, repairable", failRepairable, hopelessHealthGate, "exceeded the job's repair capacity", "80%"},
		{"early abort, beyond capacity", failBeyond, hopelessEarlyAbort, "exceeds repair capacity of", "80%"},
		{"health gate, beyond capacity", failBeyond, hopelessHealthGate, "exceeds repair capacity of", "80%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j := buildFailMsgJob(t, hopelessFiles, tc.fail)
			got := hopelessReason(j, tc.trigger)
			if !strings.Contains(got, tc.want) {
				t.Errorf("hopelessReason = %q, want it to contain %q", got, tc.want)
			}
			if strings.Contains(got, tc.notWant) {
				t.Errorf("hopelessReason = %q, must not contain %q", got, tc.notWant)
			}
		})
	}
}

// TestHopelessCallbacks_CarryTheirTrigger pins which trigger each detector's
// callback passes: the pipeline's is the early-abort heuristic, the
// downloader's is the Early Health Gate. The job is not hopeless, so the reason
// that reaches it is the trigger's own wording, and it records Assessing as
// its next state, so the hand-over is deferred to its Assessing worker and the
// reason can be read back.
func TestHopelessCallbacks_CarryTheirTrigger(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fire func(app *Application, id string)
		want string
	}{
		{"pipeline onJobHopeless", func(app *Application, id string) { app.pipeline.onJobHopeless(id) }, earlyAbortReason},
		{"downloader OnJobHopeless", func(app *Application, id string) { app.buildDownloaderOptions().OnJobHopeless(id) },
			hopelessReason(nil, hopelessHealthGate)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestApplication(t)
			j := addFailMsgJob(t, app, hopelessFiles, failRepairable)
			if err := j.BeginAttempt(time.Now()); err != nil {
				t.Fatalf("BeginAttempt: %v", err)
			}
			if err := j.SetNext(job.Assessing); err != nil {
				t.Fatalf("SetNext: %v", err)
			}
			tc.fire(app, j.ID())
			if got := app.postProcAdmissions.takeDeferred(j); !slices.Equal(got, []string{tc.want}) {
				t.Errorf("reasons deferred to the Assessing worker = %q, want [%q]", got, tc.want)
			}
		})
	}
}

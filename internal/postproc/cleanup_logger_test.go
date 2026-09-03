package postproc

import (
	"context"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// attrRecorder captures the attributes a logger carries.
//
// That is the only observable a *slog.Logger has: With() returns a new logger
// and the bound attributes are not readable from it. Emitting one record and
// reading the handler's copy is how the binding is observed rather than
// assumed.
type attrRecorder struct{ attrs map[string]string }

func newAttrRecorder() *attrRecorder { return &attrRecorder{attrs: map[string]string{}} }

func (r *attrRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *attrRecorder) WithGroup(string) slog.Handler            { return r }
func (r *attrRecorder) Handle(context.Context, slog.Record) error {
	return nil
}

func (r *attrRecorder) WithAttrs(as []slog.Attr) slog.Handler {
	for _, a := range as {
		r.attrs[a.Key] = a.Value.String()
	}
	return r
}

// TestCleanupStageLogger_BindsJobAndStage covers all four branches of the
// logger helper on both cleanup stages: {stage Log set, unset} x {job.Queue
// present, nil}.
//
// These helpers are pre-existing untested debt, surfaced by
// check_test_alignment because a //dupcomment:ok marker was added to both
// files. The gate's scope is every unexported helper in a TOUCHED file, so
// this is the real test its rules require rather than a dummy reference.
//
// The nil-Queue cases assert the ABSENCE of a "job" key, not merely the
// presence of "stage". Asserting only the latter would pass against a helper
// that always bound a job ID — including an empty one read off a nil Queue —
// which is the branch the guard exists for.
func TestCleanupStageLogger_BindsJobAndStage(t *testing.T) {
	const jobID = "job-42"

	// Each entry returns the stage's logger built against rec, either as the
	// stage's own Log or as the process default.
	// Keyed by the stage's own Name(), which is what logger binds — taken from
	// the implementations rather than restated, so a rename cannot leave this
	// table asserting a string nothing produces.
	stages := map[string]func(rec *slog.Logger, j *Job) *slog.Logger{
		(&ExtensionCleanupStage{}).Name(): func(l *slog.Logger, j *Job) *slog.Logger {
			return (&ExtensionCleanupStage{Log: l}).logger(j)
		},
		(&SampleCleanupStage{}).Name(): func(l *slog.Logger, j *Job) *slog.Logger {
			return (&SampleCleanupStage{Log: l}).logger(j)
		},
	}

	for stageName, build := range stages {
		for _, useDefault := range []bool{false, true} {
			for _, withQueue := range []bool{true, false} {
				name := stageName
				if useDefault {
					name += "/default-log"
				} else {
					name += "/stage-log"
				}
				if withQueue {
					name += "/with-queue"
				} else {
					name += "/nil-queue"
				}

				t.Run(name, func(t *testing.T) {
					rec := newAttrRecorder()
					var stageLog *slog.Logger
					if useDefault {
						// The Log == nil branch reads slog.Default().
						prev := slog.Default()
						slog.SetDefault(slog.New(rec))
						t.Cleanup(func() { slog.SetDefault(prev) })
					} else {
						stageLog = slog.New(rec)
					}

					jobRec := &Job{}
					if withQueue {
						jobRec.Job = job.New(jobID, jobID, job.Policy{})
					}
					build(stageLog, jobRec).Info("probe")

					if got := rec.attrs["stage"]; got != stageName {
						t.Errorf("stage attr = %q, want %q", got, stageName)
					}
					got, present := rec.attrs["job"]
					if !withQueue {
						if present {
							t.Errorf("job attr = %q, want it absent when Job.Job is nil", got)
						}
						return
					}
					if got != jobID {
						t.Errorf("job attr = %q, want %q", got, jobID)
					}
				})
			}
		}
	}
}

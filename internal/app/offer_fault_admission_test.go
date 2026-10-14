package app

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/storagefault"
)

// recordingHandler keeps every record it is handed.
type recordingHandler struct{ recs *[]slog.Record }

func (recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.recs = append(*h.recs, r.Clone())
	return nil
}
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// TestOfferFaultToAdmission: a fault offered to an admission lands where the
// admission's state sends it, and the one Error line says which. The state
// column is read from the admission itself, so a row fails for a fault that
// went to the wrong place even when the message is right.
func TestOfferFaultToAdmission(t *testing.T) {
	t.Parallel()
	fault := storagefault.Classify("write", "/mnt/dl/a.bin", syscall.ENOSPC)
	reason := faultReason(fault)
	const other = "Failed: some other reason"

	for _, tc := range []struct {
		name string
		// setup puts the admission in the state the row offers the fault to.
		setup func(a *postProcAdmissions, j *job.Job)
		// wantMsg is a distinctive part of the logged message.
		wantMsg string
		// wantAdmitted is whether j holds an admission afterwards.
		wantAdmitted bool
		// wantFailMsg is the admission's reason afterwards.
		wantFailMsg string
		// wantNoted is whether the fault's reason is among the admission's notes.
		wantNoted bool
	}{
		{
			name:         "an admission with no reason takes it as the run's",
			setup:        func(a *postProcAdmissions, j *job.Job) { a.admit(j, "") },
			wantMsg:      "the run fails with it",
			wantAdmitted: true,
			wantFailMsg:  reason,
		},
		{
			name:         "an admission with another reason notes it",
			setup:        func(a *postProcAdmissions, j *job.Job) { a.admit(j, other) },
			wantMsg:      "so this one is noted",
			wantAdmitted: true,
			wantFailMsg:  other,
			wantNoted:    true,
		},
		{
			name:         "an admission already holding the reason records nothing new",
			setup:        func(a *postProcAdmissions, j *job.Job) { a.admit(j, reason) },
			wantMsg:      "already records this reason",
			wantAdmitted: true,
			wantFailMsg:  reason,
		},
		{
			name: "an admission whose notes are closed logs only",
			setup: func(a *postProcAdmissions, j *job.Job) {
				a.admit(j, other)
				a.closeAndTakeNotes(j)
			},
			wantMsg:      "history entry is already being written",
			wantAdmitted: true,
			wantFailMsg:  other,
		},
		{
			name: "an ended admission logs only",
			setup: func(a *postProcAdmissions, j *job.Job) {
				a.admit(j, "")
				a.release(j)
			},
			wantMsg: "post-processing ended",
		},
		{
			// Neither caller offers a fault for such a job; the arm exists so a
			// refactor that made it reachable is logged, not dropped.
			name:         "a job never admitted is logged as not held",
			setup:        func(*postProcAdmissions, *job.Job) {},
			wantMsg:      "was not held",
			wantAdmitted: true,
			wantFailMsg:  reason,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var recs []slog.Record
			app := newTestApplication(t)
			app.log = slog.New(recordingHandler{recs: &recs})
			j := job.New("offer-fault", "offer-fault", job.Policy{})
			tc.setup(&app.postProcAdmissions, j)

			app.offerFaultToAdmission(j, fault, "verify")

			if len(recs) != 1 {
				t.Fatalf("logged %d records, want 1", len(recs))
			}
			rec := recs[0]
			if rec.Level != slog.LevelError {
				t.Errorf("level = %v, want Error", rec.Level)
			}
			if !strings.Contains(rec.Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", rec.Message, tc.wantMsg)
			}
			attrs := map[string]string{}
			rec.Attrs(func(a slog.Attr) bool {
				attrs[a.Key] = a.Value.String()
				return true
			})
			for k, want := range map[string]string{"job": j.ID(), "source": "verify", "fault": fault.Error()} {
				if attrs[k] != want {
					t.Errorf("attr %s = %q, want %q", k, attrs[k], want)
				}
			}

			a := &app.postProcAdmissions
			a.mu.Lock()
			cur, admitted := a.jobs[j]
			var failMsg string
			var notes []string
			if admitted {
				failMsg, notes = cur.failMsg, slices.Clone(cur.notes)
			}
			a.mu.Unlock()
			if admitted != tc.wantAdmitted {
				t.Errorf("admitted = %v, want %v", admitted, tc.wantAdmitted)
			}
			if failMsg != tc.wantFailMsg {
				t.Errorf("admission reason = %q, want %q", failMsg, tc.wantFailMsg)
			}
			if noted := slices.Contains(notes, reason); noted != tc.wantNoted {
				t.Errorf("admission notes = %q, want %q among them: %v", notes, reason, tc.wantNoted)
			}
		})
	}
}

// TestOfferFaultToAdmission_UnreachableArmNamesOutcome: the default arm's
// message carries the outcome's number, so a log reader can tell which
// unreachable outcome came back.
func TestOfferFaultToAdmission_UnreachableArmNamesOutcome(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	app := newTestApplication(t)
	app.log = slog.New(slog.NewTextHandler(&buf, nil))
	j := job.New("unheld", "unheld", job.Policy{})
	app.offerFaultToAdmission(j, storagefault.Classify("write", "/x", syscall.ENOSPC), "close")
	if !strings.Contains(buf.String(), "outcome 0") {
		t.Errorf("log = %q, want it to name outcome 0 (admitted)", buf.String())
	}
}

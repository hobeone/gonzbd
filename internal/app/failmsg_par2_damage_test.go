package app

import (
	"strings"
	"testing"
)

// TestFailMsgForJob_FailedPar2DoesNotCondemnIntactContent pins the numerator
// side of the beyond-repair decision: a par2 file failing to download is not
// damage that needs repairing.
//
// Repair capacity answers "how much damaged content can we reconstruct". A
// par2 file — index or recovery volume — is not content. When one fails, no
// content became unrecoverable; the job simply has less repair capacity than
// it hoped for, which the denominator already accounts for. Counting it as
// damage instead condemns a job for the loss of a file whose only purpose was
// to rescue other files.
//
// The case that makes this concrete: a par2 set consisting of an index and no
// recovery volumes. All content downloads. The index's own article fails.
// There is nothing to repair and nothing to repair it with, but the content is
// complete and unpacks — yet a naive gate sees "failed bytes > zero capacity"
// and discards the release.
//
// This was previously masked by an accident rather than handled: while the
// capacity figure counted the index, the index's own failure was compared
// against its own size, and `failed > capacity` was false by exact tie.
// Removing the index from the capacity figure breaks that tie, so the
// numerator has to say what it always meant.
func TestFailMsgForJob_FailedPar2DoesNotCondemnIntactContent(t *testing.T) {
	t.Run("index-only set, the index itself fails", func(t *testing.T) {
		job := buildFailMsgJob(t, []failMsgFile{
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.par2", bytes: 50},
		}, 1) // the index is the file that fails

		if got := job.Progress().FailedBytes(); got != 50 {
			t.Fatalf("fixture guard: FailedBytes() = %d, want 50 — the index's article must be the failure", got)
		}

		if got := failMsgForJob(job); got != "" {
			t.Errorf("failMsgForJob() = %q, want \"\" (proceed to post-processing).\n"+
				"Every content byte downloaded; only the par2 index failed. There is nothing "+
				"to repair, so the job is not beyond repair — it simply cannot be verified.", got)
		}
	})

	t.Run("recovery volume fails, content intact", func(t *testing.T) {
		job := buildFailMsgJob(t, []failMsgFile{
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.vol01+02.par2", bytes: 300},
		}, 1) // the volume fails

		if got := failMsgForJob(job); got != "" {
			t.Errorf("failMsgForJob() = %q, want \"\" — losing a recovery volume reduces "+
				"repair capacity, it does not damage content", got)
		}
	})

	t.Run("content failure still condemns when capacity is absent", func(t *testing.T) {
		job := buildFailMsgJob(t, []failMsgFile{
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.par2", bytes: 50},
		}, 0) // content fails this time

		got := failMsgForJob(job)
		if got == "" {
			t.Error("failMsgForJob() = \"\", want a beyond-repair verdict: content failed and " +
				"there are no recovery volumes to rebuild it from")
		}
		if !strings.Contains(got, "no par2 recovery volumes available") {
			t.Errorf("failMsgForJob() = %q, want the no-recovery-volumes verdict", got)
		}
	})

	t.Run("mixed failure counts only the content half", func(t *testing.T) {
		// 200 B of content fails against 300 B of recovery capacity, plus a
		// failed 300 B volume. Counting the volume's bytes as damage would
		// make it 500 > 300 and condemn a job that is comfortably repairable
		// on the content alone.
		job := buildFailMsgJob(t, []failMsgFile{
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.part02.rar", bytes: 200},
			{subject: "movie.vol01+02.par2", bytes: 300},
			{subject: "movie.vol03+04.par2", bytes: 300},
		}, 1, 2)

		if got := failMsgForJob(job); got != "" {
			t.Errorf("failMsgForJob() = %q, want \"\" — 200 B of damaged content against "+
				"600 B of declared recovery volumes is repairable; the failed volume's own "+
				"300 B is not damage", got)
		}
	})
}

package app

import (
	"strings"
	"testing"
)

// TestFailMsgForJob_UnrecognizedPar2WithholdsTheZeroCapacityVerdict pins the
// one conclusion this codebase is not entitled to draw from a filename.
//
// Repair capacity is classified from the NZB subject, before a byte is
// downloaded, by looking for the ".volNNN+MM.par2" pattern. That pattern is a
// convention, not a guarantee. The PAR2 specification says a file carrying
// recovery slices "should" be named that way; it does not require it, does not
// forbid recovery slices in a plainly-named .par2 file, and does not define an
// "index file" at all. par2 itself reads packets and ignores names — a
// recovery volume renamed to drop its .vol segment still repairs. SABnzbd,
// which has two decades of exposure to real posts, opens each base .par2 and
// counts recovery packets by scanning bytes rather than trusting the name.
//
// So "no file matched the volume pattern" means the capacity is unknown, not
// that it is zero. Those are the same value and opposite claims, and one of
// them gets a complete download thrown away: a set whose only par2 file is
// plainly named would report zero capacity and be declared beyond repair while
// its recovery data sat on disk, fully downloaded.
//
// The gate therefore withholds the zero-capacity verdict whenever the job has
// par2 files but none recognized as volumes. It still fires when the job has
// no par2 files at all, where zero really is zero.
func TestFailMsgForJob_UnrecognizedPar2WithholdsTheZeroCapacityVerdict(t *testing.T) {
	t.Run("plainly-named par2 present: capacity unknown, do not condemn", func(t *testing.T) {
		// One par2 file, conventionally named as an index. We cannot know
		// from here whether it carries recovery slices.
		job := buildFailMsgJob(t, []failMsgFile{
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.part02.rar", bytes: 200},
			{subject: "movie.par2", bytes: 300},
		}, 1) // content fails

		if got := job.RecoveryBytes(); got != 0 {
			t.Fatalf("fixture guard: RecoveryBytes() = %d, want 0 — no subject matches the volume pattern", got)
		}

		if got := failMsgForJob(job); got != "" {
			t.Errorf("failMsgForJob() = %q, want \"\".\n"+
				"No subject matched the volume pattern, but the job does carry a par2 file, "+
				"so its capacity is unknown rather than absent. Declaring the job beyond "+
				"repair here discards a download that par2 may well be able to fix.", got)
		}
	})

	t.Run("no par2 at all: zero capacity is a real finding", func(t *testing.T) {
		job := buildFailMsgJob(t, []failMsgFile{
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.part02.rar", bytes: 200},
		}, 1)

		got := failMsgForJob(job)
		if got == "" {
			t.Fatal("failMsgForJob() = \"\", want a beyond-repair verdict: the job carries no " +
				"par2 files whatsoever, so there is nothing that could repair the damage")
		}
		if !strings.Contains(got, "no par2") {
			t.Errorf("failMsgForJob() = %q, want the no-par2 verdict", got)
		}
	})

	t.Run("recognized volumes present: the capacity comparison still applies", func(t *testing.T) {
		// A normal set. Capacity is known, damage exceeds it, so the verdict
		// stands — the guard must not disable the gate for ordinary jobs.
		job := buildFailMsgJob(t, []failMsgFile{
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.part02.rar", bytes: 500},
			{subject: "movie.par2", bytes: 50},
			{subject: "movie.vol00+01.par2", bytes: 100},
		}, 1) // 500 B of damage against 100 B of recognized capacity

		got := failMsgForJob(job)
		if !strings.Contains(got, "exceeds repair capacity") {
			t.Errorf("failMsgForJob() = %q, want the exceeds-capacity verdict — the guard applies "+
				"only when nothing was recognized, not whenever a plain .par2 exists", got)
		}
	})
}

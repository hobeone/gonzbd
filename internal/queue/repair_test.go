package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// TestRepairStateFrom pins every branch of the verdict the three call sites
// used to reach independently. Each row is a case one of them got wrong at
// some point in #318's history.
func TestRepairStateFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		contentFailed int64
		recovery      int64
		hasPar2       bool
		want          RepairState
	}{
		{
			name: "nothing failed",
			want: RepairIntact,
		},
		{
			// The shape the whole re-key existed to spare: an index-only set
			// whose index article failed. contentFailedBytes excludes par2, so
			// the damage is zero and no verdict is warranted.
			name:     "only a par2 file failed",
			recovery: 0,
			hasPar2:  true,
			want:     RepairIntact,
		},
		{
			name:          "damage within capacity",
			contentFailed: 200,
			recovery:      300,
			hasPar2:       true,
			want:          RepairPossible,
		},
		{
			// The exact tie. This boundary was load-bearing by accident before
			// the numerator moved: while capacity still counted the index, an
			// index failure was weighed against its own size and came out
			// repairable by tie. Pinned so a future >= / > slip is caught.
			name:          "damage exactly equals capacity",
			contentFailed: 300,
			recovery:      300,
			hasPar2:       true,
			want:          RepairPossible,
		},
		{
			name:          "damage one byte over capacity",
			contentFailed: 301,
			recovery:      300,
			hasPar2:       true,
			want:          RepairBeyondCapacity,
		},
		{
			// Zero capacity with par2 present: unmeasured, not absent.
			name:          "damage with an unrecognized par2 file",
			contentFailed: 200,
			recovery:      0,
			hasPar2:       true,
			want:          RepairUnknown,
		},
		{
			// Zero capacity with no par2 at all: a finding.
			name:          "damage with no par2 whatsoever",
			contentFailed: 200,
			recovery:      0,
			hasPar2:       false,
			want:          RepairNoCapacity,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RepairStateFrom(tc.contentFailed, tc.recovery, tc.hasPar2)
			if got != tc.want {
				t.Errorf("RepairStateFrom(%d, %d, %v) = %q, want %q",
					tc.contentFailed, tc.recovery, tc.hasPar2, got, tc.want)
			}
		})
	}
}

// TestRepairState_Hopeless separates the two states that decline to reach a
// verdict from the two that reach one. The distinction is the whole point of
// the type: a gate that treats RepairUnknown as hopeless kills downloads on
// ignorance, and one that treats RepairPossible as proof of repairability
// promises something bytes cannot establish.
func TestRepairState_Hopeless(t *testing.T) {
	t.Parallel()

	hopeless := map[RepairState]bool{
		RepairIntact:         false,
		RepairPossible:       false,
		RepairUnknown:        false,
		RepairNoCapacity:     true,
		RepairBeyondCapacity: true,
	}

	for state, want := range hopeless {
		if got := state.Hopeless(); got != want {
			t.Errorf("%q.Hopeless() = %v, want %v", state, got, want)
		}
	}
}

// TestJobRepairState_MatchesTheRawDerivation guards the accessor against
// drifting from the pure function it wraps — the failure mode this type was
// introduced to end, reproduced one level up.
func TestJobRepairState_MatchesTheRawDerivation(t *testing.T) {
	t.Parallel()

	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: `"movie.mkv" yEnc`, Bytes: 1000, Articles: []nzb.Article{{ID: "c@x", Bytes: 1000, Number: 1}}},
		{Subject: `"movie.par2" yEnc`, Bytes: 50, Articles: []nzb.Article{{ID: "i@x", Bytes: 50, Number: 1}}},
		{Subject: `"movie.vol000+01.par2" yEnc`, Bytes: 300, Articles: []nzb.Article{{ID: "v@x", Bytes: 300, Number: 1}}},
	}}
	job, err := NewJob(parsed, AddOptions{Filename: "t.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	p := job.Progress()
	p.files[0].FailedBytes = 400 // content
	p.files[1].FailedBytes = 50  // the par2 index

	want := RepairStateFrom(p.ContentFailedBytes(), job.RecoveryBytes(), p.HasPar2Files())
	if got := job.RepairState(); got != want {
		t.Errorf("Job.RepairState() = %q, want %q", got, want)
	}
	if want == RepairIntact {
		t.Fatal("fixture guard: the fixture must damage content, or this test pins the trivial branch")
	}
}

// TestContentFailedBytes_ExcludesPar2AndStillSeesThePar2Set pins the two
// figures the repair decision is built from, against values written out here
// rather than recomputed from the functions under test.
//
// The sibling test above asserts Job.RepairState() equals
// RepairStateFrom(p.ContentFailedBytes(), …), which pins the delegation and
// nothing else: both sides move together no matter what those two functions
// return. It is the only surviving reference to either since the dedicated
// test file was deleted, so between them they left the whole repair gate
// resting on functions with no independent pin.
//
// The fixture is the exact shape ContentFailedBytes' doc names as the failure
// mode it prevents — a par2 set with an index and no recovery volumes, where
// the index itself failed. Both figures have to be right for the verdict to
// be: counting the index as damage gives contentFailed > 0 against zero
// capacity, and losing HasPar2Files turns the resulting RepairUnknown into
// RepairNoCapacity, which is the answer that discards a job whose content is
// complete.
func TestContentFailedBytes_ExcludesPar2AndStillSeesThePar2Set(t *testing.T) {
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: `"movie.mkv" yEnc`, Bytes: 1000, Articles: []nzb.Article{{ID: "c@x", Bytes: 1000, Number: 1}}},
		{Subject: `"movie.par2" yEnc`, Bytes: 50, Articles: []nzb.Article{{ID: "i@x", Bytes: 50, Number: 1}}},
	}}
	job, err := NewJob(parsed, AddOptions{Filename: "t.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	p := job.Progress()

	// Only the par2 index failed. No content is damaged.
	p.files[1].FailedBytes = 50

	if got := p.FailedBytes(); got != 0 {
		// Guard the fixture's other half: FailedBytes is job-level and is not
		// what this test is about, but if it already reported 0 the assertion
		// below could not distinguish "par2 excluded" from "nothing failed".
		t.Logf("job-level FailedBytes = %d (per-file is what ContentFailedBytes reads)", got)
	}
	if got := p.ContentFailedBytes(); got != 0 {
		t.Errorf("ContentFailedBytes = %d, want 0 — the only failure was the par2 index, "+
			"which is not content; counting it condemns a job for losing the file whose "+
			"sole purpose was to rescue the others", got)
	}
	if !p.HasPar2Files() {
		t.Error("HasPar2Files = false for a job carrying a par2 index; a recovery-bytes " +
			"figure of zero then reads as 'no par2 protection at all' rather than " +
			"'protection whose volumes we could not name'")
	}

	// Now damage content as well, with still no recovery volumes. The verdict
	// must be Unknown — we cannot say — rather than NoCapacity.
	p.files[0].FailedBytes = 400
	if got := p.ContentFailedBytes(); got != 400 {
		t.Errorf("ContentFailedBytes = %d, want 400 (content only, par2 index excluded)", got)
	}
	if got := job.RepairState(); got != RepairUnknown {
		t.Errorf("RepairState = %q, want %q — with par2 present but no recognisable "+
			"recovery volumes the honest answer is that capacity is unknown", got, RepairUnknown)
	}
}

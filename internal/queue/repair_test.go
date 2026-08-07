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

package api

import (
	"fmt"
	"testing"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

type repairHealthFile struct {
	subject string
	bytes   int64
}

// buildRepairHealthJob builds a queued job from the given files and fails the
// articles at failIdx, mirroring internal/app's buildFailMsgJob so the API
// layer can be pinned against the same fixtures the two abort gates use.
func buildRepairHealthJob(t *testing.T, files []repairHealthFile, failIdx ...int) *queue.Job {
	t.Helper()
	parsed := &nzb.NZB{}
	for i, f := range files {
		parsed.Files = append(parsed.Files, nzb.File{
			Subject:  f.subject,
			Bytes:    f.bytes,
			Articles: []nzb.Article{{ID: fmt.Sprintf("a%d@t", i), Bytes: int(f.bytes), Number: 1}},
		})
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "t.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	q := queue.New()
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for _, i := range failIdx {
		ackFailed(t, q, job.ID, fmt.Sprintf("a%d@t", i))
	}
	return job
}

// TestBuildSlot_SendsTheVerdictNotItsInputs pins the queue listing to the same
// verdict the two abort gates reach.
//
// The listing used to send raw figures and let the client re-derive the
// comparison. It reached a different answer twice — once by weighing total
// failed bytes against a capacity figure that excludes the par2 index, and
// once by reading zero capacity as proof of no repair data when the job simply
// carried a par2 file that did not match the volume-naming convention. Neither
// was reachable by a reference search over Go, because the arithmetic lived in
// TypeScript.
//
// Sending queue.RepairState removes the client's opportunity to disagree. The
// verdict's own branches are pinned in internal/queue's TestRepairStateFrom;
// what is tested here is that buildSlot asks for it rather than rebuilding it.
func TestBuildSlot_SendsTheVerdictNotItsInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		files   []repairHealthFile
		failIdx []int
		want    queue.RepairState
	}{
		{
			// The shape this whole line of work exists to spare: index
			// present, no recovery volumes, all content downloaded, the
			// index's own article fails. Weighing total failed bytes here
			// produced a red verdict on a job both gates let proceed.
			name: "a failed par2 index is not content damage",
			files: []repairHealthFile{
				{subject: "movie.part01.rar", bytes: 1000},
				{subject: "movie.par2", bytes: 50},
			},
			failIdx: []int{1},
			want:    queue.RepairIntact,
		},
		{
			// A plainly-named par2 file: the PAR2 specification recommends
			// the .volNNN+MM convention but does not require it, so this file
			// may carry recovery slices the classification cannot see.
			name: "zero capacity with a par2 file present is unknown",
			files: []repairHealthFile{
				{subject: "movie.part01.rar", bytes: 1000},
				{subject: "movie.par2", bytes: 50},
			},
			failIdx: []int{0},
			want:    queue.RepairUnknown,
		},
		{
			name: "no par2 at all leaves the verdict standing",
			files: []repairHealthFile{
				{subject: "movie.part01.rar", bytes: 1000},
				{subject: "movie.part02.rar", bytes: 50},
			},
			failIdx: []int{1},
			want:    queue.RepairNoCapacity,
		},
		{
			name: "content damage within recognized capacity",
			files: []repairHealthFile{
				{subject: "movie.part01.rar", bytes: 1000},
				{subject: "movie.part02.rar", bytes: 200},
				{subject: "movie.vol01+02.par2", bytes: 300},
			},
			failIdx: []int{1},
			want:    queue.RepairPossible,
		},
		{
			name: "content damage beyond recognized capacity",
			files: []repairHealthFile{
				{subject: "movie.part01.rar", bytes: 1000},
				{subject: "movie.part02.rar", bytes: 400},
				{subject: "movie.vol01+02.par2", bytes: 300},
			},
			failIdx: []int{1},
			want:    queue.RepairBeyondCapacity,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job := buildRepairHealthJob(t, tc.files, tc.failIdx...)
			slot := buildSlot(job, false, 0, 0, nil, app.JobCheckpointState{})

			if slot.RepairState != tc.want {
				t.Errorf("RepairState = %q, want %q", slot.RepairState, tc.want)
			}
			// The listing must agree with the job it describes, or the row
			// contradicts the gate that is acting on the same job.
			if got := job.RepairState(); slot.RepairState != got {
				t.Errorf("buildSlot sent %q but Job.RepairState() is %q — the listing "+
					"re-derived the verdict instead of asking for it", slot.RepairState, got)
			}
		})
	}
}

// TestBuildSlot_FailedBytesStaysTheTotal guards the figure the drawer
// displays. It is deliberately *not* the repair numerator: a failed par2 file
// is a real failure worth showing, it is just not damage needing repair.
func TestBuildSlot_FailedBytesStaysTheTotal(t *testing.T) {
	t.Parallel()

	job := buildRepairHealthJob(t, []repairHealthFile{
		{subject: "movie.part01.rar", bytes: 1000},
		{subject: "movie.par2", bytes: 50},
	}, 1)

	slot := buildSlot(job, false, 0, 0, nil, app.JobCheckpointState{})
	if slot.FailedBytes != 50 {
		t.Errorf("FailedBytes = %d, want 50 — the par2 index's failure is still a failure to report",
			slot.FailedBytes)
	}
	if slot.RepairState != queue.RepairIntact {
		t.Errorf("RepairState = %q, want %q — the same failure is not content damage",
			slot.RepairState, queue.RepairIntact)
	}
}

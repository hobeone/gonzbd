package api

import (
	"fmt"
	"testing"

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
		if _, err := q.MarkArticleFailed(job.ID, fmt.Sprintf("a%d@t", i)); err != nil {
			t.Fatalf("MarkArticleFailed(%d): %v", i, err)
		}
	}
	return job
}

// TestBuildSlot_CarriesTheInputsTheHealthVerdictNeeds pins the queue listing as
// the third consumer of the failed-bytes-against-capacity comparison.
//
// The two abort gates — failMsgForJob and the dispatcher's Early Health Gate —
// weigh *content* damage against *recognized* recovery capacity, and withhold a
// verdict entirely when the capacity figure is zero only because no par2 file
// matched the volume-naming convention. The UI renders the same comparison as a
// health label.
//
// Until this test, queueSlot carried neither input: it sent total failed bytes
// (which counts a failed par2 file as damage) beside a capacity figure that had
// just stopped counting the index. The UI could not reproduce the backend's
// judgment from what was on the wire, so it rendered a definite red verdict on
// exactly the job shapes the backend deliberately spares.
func TestBuildSlot_CarriesTheInputsTheHealthVerdictNeeds(t *testing.T) {
	t.Parallel()

	t.Run("a failed par2 index is not content damage", func(t *testing.T) {
		t.Parallel()
		// The shape this PR exists to stop condemning: index present, no
		// recovery volumes, every content byte downloaded, the index's own
		// article fails.
		job := buildRepairHealthJob(t, []repairHealthFile{
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.par2", bytes: 50},
		}, 1)

		slot := buildSlot(job, false, 0, 0, nil)

		if slot.FailedBytes != 50 {
			t.Fatalf("fixture guard: FailedBytes = %d, want 50 — the index's article must be the failure", slot.FailedBytes)
		}
		if slot.RecoveryBytes != 0 {
			t.Fatalf("fixture guard: RecoveryBytes = %d, want 0 — the fixture must have no recovery volumes", slot.RecoveryBytes)
		}
		if slot.ContentFailedBytes != 0 {
			t.Errorf("ContentFailedBytes = %d, want 0 — only the par2 index failed, so no content "+
				"became unrecoverable. Sending the total instead makes the UI render "+
				"\"No repair data\" for a job both abort gates let proceed.", slot.ContentFailedBytes)
		}
	})

	t.Run("zero capacity with a par2 file present is unknown, not absent", func(t *testing.T) {
		t.Parallel()
		// A plainly-named par2 file. The PAR2 specification only recommends
		// the .volNNN+MM convention, so this file may carry recovery slices
		// that the subject-based classification cannot see.
		job := buildRepairHealthJob(t, []repairHealthFile{
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.par2", bytes: 50},
		}, 0)

		slot := buildSlot(job, false, 0, 0, nil)

		if slot.RecoveryBytes != 0 {
			t.Fatalf("fixture guard: RecoveryBytes = %d, want 0", slot.RecoveryBytes)
		}
		if !slot.RecoveryCapacityUnknown {
			t.Error("RecoveryCapacityUnknown = false, want true — the job carries a par2 file that " +
				"was not recognized as a volume, so zero capacity is ignorance rather than a " +
				"finding. Both abort gates withhold their verdict here; the UI must be able to too.")
		}
	})

	t.Run("no par2 at all leaves the zero-capacity verdict standing", func(t *testing.T) {
		t.Parallel()
		job := buildRepairHealthJob(t, []repairHealthFile{
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.part02.rar", bytes: 50},
		}, 1)

		slot := buildSlot(job, false, 0, 0, nil)

		if slot.RecoveryCapacityUnknown {
			t.Error("RecoveryCapacityUnknown = true, want false — the job carries no par2 file of " +
				"any kind, so zero capacity is a finding and the red verdict is correct")
		}
		if slot.ContentFailedBytes != 50 {
			t.Errorf("ContentFailedBytes = %d, want 50 — this failure is content", slot.ContentFailedBytes)
		}
	})

	t.Run("content damage against real volumes is unchanged", func(t *testing.T) {
		t.Parallel()
		job := buildRepairHealthJob(t, []repairHealthFile{
			{subject: "movie.part01.rar", bytes: 1000},
			{subject: "movie.part02.rar", bytes: 200},
			{subject: "movie.vol01+02.par2", bytes: 300},
		}, 1)

		slot := buildSlot(job, false, 0, 0, nil)

		if slot.RecoveryCapacityUnknown {
			t.Error("RecoveryCapacityUnknown = true, want false — a recognized volume is present")
		}
		if slot.ContentFailedBytes != 200 || slot.RecoveryBytes != 300 {
			t.Errorf("ContentFailedBytes/RecoveryBytes = %d/%d, want 200/300",
				slot.ContentFailedBytes, slot.RecoveryBytes)
		}
	})
}

package queue

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

func helperMakeNZB(nFiles int) *nzb.NZB {
	parsed := &nzb.NZB{
		Meta:   map[string][]string{"title": {"test"}},
		Groups: []string{"alt.binaries.test"},
		AvgAge: time.Unix(1700000000, 0),
	}
	for range nFiles {
		parsed.Files = append(parsed.Files, nzb.File{
			Subject: "file.bin",
			Date:    time.Unix(1700000000, 0),
			Bytes:   1_000_000,
			Articles: []nzb.Article{
				{ID: "a1@h", Bytes: 500_000, Number: 1},
				{ID: "a2@h", Bytes: 500_000, Number: 2},
			},
		})
	}
	return parsed
}

func helperMakeJob(t *testing.T, name string) *Job {
	t.Helper()
	parsed := helperMakeNZB(1)
	job, err := NewJob(parsed, AddOptions{
		Filename: name + ".nzb",
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob failed: %v", err)
	}
	job.ID = name
	return job
}

func TestActiveSet_ResidencyProperty(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	q := New(WithStateDir(tmpDir))
	q.SetMaxActiveJobs(2)

	job := helperMakeJob(t, "job1")

	if err := q.Add(job); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	assertResidency := func(phaseName string, expectedResident bool) {
		t.Helper()
		phase := job.Phase()
		isResidentPhase := (phase == PhaseActive || phase == PhaseProcessing)
		if isResidentPhase != expectedResident {
			t.Errorf("[%s] expected resident phase=%v, got phase=%v (%s)", phaseName, expectedResident, isResidentPhase, phase)
		}

		hasManifest := job.Manifest() != nil
		hasProgress := job.Progress() != nil
		inActiveSet := q.ActiveSet().IsResident(job.ID)

		if expectedResident {
			if !hasManifest || !hasProgress || !inActiveSet {
				t.Errorf("[%s] RESIDENCY VIOLATION: expected (manifest!=nil, progress!=nil, inActiveSet=true), got (hasManifest=%v, hasProgress=%v, inActiveSet=%v)",
					phaseName, hasManifest, hasProgress, inActiveSet)
			}
		} else {
			if hasManifest || hasProgress || inActiveSet {
				t.Errorf("[%s] NON-RESIDENCY VIOLATION: expected (manifest==nil, progress==nil, inActiveSet=false), got (hasManifest=%v, hasProgress=%v, inActiveSet=%v)",
					phaseName, hasManifest, hasProgress, inActiveSet)
			}
		}
	}

	// 1. Initially after Add, job is promoted to Downloading (Active)
	assertResidency("1. Promoting to Downloading", true)

	// 2. Transition Active -> Paused (StatusPaused)
	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause failed: %v", err)
	}
	assertResidency("2. Active -> Paused", false)

	// 3. Transition Paused -> Queued (Pending) -> Active (Downloading)
	if err := q.Resume(job.ID); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	// Resume triggers PromoteNext, so job flips to Downloading (Active)
	assertResidency("3. Resumed -> Active (Downloading)", true)

	// 4. Transition Active -> Processing (StatusVerifying)
	if err := q.SetStatus(job.ID, constants.StatusVerifying); err != nil {
		t.Fatalf("SetStatus Verifying failed: %v", err)
	}
	assertResidency("4. Active -> Processing (Verifying)", true)

	// 5. Transition Processing -> Terminal (StatusCompleted)
	if err := q.SetStatus(job.ID, constants.StatusCompleted); err != nil {
		t.Fatalf("SetStatus Completed failed: %v", err)
	}
	assertResidency("5. Processing -> Terminal (Completed)", false)

	// 6. Transition Terminal -> Queued (Retry) -> Active (Downloading)
	job2 := helperMakeJob(t, "job2")
	if err := q.Add(job2); err != nil {
		t.Fatalf("Add job2 failed: %v", err)
	}
	if err := q.SetStatus(job2.ID, constants.StatusFailed); err != nil {
		t.Fatalf("SetStatus Failed failed: %v", err)
	}
	if q.ActiveSet().IsResident("job2") || job2.Manifest() != nil || job2.Progress() != nil {
		t.Errorf("6a. Failed job2 expected non-resident")
	}

	if err := q.Retry(job2.ID); err != nil {
		t.Fatalf("Retry failed: %v", err)
	}
	if !q.ActiveSet().IsResident("job2") || job2.Manifest() == nil || job2.Progress() == nil {
		t.Errorf("6b. Retried job2 expected resident")
	}
}

func TestActiveSet_StructuralMemoryBound(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	q := New(WithStateDir(tmpDir))
	// Pause queue so promotion doesn't run during Add
	q.PauseAll()

	const N = 1000
	for i := range N {
		id := "job-" + strconv.Itoa(i)
		job := helperMakeJob(t, id)
		if err := q.Add(job); err != nil {
			t.Fatalf("Add job %s failed: %v", id, err)
		}
	}

	if q.ActiveSet().Len() != 0 {
		t.Fatalf("Expected ActiveSet.Len() == 0 for paused queue, got %d", q.ActiveSet().Len())
	}

	// Unpaused queue: set maxActive to 4
	q.ResumeAll()
	q.SetMaxActiveJobs(4)

	if q.ActiveSet().Len() != 4 {
		t.Fatalf("Expected ActiveSet.Len() == 4 after resume, got %d", q.ActiveSet().Len())
	}

	residentCount := 0
	nonResidentCount := 0

	for _, j := range q.List() {
		if j.Manifest() != nil {
			residentCount++
			if j.Progress() == nil {
				t.Errorf("Job %s has manifest but nil progress", j.ID)
			}
		} else {
			nonResidentCount++
			if j.Progress() != nil {
				t.Errorf("Job %s has nil manifest but non-nil progress", j.ID)
			}
		}
	}

	if residentCount != 4 {
		t.Errorf("Expected exactly 4 resident jobs, got %d", residentCount)
	}
	if nonResidentCount != N-4 {
		t.Errorf("Expected exactly %d non-resident jobs, got %d", N-4, nonResidentCount)
	}
}

func TestClaim_CorruptManifest(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	q := New(WithStateDir(tmpDir))
	q.PauseAll()

	// Add valid job
	job := helperMakeJob(t, "corrupt-job")
	if err := q.Add(job); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	// Add second valid job
	job2 := helperMakeJob(t, "good-job")
	if err := q.Add(job2); err != nil {
		t.Fatalf("Add good-job failed: %v", err)
	}

	// Corrupt manifest file on disk for corrupt-job
	manifestPath := filepath.Join(tmpDir, "manifests", "corrupt-job.json.gz")
	if err := os.WriteFile(manifestPath, []byte("NOT_A_VALID_GZIP_FILE"), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Resume queue to trigger PromoteNext
	q.ResumeAll()
	q.PromoteNext(context.Background())

	// corrupt-job should have been quarantined
	corruptPath := manifestPath + ".corrupt"
	if _, err := os.Stat(corruptPath); os.IsNotExist(err) {
		t.Errorf("Expected quarantined file %s to exist, but it was not found", corruptPath)
	}

	// good-job should have been promoted successfully to Active
	if !q.ActiveSet().IsResident("good-job") {
		t.Errorf("Expected good-job to be resident in ActiveSet")
	}
}

package api

import "testing"

// TestFirstIncompleteFile_SkipsUnfetchedVolumes pins the skip added with the
// fetch policy. A recovery volume that is never downloaded is never Complete,
// so without the skip it becomes the job's reported current file for the
// whole post-processing phase — a loop a reviewer classifies correctly as
// index-space and still ships the bug.
func TestFirstIncompleteFile_SkipsUnfetchedVolumes(t *testing.T) {
	_, job := newOnDemandPar2Job(t)

	// File 0 is the only content file; complete it. File 1 is the recovery
	// volume, held and therefore never Complete.
	if err := job.MarkFileComplete(0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}

	if got := firstIncompleteFile(job); got != "" {
		t.Errorf("firstIncompleteFile = %q, want empty — a held recovery volume was reported as the current file", got)
	}
}

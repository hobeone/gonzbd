package app

import (
	"context"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// sweptFixture returns the resume unit fixture with its job's manifest in the
// given residency. sweptAnArticle is the check, after the sweep, that it
// reached the job: the fixture records article 0 durable, so a Done bit on it
// proves the sweep seeded the job rather than passing it by.
func sweptFixture(t *testing.T, resident bool) *resumeUnitFixture {
	t.Helper()
	f := newResumeUnitFixture(t)
	// The manifest on disk is what a hydration reads; Add does not write it.
	if err := writeJobManifest(f.app.config.GetGeneral().AdminDir, f.job); err != nil {
		t.Fatalf("writeJobManifest: %v", err)
	}
	if !resident {
		f.job.Evict()
	}
	if f.job.Resident() != resident {
		t.Fatalf("fixture guard: Resident() = %v, want %v", f.job.Resident(), resident)
	}
	return f
}

func sweptAnArticle(t *testing.T, f *resumeUnitFixture) {
	t.Helper()
	if !liveProgress(t, f.app, f.job.ID()).ArticleDone(0) {
		t.Fatal("the sweep did not seed the job's durable article, so the " +
			"residency assertion beside it says nothing about a swept job")
	}
}

// A job the sweep had to load is not left loaded: the dispatcher records no
// residency for it, so nothing would ever evict it while the job holds
// nothing.
func TestResumeAllJobs_EvictsAJobItHydrated(t *testing.T) {
	t.Parallel()
	f := sweptFixture(t, false)

	if err := f.app.resumeAllJobs(t.Context()); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}

	sweptAnArticle(t, f)
	if f.job.Resident() {
		t.Error("the sweep left the manifest of a job it hydrated resident")
	}
}

// The release is a defer, so a sweep aborted by its context still hands back
// the manifest of the job it was in the middle of.
func TestResumeJob_AnAbortedIterationStillReleasesWhatItHydrated(t *testing.T) {
	t.Parallel()
	f := sweptFixture(t, false)
	row, ok := f.app.dispatcher.Row(f.job.ID())
	if !ok {
		t.Fatal("fixture job is not registered")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := f.app.resumeJob(ctx, row, f.job); err == nil {
		t.Fatal("resumeJob returned nil on a cancelled context, so the abort path was not taken")
	}

	if f.job.Resident() {
		t.Error("an aborted iteration left the manifest it hydrated resident")
	}
}

// The dispatcher's own bookkeeping agrees with the eviction: it never recorded
// the sweep's load, so a job that later holds what its position needs is
// hydrated again by the first tick rather than believed resident.
func TestResumeAllJobs_EvictedJobIsHydratedAgainWhenItHolds(t *testing.T) {
	t.Parallel()
	f := sweptFixture(t, false)

	if err := f.app.resumeAllJobs(t.Context()); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}
	if f.job.Resident() {
		t.Fatal("fixture guard: the sweep left the job resident")
	}
	if err := f.job.Grant(job.NewLease(71401)); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	f.app.dispatcher.Tick(t.Context())

	if !f.job.Resident() {
		t.Error("a job holding a lease was not hydrated by the tick after the sweep evicted it")
	}
}

// A job that was resident before the sweep is not the sweep's to evict.
func TestResumeAllJobs_KeepsAJobAlreadyResident(t *testing.T) {
	t.Parallel()
	f := sweptFixture(t, true)

	if err := f.app.resumeAllJobs(t.Context()); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}

	sweptAnArticle(t, f)
	if !f.job.Resident() {
		t.Error("the sweep evicted a job that was resident before it ran")
	}
}

// A job the sweep's own repair handed to post-processing is read by it
// without a hydration, so the sweep must not take its manifest away.
func TestResumeAllJobs_KeepsAHydratedJobAdmittedToPostProcessing(t *testing.T) {
	t.Parallel()
	f := sweptFixture(t, false)
	f.app.postProcAdmissions.admit(f.job, "")

	if err := f.app.resumeAllJobs(t.Context()); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}

	sweptAnArticle(t, f)
	if !f.job.Resident() {
		t.Error("the sweep evicted the manifest of a job admitted to post-processing")
	}
}

// A job holding a lease needs its manifest whatever the sweep did to load it.
func TestResumeAllJobs_KeepsAHydratedJobThatHoldsALease(t *testing.T) {
	t.Parallel()
	f := sweptFixture(t, false)
	if err := f.job.Grant(job.NewLease(71402)); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if err := f.app.resumeAllJobs(t.Context()); err != nil {
		t.Fatalf("resumeAllJobs: %v", err)
	}

	sweptAnArticle(t, f)
	if !f.job.Resident() {
		t.Error("the sweep evicted the manifest of a job that holds a lease")
	}
}

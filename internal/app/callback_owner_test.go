package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/durability"
)

// Each per-job assembler callback reports on the instance whose request it
// processed, and each handler acts on that instance. An attempt that has left the queue reports
// against itself, never against the retry registered under its ID (#1056).
// The fixture is retriedUnderOneID: "first" has left, "second" holds the ID.

func TestHandleArticleWritten_RecordsOnTheWritingInstance(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	first, second := retriedUnderOneID(t, application, "feedface00105603")
	emit(t, first, second)

	application.handleArticleWritten(first, 0, 0, 0, 1000, 0xdead)

	application.recorder.mu.Lock()
	underFirst, underSecond := len(application.recorder.pending[first]), len(application.recorder.pending[second])
	application.recorder.mu.Unlock()
	if underFirst != 1 || underSecond != 0 {
		t.Errorf("rows buffered under first = %d, second = %d; want 1 and 0: the row belongs to the attempt that wrote", underFirst, underSecond)
	}
	if second.Progress().ArticleDone(0) {
		t.Error("the retry's article was marked Done by the earlier attempt's write")
	}
}

func TestHandleArticleRejected_FailsOnTheWritingInstance(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	first, second := retriedUnderOneID(t, application, "feedface00105604")
	emit(t, first, second)

	application.handleArticleRejected(first, 0, 0, "redelivered after its file was completed", assembler.WholeArticle)

	if !first.Progress().ArticleFailed(0) {
		t.Error("the rejection was not recorded on the attempt whose straggler it was")
	}
	if second.Progress().ArticleFailed(0) {
		t.Error("the retry's article was failed by the earlier attempt's rejection")
	}
}

func TestHandleFileUntrusted_LeavesADepartedInstancesRowsAlone(t *testing.T) {
	t.Parallel()
	// The loose-record fixture: newTestApplication has no SQLite store, and
	// the rows the untrust would purge live there.
	a := newLREnv(t).newApp(t)
	application := a.Application
	first, second := retriedUnderOneID(t, application, "feedface00105605")
	// The fixture registers through the dispatcher alone; AddJob would have
	// seeded the job_files rows the written rows hang off.
	if err := seedJobFiles(t.Context(), application.durable, second); err != nil {
		t.Fatalf("seedJobFiles: %v", err)
	}
	emit(t, first, second)
	p := application.pipeline
	if err := p.registerFile(second, 0); err != nil {
		t.Fatalf("registerFile(second): %v", err)
	}
	application.recorder.noteWritten(second, durability.WrittenRow{FileIdx: 0, ArtIdx: 0, Length: 1000})
	if !second.Progress().ArticleDone(0) {
		t.Fatal("fixture: the retry's article is not Done")
	}
	// Flushed, so the row is in SQLite under the ID the two attempts share:
	// an untrust taken for the earlier attempt would delete it there.
	if err := application.recorder.flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := rowsFor(t, a.repo.DB(), second.ID()); len(got) != 1 {
		t.Fatalf("fixture: rows in SQLite under the ID = %d, want 1", len(got))
	}

	application.handleFileUntrusted(first, 0)

	if got := rowsFor(t, a.repo.DB(), second.ID()); len(got) != 1 {
		t.Errorf("rows in SQLite under the ID after the earlier attempt's untrust = %d, want 1: the retry's record was purged", len(got))
	}
	if !second.Progress().ArticleDone(0) {
		t.Error("the retry's written article was returned to Outstanding by the earlier attempt's untrust")
	}
	if _, err := p.lookupFile(second.ID(), 0); err != nil {
		t.Errorf("the retry's cached file info was forgotten by the earlier attempt's untrust: %v", err)
	}
}

func TestHandleArticlesUnwritten_ClearsOnTheWritingInstance(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	first, second := retriedUnderOneID(t, application, "feedface00105607")
	emit(t, first, second)

	application.handleArticlesUnwritten(first, 0, []int32{0})

	if articleInFlight(t, first, 0) {
		t.Error("the attempt's rolled-back article is still Emitted")
	}
	if !articleInFlight(t, second, 0) {
		t.Error("the retry's article was returned to Outstanding by the earlier attempt's roll-back")
	}
}

// stringOwner is an assembler.Owner no production path produces: refFor
// supplies a *job.Job, so instanceOf refuses anything else loudly.
type stringOwner string

func (s stringOwner) ID() string { return string(s) }

func TestInstanceOf_RefusesAnOwnerThatIsNotAJob(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("instanceOf returned for an owner that is not a *job.Job, want a panic")
		}
	}()
	instanceOf(stringOwner("not-a-job"))
}

func TestCompleteFinalizedFile_RefusesADepartedInstance(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	first, second := retriedUnderOneID(t, application, "feedface00105606")

	if err := application.completeFinalizedFile(FileComplete{Job: first, FileIdx: 0}); err == nil {
		t.Error("a completion of an attempt that left the queue was taken, want an error")
	}
	if second.Progress().FileComplete(0) {
		t.Error("the retry's file was marked complete by the earlier attempt's completion")
	}
}

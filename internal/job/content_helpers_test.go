package job

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
)

// TestJobPar2Recovered_ReadsThroughBeforeHydration covers both branches of the
// Job-level accessor. The pre-hydration branch is the one #504 turns on: a
// restored job that is never hydrated — it holds no lease or slot and is not
// Fetching, so neither Hydrate call site reaches it — must still report the
// flag, or persistIfChanged writes false back over the stored row.
func TestJobPar2Recovered_ReadsThroughBeforeHydration(t *testing.T) {
	m := NewManifest([]JobFile{
		{Subject: "f", Bytes: 100, Articles: []JobArticle{{ID: "<a@x>", Bytes: 100, Number: 1}}},
	})

	j := New("j", "j", Policy{})
	if j.Par2Recovered() {
		t.Fatal("a job with neither progress nor restored state must report false")
	}

	j.RestoreProgressState("repair needed", time.Time{}, time.Time{}, true)
	if !j.Par2Recovered() {
		t.Error("Par2Recovered = false before hydration; the restored value must be readable")
	}

	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if !j.Par2Recovered() {
		t.Error("Par2Recovered = false after hydration; AttachContent must seed the flag")
	}
	// Once progress exists it owns the value, so a later change there is what
	// the accessor reports.
	j.progress.restorePar2Recovered(false)
	if j.Par2Recovered() {
		t.Error("Par2Recovered still true after the live record cleared it; the accessor must not keep reading the restored copy")
	}
}

// TestRestoreProgressState_AppliesToALiveRecord covers the branch taken when
// progress already exists. No production caller reaches it — restoreJobMetadata
// runs before hydration — but without it a misordered call would write to
// fields the accessors stop reading once progress is installed and report
// success, which is the silent drop this whole path exists to remove.
func TestRestoreProgressState_AppliesToALiveRecord(t *testing.T) {
	m := NewManifest([]JobFile{
		{Subject: "f", Bytes: 100, Articles: []JobArticle{{ID: "<a@x>", Bytes: 100, Number: 1}}},
	})
	j := New("j", "j", Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	start := time.Unix(1700000100, 0).UTC()
	finish := time.Unix(1700000200, 0).UTC()
	j.RestoreProgressState("repair needed", start, finish, true)

	if got := j.Progress().Par2ReleaseReason(); got != "repair needed" {
		t.Errorf("Par2ReleaseReason = %q, want %q — the write must reach the live record", got, "repair needed")
	}
	if !j.Progress().Par2Recovered() {
		t.Error("Par2Recovered = false; the write must reach the live record")
	}
	if got := j.Progress().DownloadStarted(); !got.Equal(start) {
		t.Errorf("DownloadStarted = %v, want %v", got, start)
	}
	if got := j.Progress().DownloadFinished(); !got.Equal(finish) {
		t.Errorf("DownloadFinished = %v, want %v", got, finish)
	}
	// The Job-level copy must stay empty, so nothing can later seed a second,
	// stale value over the live record.
	if j.restoredPar2Reason != "" || j.restoredPar2Recovered {
		t.Error("the restored* fields were written while progress was live; only one copy may be authoritative")
	}
}

// TestJobStampOrZero covers the filter both restore routes apply. Unix() > 0 is
// the whole predicate, so the boundary is the epoch itself: 1970-01-01 is
// rejected, one second later is kept. A job the process actually ran cannot
// carry either, which is what makes the epoch a safe sentinel for "the store
// held nothing here".
func TestJobStampOrZero(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{name: "zero time", in: time.Time{}, want: time.Time{}},
		{name: "the epoch itself", in: time.Unix(0, 0).UTC(), want: time.Time{}},
		{name: "before the epoch", in: time.Unix(-1, 0).UTC(), want: time.Time{}},
		{name: "one second after the epoch", in: time.Unix(1, 0).UTC(), want: time.Unix(1, 0).UTC()},
		{name: "a real stamp", in: time.Unix(1700000100, 0).UTC(), want: time.Unix(1700000100, 0).UTC()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobStampOrZero(tc.in); !got.Equal(tc.want) {
				t.Errorf("jobStampOrZero(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestAttachContent_RefusesASecondAttach pins the guard. A re-attach would
// install a fresh JobProgress over a live one, discarding every done/failed bit
// and both stamps — and because the first attach zeroed the Job-level copy,
// there would be nothing left to seed the replacement from either. Silent, and
// unrecoverable.
func TestAttachContent_RefusesASecondAttach(t *testing.T) {
	m := NewManifest([]JobFile{
		{Subject: "f", Bytes: 100, Articles: []JobArticle{{ID: "<a@x>", Bytes: 100, Number: 1}}},
	})
	j := New("j", "j", Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("first AttachContent: %v", err)
	}
	markWritten(t, j, 0)

	if err := j.AttachContent(m); err == nil {
		t.Fatal("second AttachContent returned nil; it must refuse rather than replace a live record")
	}
	// The refusal must leave the existing record untouched, not half-replaced.
	if got := j.Progress().PendingArticles(); got != 0 {
		t.Errorf("PendingArticles = %d after a refused re-attach, want 0 — the live record was disturbed", got)
	}
}

// TestRestoreProgressState_FiltersStampsTheSameWayBeforeAndAfterHydration pins
// that both routes apply jobStampOrZero. A stamp accepted into the Job-level
// copy but rejected by restoreDownloadStamps would make the same accessor
// answer differently either side of hydration.
func TestRestoreProgressState_FiltersStampsTheSameWayBeforeAndAfterHydration(t *testing.T) {
	m := NewManifest([]JobFile{
		{Subject: "f", Bytes: 100, Articles: []JobArticle{{ID: "<a@x>", Bytes: 100, Number: 1}}},
	})
	// Unix() <= 0, so isJobStamp rejects it: a stamp this process could not
	// have minted.
	bad := time.Unix(0, 0).UTC()
	good := time.Unix(1700000200, 0).UTC()

	j := New("j", "j", Policy{})
	j.RestoreProgressState("", bad, good, false)

	beforeStart, beforeFinish := j.DownloadStarted(), j.DownloadFinished()
	if !beforeStart.IsZero() {
		t.Errorf("DownloadStarted = %v before hydration, want zero — a non-job stamp must be filtered here too", beforeStart)
	}

	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if got := j.DownloadStarted(); !got.Equal(beforeStart) {
		t.Errorf("DownloadStarted = %v after hydration, %v before; the two tiers filter differently", got, beforeStart)
	}
	if got := j.DownloadFinished(); !got.Equal(beforeFinish) {
		t.Errorf("DownloadFinished = %v after hydration, %v before; the two tiers filter differently", got, beforeFinish)
	}
	if got := j.DownloadFinished(); !got.Equal(good) {
		t.Errorf("DownloadFinished = %v, want %v — a valid stamp must survive both routes", got, good)
	}
}

// TestRestorePar2Recovered covers the door AttachContent seeds through. It
// must be able to write false as well as true: a job restored with the flag
// clear and one that was never persisted at all reach the same seeding line,
// and a setter that only ever latched true would leave a re-fetched job
// claiming a repair it did not need.
func TestRestorePar2Recovered(t *testing.T) {
	p := newJobProgress(NewManifest([]JobFile{
		{Subject: "f", Bytes: 100, Articles: []JobArticle{{ID: "<a@x>", Bytes: 100, Number: 1}}},
	}))
	if p.Par2Recovered() {
		t.Fatal("a fresh JobProgress must start with par2Recovered false")
	}
	p.restorePar2Recovered(true)
	if !p.Par2Recovered() {
		t.Error("Par2Recovered = false after restoring true")
	}
	p.restorePar2Recovered(false)
	if p.Par2Recovered() {
		t.Error("Par2Recovered = true after restoring false; the door must not latch")
	}
}

// TestUndeferRecovery covers undeferRecovery, which is the only function that
// DERIVES par2Recovered from a change it made — `git grep -n 'par2Recovered =
// true' internal/job/` finds one line. It is not the only writer:
// restorePar2Recovered installs a value read back from the store, and
// ResetForRetry clears it.
//
// Its return value is load-bearing: MarkArticleFailed records a release reason
// only when this reports a change, so a wrong "true" would attach a reason to a
// job whose volumes were never released.
func TestUndeferRecovery(t *testing.T) {
	newHeld := func(t *testing.T) *Job {
		t.Helper()
		m := NewManifest([]JobFile{
			{Subject: "data", Bytes: 100, Articles: []JobArticle{{ID: "<d0@x>", Bytes: 100, Number: 1}}},
			{Subject: "vol01+02.par2", Bytes: 100, IsPar2Recovery: true,
				Articles: []JobArticle{{ID: "<p0@x>", Bytes: 100, Number: 1}}},
		})
		j := New("j", "j", Policy{})
		if err := j.AttachContent(m); err != nil {
			t.Fatalf("AttachContent: %v", err)
		}
		j.progress.files[1].Fetch = FetchIfNeeded
		return j
	}

	t.Run("releases a held volume and records the flag", func(t *testing.T) {
		j := newHeld(t)
		if j.progress.Par2Recovered() {
			t.Fatal("fixture guard: Par2Recovered must start false")
		}
		if !j.undeferRecovery([]int{1}) {
			t.Fatal("undeferRecovery reported no change for a held volume")
		}
		if j.progress.files[1].Fetch != FetchAlways {
			t.Errorf("Fetch = %v, want FetchAlways", j.progress.files[1].Fetch)
		}
		if !j.progress.Par2Recovered() {
			t.Error("Par2Recovered = false after a release, want true")
		}
	})

	t.Run("reports no change when nothing was held", func(t *testing.T) {
		j := newHeld(t)
		// File 0 is ordinary data and already FetchAlways, so there is nothing
		// to release and the flag must stay false.
		if j.undeferRecovery([]int{0}) {
			t.Error("undeferRecovery reported a change for an already-FetchAlways file")
		}
		if j.progress.Par2Recovered() {
			t.Error("Par2Recovered = true without any release")
		}
	})

	t.Run("ignores out-of-range indices without reporting a change", func(t *testing.T) {
		j := newHeld(t)
		if j.undeferRecovery([]int{-1, 99}) {
			t.Error("undeferRecovery reported a change for out-of-range indices")
		}
		if j.progress.Par2Recovered() {
			t.Error("Par2Recovered = true after only out-of-range indices")
		}
	})
}

// TestMarkArticleFailed_RespectsRepairPolicy pins the live-download half of
// the fetch-decision policy gate: a permanent article failure must not
// release a job's deferred recovery volumes when the job's Policy.Repair is
// false (PP=0), and must release them when Policy.Repair is true. Repairing
// never runs for a PP=0 job, so releasing them here — during an active
// download, not only at the Assessing verdict — would still spend bandwidth
// nothing uses.
//
// The true-policy case uses PolicyFromPP(1) (PPRepair) rather than
// PolicyFromPP(3): PPRepair sets only Repair true, leaving Unpack and Delete
// false, so it is the fixture that tells the content.go gate apart from a
// mistaken check on Policy.Unpack or Policy.Delete instead of Policy.Repair.
func TestMarkArticleFailed_RespectsRepairPolicy(t *testing.T) {
	newFixture := func(t *testing.T, pol Policy) *Job {
		t.Helper()
		m := NewManifest([]JobFile{
			{Subject: "data", Bytes: 100, Articles: []JobArticle{{ID: "<d0@x>", Bytes: 100, Number: 1}}},
			{Subject: "vol01+02.par2", Bytes: 100, IsPar2Recovery: true,
				Articles: []JobArticle{{ID: "<p0@x>", Bytes: 100, Number: 1}}},
		})
		j := New("j", "j", pol)
		if err := j.AttachContent(m); err != nil {
			t.Fatalf("AttachContent: %v", err)
		}
		j.progress.files[1].Fetch = FetchIfNeeded
		return j
	}

	t.Run("Repair false (PP=0) holds the volume", func(t *testing.T) {
		j := newFixture(t, Policy{}) // zero value: Repair == false, matching PP=0
		if err := j.MarkArticleFailed(0); err != nil {
			t.Fatalf("MarkArticleFailed: %v", err)
		}
		if got := j.progress.files[1].Fetch; got != FetchIfNeeded {
			t.Errorf("recovery volume fetch policy = %v, want FetchIfNeeded (still held); a PP=0 job's live-download "+
				"article failure released it despite Policy.Repair == false", got)
		}
		if j.progress.Par2Recovered() {
			t.Error("Par2Recovered = true after a PP=0 job's article failure; nothing was actually released")
		}
	})

	t.Run("Repair true (PPRepair) releases the volume", func(t *testing.T) {
		j := newFixture(t, PolicyFromPP(1)) // PPRepair: Repair true, Unpack/Delete false
		if err := j.MarkArticleFailed(0); err != nil {
			t.Fatalf("MarkArticleFailed: %v", err)
		}
		if got := j.progress.files[1].Fetch; got != FetchAlways {
			t.Errorf("recovery volume fetch policy = %v, want FetchAlways (released); a PPRepair job's live-download "+
				"article failure must release held recovery volumes", got)
		}
		if !j.progress.Par2Recovered() {
			t.Error("Par2Recovered = false after a PPRepair job's article failure; the volume should have been released")
		}
	})
}

// markWritten marks article artIdx Done the way the recorder does, through
// MarkArticleWritten, with a row that tiles the file's articles end to end.
func markWritten(t testing.TB, j *Job, artIdx int) {
	t.Helper()
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("markWritten: manifest: %v", err)
	}
	var off int64
	fileIdx := 0
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		if artIdx < lo || artIdx >= hi {
			continue
		}
		fileIdx = fi
		for i := lo; i < artIdx; i++ {
			off += int64(m.ArticleBytes(i))
		}
	}
	if err := j.MarkArticleWritten(durability.WrittenRow{
		FileIdx: fileIdx, ArtIdx: int32(artIdx), Offset: off, Length: int64(m.ArticleBytes(artIdx)),
	}); err != nil {
		t.Fatalf("MarkArticleWritten(%d): %v", artIdx, err)
	}
}

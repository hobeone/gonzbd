package job

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/durability"
)

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
	j.RestoreProgressState("repair needed", start, finish)

	if got := j.Progress().Par2ReleaseReason(); got != "repair needed" {
		t.Errorf("Par2ReleaseReason = %q, want %q — the write must reach the live record", got, "repair needed")
	}
	if got := j.Progress().DownloadStarted(); !got.Equal(start) {
		t.Errorf("DownloadStarted = %v, want %v", got, start)
	}
	if got := j.Progress().DownloadFinished(); !got.Equal(finish) {
		t.Errorf("DownloadFinished = %v, want %v", got, finish)
	}
	// The Job-level copy must stay empty, so nothing can later seed a second,
	// stale value over the live record.
	if j.restoredPar2Reason != "" {
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
	j.RestoreProgressState("", bad, good)

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

// TestUndeferRecovery covers undeferRecovery: it moves each held volume it is
// given to FetchAlways and leaves everything else alone.
func TestUndeferRecovery(t *testing.T) {
	newHeld := func(t *testing.T) *Job {
		t.Helper()
		m := NewManifest([]JobFile{
			{Subject: "data", Bytes: 100, Articles: []JobArticle{{ID: "<d0@x>", Bytes: 100, Number: 1}}},
			{Subject: "vol01+02.par2", Bytes: 100,
				Articles: []JobArticle{{ID: "<p0@x>", Bytes: 100, Number: 1}}},
		})
		j := New("j", "j", Policy{})
		if err := j.AttachContent(m); err != nil {
			t.Fatalf("AttachContent: %v", err)
		}
		j.progress.files[1].Fetch = FetchIfNeeded
		return j
	}

	t.Run("releases a held volume", func(t *testing.T) {
		j := newHeld(t)
		j.undeferRecovery([]int{1})
		if j.progress.files[1].Fetch != FetchAlways {
			t.Errorf("Fetch = %v, want FetchAlways", j.progress.files[1].Fetch)
		}
	})

	t.Run("leaves a volume it was not given", func(t *testing.T) {
		j := newHeld(t)
		j.undeferRecovery([]int{0})
		if j.progress.files[1].Fetch != FetchIfNeeded {
			t.Errorf("Fetch = %v, want FetchIfNeeded: the volume was not named", j.progress.files[1].Fetch)
		}
	})

	t.Run("ignores out-of-range indices", func(t *testing.T) {
		j := newHeld(t)
		j.undeferRecovery([]int{-1, 99})
		if j.progress.files[1].Fetch != FetchIfNeeded {
			t.Errorf("Fetch = %v after only out-of-range indices, want FetchIfNeeded", j.progress.files[1].Fetch)
		}
	})
}

// TestMarkArticleFailed_ChangesNoFetchPolicy pins that a permanent article
// failure releases nothing and records no release reason, whatever the job's
// Policy.Repair: the Assessing verdict alone sizes and makes the release
// (app.maybeReleaseRecoveryVolumes).
func TestMarkArticleFailed_ChangesNoFetchPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		pol  Policy
	}{
		{"Repair false (PP=0)", Policy{}},
		{"Repair true (PPRepair)", PolicyFromPP(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManifest([]JobFile{
				{Subject: "data", Bytes: 100, Articles: []JobArticle{{ID: "<d0@x>", Bytes: 100, Number: 1}}},
				{Subject: "vol01+02.par2", Bytes: 100,
					Articles: []JobArticle{{ID: "<p0@x>", Bytes: 100, Number: 1}}},
			})
			j := New("j", "j", tc.pol)
			if err := j.AttachContent(m); err != nil {
				t.Fatalf("AttachContent: %v", err)
			}
			j.progress.files[1].Fetch = FetchIfNeeded
			if err := j.MarkArticleFailed(0, WholeArticle); err != nil {
				t.Fatalf("MarkArticleFailed: %v", err)
			}
			if got := j.progress.files[1].Fetch; got != FetchIfNeeded {
				t.Errorf("recovery volume fetch policy = %v, want FetchIfNeeded: a live failure must not release", got)
			}
			if got := j.Par2ReleaseReason(); got != "" {
				t.Errorf("Par2ReleaseReason = %q, want empty: a live failure records no release", got)
			}
		})
	}
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

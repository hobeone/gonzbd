package job

import (
	"slices"
	"testing"
	"time"
)

// newInterleavedRecoveryJob attaches a repairing job whose files are a
// recovery volume, a content file and a second recovery volume, one article
// each, in that index order.
func newInterleavedRecoveryJob(t *testing.T, restoredRecovered bool) *Job {
	t.Helper()
	m := NewManifest([]JobFile{
		{Subject: "a.vol00+01.par2", Bytes: 100, IsPar2Recovery: true,
			Articles: []JobArticle{{ID: "<v0@x>", Bytes: 100, Number: 1}}},
		{Subject: "a.bin", Bytes: 100, Articles: []JobArticle{{ID: "<d0@x>", Bytes: 100, Number: 1}}},
		{Subject: "a.vol01+02.par2", Bytes: 100, IsPar2Recovery: true,
			Articles: []JobArticle{{ID: "<v1@x>", Bytes: 100, Number: 1}}},
	})
	j := New("j", "j", PolicyFromPP(3))
	j.RestoreProgressState("", time.Time{}, time.Time{}, restoredRecovered)
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	return j
}

// TestInstallFileVerification_RestoreDoesNotUndoARelease pins that a hydration
// never ends with par2Recovered set and a recovery volume still held: nothing
// releases a volume once par2Recovered is set, so such a volume would wait on
// the post-processing retry instead (#858).
//
// Two ways in. The record can say so — the dispatcher's row carries
// par2_recovered and the recorder's job_files row carries the policy, and a
// crash between their writes leaves the release in one and not the other. And
// a hydration can say so itself: files install in index order, so a content
// file's lost article releases the volumes held so far, and a later volume's
// restore puts its hold back.
func TestInstallFileVerification_RestoreDoesNotUndoARelease(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		restoredRecovered bool
		failed            []int32 // file 1's lost articles, by global index
	}{
		{name: "the record already released", restoredRecovered: true},
		{name: "a content loss earlier in the hydration releases", failed: []int32{1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j := newInterleavedRecoveryJob(t, tc.restoredRecovered)
			install := []FileVerification{
				{FileIdx: 0, RestorePolicy: true, Policy: FetchIfNeeded},
				{FileIdx: 1, RestorePolicy: true, Policy: FetchAlways, Failed: tc.failed},
				{FileIdx: 2, RestorePolicy: true, Policy: FetchIfNeeded},
			}
			for _, v := range install {
				if _, err := j.InstallFileVerification(v); err != nil {
					t.Fatalf("InstallFileVerification(%d): %v", v.FileIdx, err)
				}
			}
			p := j.Progress()
			if !p.Par2Recovered() {
				t.Fatal("precondition: Par2Recovered = false, so this is not the case under test")
			}
			if held := p.DeferredRecoveryIndices(); len(held) > 0 {
				t.Errorf("volumes %v still FetchIfNeeded with Par2Recovered set: nothing releases them again", held)
			}
			// The record holds the hold for both volumes, so both are owed a
			// write of the release.
			var owed []int
			for _, st := range j.TakeFetchPolicyChanges() {
				if FetchPolicy(st.FetchPolicy) != FetchAlways {
					t.Errorf("file %d handed to the record at %v, want FetchAlways", st.FileIdx, FetchPolicy(st.FetchPolicy))
				}
				owed = append(owed, st.FileIdx)
			}
			if !slices.Equal(owed, []int{0, 2}) {
				t.Errorf("files handed to the record = %v, want [0 2]", owed)
			}
		})
	}
}

// TestTakeFetchPolicyChanges pins that every writer of a file's fetch policy
// leaves a change for the recorder to take, once, and that a write which
// changes nothing leaves none (#858).
func TestTakeFetchPolicyChanges(t *testing.T) {
	t.Parallel()
	taken := func(j *Job) []int {
		states := j.TakeFetchPolicyChanges()
		if states == nil {
			return nil
		}
		out := make([]int, 0, len(states))
		for _, st := range states {
			out = append(out, st.FileIdx)
		}
		slices.Sort(out)
		return out
	}
	t.Run("set, release and discard", func(t *testing.T) {
		t.Parallel()
		j := newInterleavedRecoveryJob(t, false)
		if err := j.SetFileFetchPolicy(0, FetchIfNeeded); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		if err := j.SetFileFetchPolicy(2, FetchIfNeeded); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		if got := taken(j); !slices.Equal(got, []int{0, 2}) {
			t.Errorf("after the sets, took %v, want [0 2]", got)
		}
		if got := taken(j); got != nil {
			t.Errorf("a second take returned %v, want nothing", got)
		}
		if err := j.SetFileFetchPolicy(1, FetchAlways); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		if got := taken(j); got != nil {
			t.Errorf("a set that changed nothing left %v, want nothing", got)
		}
		if err := j.MarkArticleFailed(1, WholeArticle); err != nil {
			t.Fatalf("MarkArticleFailed: %v", err)
		}
		if got := taken(j); !slices.Equal(got, []int{0, 2}) {
			t.Errorf("after the failure's release, took %v, want [0 2]", got)
		}
		st := j.TakeFetchPolicyChanges()
		if st != nil {
			t.Errorf("a second take after the release returned %+v", st)
		}

		k := newInterleavedRecoveryJob(t, false)
		if err := k.SetFileFetchPolicy(2, FetchIfNeeded); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		k.TakeFetchPolicyChanges()
		if !k.DiscardDeferredPar2() {
			t.Fatal("DiscardDeferredPar2 reported no change")
		}
		states := k.TakeFetchPolicyChanges()
		if len(states) != 1 || states[0].FileIdx != 2 || FetchPolicy(states[0].FetchPolicy) != FetchNever {
			t.Errorf("after the discard, took %+v, want file 2 at FetchNever", states)
		}
	})
	t.Run("hands over the file's whole record state", func(t *testing.T) {
		t.Parallel()
		// The take's state replaces the file's whole job_files row, so a
		// policy change must not also clear its complete flag or filename.
		j := newInterleavedRecoveryJob(t, false)
		if err := j.RecordFileFilename(2, j.Name(), "a.vol01+02.par2"); err != nil {
			t.Fatalf("RecordFileFilename: %v", err)
		}
		if err := j.MarkFileComplete(2); err != nil {
			t.Fatalf("MarkFileComplete: %v", err)
		}
		if err := j.SetFileFetchPolicy(2, FetchNever); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		want, ok := j.FileState(2)
		if !ok || !want.Complete || want.Filename != "a.vol01+02.par2" || FetchPolicy(want.FetchPolicy) != FetchNever {
			t.Fatalf("precondition: FileState(2) = %+v, %v; want complete, named, FetchNever", want, ok)
		}
		states := j.TakeFetchPolicyChanges()
		if len(states) != 1 || states[0] != want {
			t.Errorf("took %+v, want exactly [%+v], the file's whole record state", states, want)
		}
	})
	t.Run("no progress", func(t *testing.T) {
		t.Parallel()
		if got := New("j", "j", Policy{}).TakeFetchPolicyChanges(); got != nil {
			t.Errorf("a job with no progress returned %+v", got)
		}
	})
}

package job

import (
	"slices"
	"testing"
	"time"
)

// newInterleavedRecoveryJob attaches a repairing job whose files are a
// recovery volume, a content file and a second recovery volume, one article
// each, in that index order.
func newInterleavedRecoveryJob(t *testing.T) *Job {
	t.Helper()
	m := NewManifest([]JobFile{
		{Subject: "a.vol00+01.par2", Bytes: 100,
			Articles: []JobArticle{{ID: "<v0@x>", Bytes: 100, Number: 1}}},
		{Subject: "a.bin", Bytes: 100, Articles: []JobArticle{{ID: "<d0@x>", Bytes: 100, Number: 1}}},
		{Subject: "a.vol01+02.par2", Bytes: 100,
			Articles: []JobArticle{{ID: "<v1@x>", Bytes: 100, Number: 1}}},
	})
	j := New("j", "j", PolicyFromPP(3))
	j.RestoreProgressState("", time.Time{}, time.Time{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	return j
}

// TestInstallFileVerification_RestoreKeepsTheHold pins that a hydration
// restores a held volume as held: the restored policy is FetchIfNeeded, and
// neither a restored release reason nor a content file's lost article earlier
// in the same hydration turns it into FetchAlways (#858). Only the Assessing
// verdict releases a volume, and it does so through Job.ReleaseRecovery.
func TestInstallFileVerification_RestoreKeepsTheHold(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		reason string
		failed []int32 // file 1's lost articles, by global index
	}{
		{name: "a restored release reason", reason: "repair needed"},
		{name: "a content loss earlier in the hydration", failed: []int32{1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j := newInterleavedRecoveryJob(t)
			j.SetPar2ReleaseReason(tc.reason)
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
			if held := p.DeferredRecoveryIndices(); !slices.Equal(held, []int{0, 2}) {
				t.Errorf("held volumes = %v, want [0 2]: a restored hold must stay a hold", held)
			}
			for _, fi := range []int{0, 2} {
				if got := p.FileFetchPolicy(fi); got != FetchIfNeeded {
					t.Errorf("file %d policy = %v, want FetchIfNeeded", fi, got)
				}
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
		j := newInterleavedRecoveryJob(t)
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
		if got := taken(j); got != nil {
			t.Errorf("after the failure, took %v, want nothing: a failure changes no fetch policy", got)
		}
		if err := j.SetFileFetchPolicy(2, FetchAlways); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		if got := taken(j); !slices.Equal(got, []int{2}) {
			t.Errorf("after the release, took %v, want [2]", got)
		}

		k := newInterleavedRecoveryJob(t)
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
		j := newInterleavedRecoveryJob(t)
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

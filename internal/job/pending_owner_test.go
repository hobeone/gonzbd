package job

import (
	"slices"
	"testing"
)

// heldVolumeManifest is a two-article content file followed by a three-article
// recovery volume.
func heldVolumeManifest() *Manifest {
	return NewManifest([]JobFile{
		{Subject: `"a.rar" yEnc (1/2)`, Bytes: 200, Articles: []JobArticle{
			{ID: "<a1@x>", Bytes: 100, Number: 1}, {ID: "<a2@x>", Bytes: 100, Number: 2},
		}},
		{Subject: `"a.vol0+1.par2" yEnc (1/3)`, Bytes: 300, IsPar2Recovery: true, Articles: []JobArticle{
			{ID: "<p1@x>", Bytes: 100, Number: 1}, {ID: "<p2@x>", Bytes: 100, Number: 2}, {ID: "<p3@x>", Bytes: 100, Number: 3},
		}},
	})
}

// newHeldVolumeJob attaches heldVolumeManifest to a repairing job.
func newHeldVolumeJob(t *testing.T) (*Job, *Manifest) {
	t.Helper()
	m := heldVolumeManifest()
	j := New("held", "held", PolicyFromPP(3))
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	return j, m
}

// offered returns the articles ForEachUnfinishedArticle hands out.
func offered(t *testing.T, j *Job) []int32 {
	t.Helper()
	var out []int32
	if err := j.ForEachUnfinishedArticle(func(_ int, i int32, _ string, _, _ int, _ string) bool {
		out = append(out, i)
		return true
	}); err != nil {
		t.Fatalf("ForEachUnfinishedArticle: %v", err)
	}
	return out
}

// checkPendingMatchesRecompute asserts that the live pending counters are
// what a recompute derives from the bits, and that they read want.
func checkPendingMatchesRecompute(t *testing.T, j *Job, m *Manifest, want int) {
	t.Helper()
	live := j.Progress()
	derived := live.clone()
	derived.recompute(m)
	if live.PendingArticles() != derived.PendingArticles() {
		t.Errorf("PendingArticles = %d, a recompute derives %d", live.PendingArticles(), derived.PendingArticles())
	}
	for fi := range live.files {
		if live.files[fi].Pending != derived.files[fi].Pending {
			t.Errorf("file %d Pending = %d, a recompute derives %d", fi, live.files[fi].Pending, derived.files[fi].Pending)
		}
	}
	if got := live.PendingArticles(); got != want {
		t.Errorf("PendingArticles = %d, want %d", got, want)
	}
}

// TestFetchPolicyChange_PendingFollowsThePolicy pins #903: a change of a
// file's fetch policy moves the pending counters with it, through every door
// that changes the policy, so a held or discarded file counts nothing and a
// released one counts every article it still owes.
func TestFetchPolicyChange_PendingFollowsThePolicy(t *testing.T) {
	t.Parallel()
	t.Run("ingest holds a volume", func(t *testing.T) {
		t.Parallel()
		j, m := newHeldVolumeJob(t)
		if err := j.SetFileFetchPolicy(1, FetchIfNeeded); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		checkPendingMatchesRecompute(t, j, m, 2)
	})
	t.Run("a hydration restores a hold", func(t *testing.T) {
		t.Parallel()
		j, m := newHeldVolumeJob(t)
		if _, err := j.InstallFileVerification(FileVerification{FileIdx: 1, RestorePolicy: true, Policy: FetchIfNeeded}); err != nil {
			t.Fatalf("InstallFileVerification: %v", err)
		}
		checkPendingMatchesRecompute(t, j, m, 2)
	})
	t.Run("a hydration restores a complete held volume", func(t *testing.T) {
		t.Parallel()
		// Every article of a complete=1 file with no rows is failed after the
		// policy is restored: the failures must not count against a file
		// that is not being fetched.
		j, m := newHeldVolumeJob(t)
		if _, err := j.InstallFileVerification(FileVerification{FileIdx: 1, RestorePolicy: true, Policy: FetchIfNeeded, Complete: true}); err != nil {
			t.Fatalf("InstallFileVerification: %v", err)
		}
		checkPendingMatchesRecompute(t, j, m, 2)
	})
	t.Run("a set releases a held volume", func(t *testing.T) {
		t.Parallel()
		j, m := newHeldVolumeJob(t)
		if err := j.SetFileFetchPolicy(1, FetchIfNeeded); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		j.Evict()
		if err := j.RestoreContent(m); err != nil {
			t.Fatalf("RestoreContent: %v", err)
		}
		if err := j.SetFileFetchPolicy(1, FetchAlways); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		checkPendingMatchesRecompute(t, j, m, 5)
		if got := offered(t, j); !slices.Equal(got, []int32{0, 1, 2, 3, 4}) {
			t.Errorf("offered %v, want every article: the released volume's are owed", got)
		}
	})
	t.Run("the verdict discards a held volume", func(t *testing.T) {
		t.Parallel()
		j, m := newHeldVolumeJob(t)
		if err := j.SetFileFetchPolicy(1, FetchIfNeeded); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		if err := j.MarkArticleEmitted(0); err != nil {
			t.Fatalf("MarkArticleEmitted: %v", err)
		}
		if !j.DiscardDeferredPar2() {
			t.Fatal("DiscardDeferredPar2 reported no change")
		}
		checkPendingMatchesRecompute(t, j, m, 1)
	})
	t.Run("the verdict releases a held volume", func(t *testing.T) {
		t.Parallel()
		j, m := newHeldVolumeJob(t)
		if err := j.SetFileFetchPolicy(1, FetchIfNeeded); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		if err := j.UndeferRecoveryVolumes([]int{1}); err != nil {
			t.Fatalf("UndeferRecoveryVolumes: %v", err)
		}
		checkPendingMatchesRecompute(t, j, m, 5)
	})
}

// TestFetchPolicyDoors_OneResidencyRule pins the other half of #903: the
// release and the discard are both progress-tier writes, so each takes effect
// on an evicted job, and the re-hydration then offers what the policy says.
func TestFetchPolicyDoors_OneResidencyRule(t *testing.T) {
	t.Parallel()
	t.Run("release", func(t *testing.T) {
		t.Parallel()
		j, m := newHeldVolumeJob(t)
		if err := j.SetFileFetchPolicy(1, FetchIfNeeded); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		j.Evict()
		if err := j.UndeferRecoveryVolumes([]int{1}); err != nil {
			t.Fatalf("UndeferRecoveryVolumes on an evicted job: %v", err)
		}
		if !j.Par2Recovered() {
			t.Error("Par2Recovered = false after the release")
		}
		if err := j.RestoreContent(m); err != nil {
			t.Fatalf("RestoreContent: %v", err)
		}
		checkPendingMatchesRecompute(t, j, m, 5)
		if got := offered(t, j); !slices.Equal(got, []int32{0, 1, 2, 3, 4}) {
			t.Errorf("offered %v, want every article", got)
		}
	})
	t.Run("discard", func(t *testing.T) {
		t.Parallel()
		j, m := newHeldVolumeJob(t)
		if err := j.SetFileFetchPolicy(1, FetchIfNeeded); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
		j.Evict()
		if !j.DiscardDeferredPar2() {
			t.Fatal("DiscardDeferredPar2 on an evicted job reported no change")
		}
		if err := j.RestoreContent(m); err != nil {
			t.Fatalf("RestoreContent: %v", err)
		}
		checkPendingMatchesRecompute(t, j, m, 2)
		if got := offered(t, j); !slices.Equal(got, []int32{0, 1}) {
			t.Errorf("offered %v, want the content file's articles only", got)
		}
	})
	t.Run("out of range", func(t *testing.T) {
		t.Parallel()
		j, _ := newHeldVolumeJob(t)
		j.Evict()
		if err := j.UndeferRecoveryVolumes([]int{2}); err == nil {
			t.Error("UndeferRecoveryVolumes accepted file index 2 of a two-file job")
		}
	})
}

// TestRestoreContent_RefusesADifferentFileShape pins that the progress tier's
// own copy of the file ranges cannot disagree with the manifest it is paired
// with: equal totals split differently between the files are refused.
func TestRestoreContent_RefusesADifferentFileShape(t *testing.T) {
	t.Parallel()
	j, m := newHeldVolumeJob(t)
	j.Evict()
	other := NewManifest([]JobFile{
		{Subject: `"a.rar" yEnc (1/3)`, Bytes: 300, Articles: []JobArticle{
			{ID: "<a1@x>", Bytes: 100, Number: 1}, {ID: "<a2@x>", Bytes: 100, Number: 2}, {ID: "<a3@x>", Bytes: 100, Number: 3},
		}},
		{Subject: `"a.vol0+1.par2" yEnc (1/2)`, Bytes: 200, IsPar2Recovery: true, Articles: []JobArticle{
			{ID: "<p1@x>", Bytes: 100, Number: 1}, {ID: "<p2@x>", Bytes: 100, Number: 2},
		}},
	})
	if other.NumArticles() != m.NumArticles() || other.NumFiles() != m.NumFiles() {
		t.Fatal("precondition: the two manifests must agree on both totals")
	}
	if err := j.RestoreContent(other); err == nil {
		t.Error("RestoreContent accepted a manifest whose files split the articles differently")
	}
}

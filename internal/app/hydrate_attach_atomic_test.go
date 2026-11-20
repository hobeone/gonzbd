package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

// A hydrating job becomes resident and has its verified articles Done in one
// step (#1066). The downloader reads a resident job through
// ForEachUnfinishedArticle; a reader that found the job resident before the
// read-back's rows were installed would hand a verified article out again, and
// its copy, refused against the bytes on disk, would count toward the file's
// parts and finish the file short.
//
// The observer reads the job the way the downloader does, at the first instant
// AttachArticles has released the content lock: the first moment any reader can
// see it. Its first two articles are recorded and on disk, so an attach that
// installed nothing, or left the install for a second step, lists article 0 or
// 1 at that instant.
func TestVerifyAndAttach_NoReaderSeesAVerifiedArticleUnfinished(t *testing.T) {
	t.Parallel()
	env := newLREnv(t)
	a := env.newApp(t)
	j0 := a.addJob(t, "atomic", 3, 2)
	env.writeFileA(t, j0, 3, 0, 1)
	a.recordFileA(t, j0.ID(), false, 0, 1)
	m, err := j0.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	fresh := job.New(j0.ID(), j0.Name(), job.Policy{})
	var observed int
	var listed [][]int32
	fresh.SetAttachObserver(func() {
		observed++
		var unfinished []int32
		// The observer runs only after an attach, so the job is resident and the
		// listing cannot be refused; an error would leave this round empty and
		// the assertions below passing without having read anything.
		err := fresh.ForEachUnfinishedArticle(func(_ int, artIdx int32, _ string, _, _ int, _ string) bool {
			unfinished = append(unfinished, artIdx)
			return true
		})
		if err != nil {
			t.Errorf("a reader at attach %d could not list the job's articles: %v", observed-1, err)
		}
		listed = append(listed, unfinished)
	})
	if err := a.verifyFromRecord(t, fresh, m); err != nil {
		t.Fatalf("verifyAndAttach: %v", err)
	}
	if !fresh.Resident() {
		t.Fatal("verifyAndAttach returned nil but the job is not resident")
	}
	if observed == 0 {
		t.Fatal("hydration never attached a manifest, so no reader instant was observed")
	}
	for i, unfinished := range listed {
		for _, art := range unfinished {
			if art == 0 || art == 1 {
				t.Fatalf("a reader at attach %d listed article %d as unfinished; "+
					"its bytes were verified on disk, so the downloader would fetch it again", i, art)
			}
		}
	}
}

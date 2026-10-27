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
// The reader here does what the downloader does, from the first instant the
// job reads as resident. Each round hydrates a fresh job whose first two
// articles are recorded and on disk, so a round in which the attach and the
// install were separate steps gives the reader a window to list article 0 or 1.
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

	const rounds = 300
	for round := range rounds {
		hydrateWithReader(t, a, m, j0.ID(), j0.Name(), round)
	}
}

// hydrateWithReader runs one round of
// TestVerifyAndAttach_NoReaderSeesAVerifiedArticleUnfinished. The reader stops
// when the round ends, whatever ended it, so a failed round leaves no
// goroutine spinning.
func hydrateWithReader(t *testing.T, a *lrApp, m *job.Manifest, id, name string, round int) {
	t.Helper()
	fresh := job.New(id, name, job.Policy{})
	stop := make(chan struct{})
	defer close(stop)
	seen := make(chan []int32, 1)
	go func() {
		for !fresh.Resident() { // a spin is the point: the reader must arrive at the first resident instant
			select {
			case <-stop:
				return
			default:
			}
		}
		var listed []int32
		_ = fresh.ForEachUnfinishedArticle(func(_ int, artIdx int32, _ string, _, _ int, _ string) bool { //nolint:errcheck // resident above, and nothing evicts it
			listed = append(listed, artIdx)
			return true
		})
		seen <- listed
	}()
	if err := a.verifyFromRecord(t, fresh, m); err != nil {
		t.Fatalf("round %d: verifyAndAttach: %v", round, err)
	}
	if !fresh.Resident() {
		t.Fatalf("round %d: verifyAndAttach returned nil but the job is not resident", round)
	}
	for _, art := range <-seen {
		if art == 0 || art == 1 {
			t.Fatalf("round %d: a reader that found the job resident listed article %d as unfinished; "+
				"its bytes were verified on disk, so the downloader would fetch it again", round, art)
		}
	}
}

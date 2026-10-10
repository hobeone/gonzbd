//go:build crash && linux

package crash

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// killFixture is the shape both SIGKILL tests use: slow enough that the
// recorder's 5 s flush lands with most of the file still to fetch, so a kill
// after it keeps recorded work and risks unrecorded work.
func killFixture() harnessOpts {
	return harnessOpts{
		Connections: 1,
		BodyDelay:   60 * time.Millisecond,
		Files:       []fileSpec{{Name: "payload.bin", Size: 16 << 20, PartSize: 128 << 10}},
	}
}

// TestSIGKILL_NoArticleIsResolvedWithoutItsBytes pins that a row in
// written_articles is never ahead of its bytes, and that the restarted daemon
// resolves an article only on bytes that hash to its row.
//
// The kill is what gives the check teeth. A SIGKILL destroys the process's
// in-memory buffers with no flush, so a row recorded before its bytes left the
// process has no bytes in the file afterwards — and this test reads the file,
// with the daemon dead and from its own database.
//
// What it does NOT check is that a byte reached the platter; see the package
// doc. The claim under test here is the process-boundary half.
func TestSIGKILL_NoArticleIsResolvedWithoutItsBytes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, killFixture())
	jobID := h.AddJob()

	// Grounding: the kill must land after a flush recorded real work and while
	// more was unrecorded.
	h.WaitForRecordedBacklog(jobID, 8, 3)

	h.KillAndDropPageCache()
	servedAtKill := h.Server.ArticlesServed()

	db := h.openDB()
	files := h.JobFiles(db, jobID)
	rows := h.Rows(db, jobID)
	recorded := 0
	for _, rs := range rows {
		recorded += len(rs)
	}
	if recorded == 0 {
		t.Fatal("nothing was recorded at kill time — the fixture never reached the " +
			"state this test exists to check")
	}
	if len(servedAtKill) <= recorded {
		t.Fatalf("%d articles served but %d already recorded at kill time — no "+
			"unrecorded work was at risk", len(servedAtKill), recorded)
	}

	// Every recorded row has bytes in the file that hash to its CRC.
	checked := 0
	for _, f := range files {
		if f.Filename == "" {
			t.Fatalf("file %d has no resolved filename in job_files; the partial it "+
				"wrote cannot be located, which is issue #361's shape", f.FileIdx)
		}
		path := filepath.Join(h.JobDir(), f.Filename)
		for _, r := range rows[f.FileIdx] {
			got, present := h.ReadRegionCRC(path, r.Offset, r.Length)
			if !present {
				t.Errorf("file %d article %d is recorded at [%d,%d) but %s is shorter — "+
					"a row outlived its data", f.FileIdx, r.ArtIdx, r.Offset, r.Offset+r.Length, path)
				continue
			}
			if got != r.CRC32 {
				t.Errorf("file %d article %d at [%d,%d) hashes to %#x, want %#x — a row "+
					"outlived its data", f.FileIdx, r.ArtIdx, r.Offset, r.Offset+r.Length, got, r.CRC32)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no row's bytes were actually read back")
	}

	// The restarted daemon resolves articles from those rows and the file. A
	// file it completes from a wrongly resolved article has the wrong bytes.
	h.Restart()
	h.WaitForJobComplete(jobID)
	h.AssertCompletedFilesMatch()
	t.Logf("checked %d recorded articles against their bytes; %d articles had been served",
		checked, len(servedAtKill))
}

// TestSIGKILL_NoVerifiedArticleIsFetchedAgain pins what a crash costs: an
// article recorded before the kill, whose bytes verify on restart, is never
// fetched again. Only work the kill caught unrecorded is redone.
//
// Re-fetching is measured at the wire, from the mock server's per-article
// delivery counts.
func TestSIGKILL_NoVerifiedArticleIsFetchedAgain(t *testing.T) {
	t.Parallel()
	opts := killFixture()
	h := newHarness(t, opts)
	jobID := h.AddJob()

	h.WaitForRecordedBacklog(jobID, 8, 3)
	servedBefore := h.Server.ArticlesServed()

	// Captured before the kill so the completed file can be compared against
	// it. This pins CONTINUITY — that the file which finishes is the one the
	// crashed run was writing, moved into place by a rename rather than
	// rebuilt beside it.
	partials := h.PartialPaths()
	if len(partials) != 1 {
		t.Fatalf("expected one partial file before the kill, found %v", partials)
	}
	beforeIno := inodeOf(t, partials[0])

	h.KillAndDropPageCache()

	recordedIDs := h.RecordedMessageIDs(jobID)
	if len(recordedIDs) == 0 {
		t.Fatal("no article was recorded at kill time — the assertion below would hold vacuously")
	}

	h.Restart()
	h.WaitForJobComplete(jobID)
	servedAfter := h.Server.ArticlesServed()

	var refetched []string
	for id := range recordedIDs {
		if servedAfter[id] > servedBefore[id] {
			refetched = append(refetched, id)
		}
	}
	if len(refetched) > 0 {
		t.Errorf("%d of %d articles recorded at kill time were fetched again after the "+
			"restart; their bytes were in the file: %v", len(refetched), len(recordedIDs), refetched)
	}
	rework := 0
	for id, before := range servedBefore {
		if before > 0 && servedAfter[id] > before {
			rework++
		}
	}
	t.Logf("re-fetched %d of %d articles served before the kill; %d were recorded",
		rework, len(servedBefore), len(recordedIDs))

	// #361: the resume must continue the partial it left, not orphan it and
	// start "<name>.1.<ext>". Asserted on the job's FILE SET, because under
	// #361 the original partial keeps its name and its inode while the orphan
	// is written beside it.
	wantNames := make([]string, 0, len(opts.Files))
	for _, f := range opts.Files {
		wantNames = append(wantNames, f.Name)
	}
	slices.Sort(wantNames)
	if got := h.CompletedJobFileNames(); !slices.Equal(got, wantNames) {
		t.Errorf("the completed job directory holds %v, want exactly %v — the restart "+
			"wrote a second file beside the partial it should have continued (#361)",
			got, wantNames)
	}

	// Continuity, which the file set alone does not give.
	final, err := h.findUnder(h.CompleteDir, opts.Files[0].Name)
	if err != nil {
		t.Fatalf("locate the completed file: %v", err)
	}
	if got := inodeOf(t, final); got != beforeIno {
		t.Errorf("the completed file is inode %d but the partial written before the crash "+
			"was inode %d — the restart did not resume into the file it left", got, beforeIno)
	}
	h.AssertCompletedFilesMatch()
}

// inodeOf returns a file's inode number.
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: no syscall.Stat_t", path)
	}
	return st.Ino
}

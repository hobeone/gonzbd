//go:build crash

package crash

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// killFixture is the shape both SIGKILL tests use: enough articles that a
// checkpoint window is several of them wide, and slow enough that the kill
// lands in the middle of one.
func killFixture() harnessOpts {
	return harnessOpts{
		CheckpointBytes:    1 << 20,
		CheckpointInterval: time.Hour, // the byte bound is the one under test
		WriteCacheBytes:    1 << 20,
		Connections:        1,
		BodyDelay:          8 * time.Millisecond,
		Files:              []fileSpec{{Name: "payload.bin", Size: 16 << 20, PartSize: 128 << 10}},
	}
}

// TestSIGKILL_NoArticleIsResolvedWithoutItsBytes is the end-to-end pin for
// S1 and S2: nothing may claim an article is resolved on the strength of the
// article having entered a buffer, a channel or the write cache.
//
// The kill is what gives the check teeth. A SIGKILL destroys the assembler's
// write cache with no flush, so an article acked before its bytes left the
// process has no bytes in the file afterwards — and this test reads the file.
// It reads it with the daemon dead and from its own database, because after a
// crash the database is the only record of what was CLAIMED and the file is
// the only record of what is actually there.
//
// What it does NOT check is that an fsync'd byte reached the platter; see the
// package doc. The claim under test here is the process-boundary half.
func TestSIGKILL_NoArticleIsResolvedWithoutItsBytes(t *testing.T) {
	h := newHarness(t, killFixture())
	jobID := h.AddJob()

	// Grounding: the kill must land after real work was acked and while more
	// was still in flight. Both halves are asserted below; waiting here is
	// what makes the first of them true rather than hoped for.
	h.WaitForDurableBytes(jobID, 4<<20)
	// ... and then wait for the daemon to get ahead of its own barrier again,
	// so the kill lands mid-window. See WaitForUnackedBacklog.
	h.WaitForUnackedBacklog(jobID, 3)

	h.KillAndDropPageCache()
	// Read after the kill, not before: a body still being written when the
	// process died is not counted (the write fails), so this cannot credit
	// the daemon with an article it never received.
	servedAtKill := h.Server.ArticlesServed()

	db := h.openDB()
	files := h.JobFiles(db, jobID)
	if len(files) != len(h.opts.Files) {
		t.Fatalf("job_files has %d rows, want %d", len(files), len(h.opts.Files))
	}
	runs := h.Runs(db, jobID)
	durable := h.DurableOrdinals(jobID)

	// Grounding 1: the record actually claims something. An empty claim set
	// would make every assertion below vacuously true, which is how a harness
	// comes to certify a property it never checked.
	claimed := 0
	for _, f := range files {
		for i := range f.ArticleCount {
			if f.Done[i] && !f.Failed[i] {
				claimed++
				continue
			}
			if bits := durable[f.FileIdx]; i < len(bits) && bits[i] {
				claimed++
			}
		}
	}
	if claimed == 0 {
		t.Fatal("nothing was claimed resolved or durable at kill time — the fixture " +
			"never reached the state this test exists to check")
	}

	// Grounding 2: the kill landed MID-window. If every article the server
	// served had already been acked, the write cache was empty when the
	// process died and the check below could not have caught an over-claim.
	if len(servedAtKill) <= claimed {
		t.Fatalf("%d articles served but %d already claimed at kill time — the kill "+
			"did not land inside a checkpoint window, so no unacked bytes were at risk",
			len(servedAtKill), claimed)
	}

	// The check itself: every recorded run must have bytes in the file that
	// hash to the CRC the record holds for it.
	//
	// The unit is a RUN rather than an article, and that is the whole of what
	// the durable-runs change did to this assertion. A run's crc32 is combined
	// left-to-right over the articles it covers, so checking it checks every
	// one of them at once — and there is no longer a per-article CRC to check
	// individually.
	//
	// It is also strictly stronger than the check it replaced. That one could
	// only compare an article's bytes against a Class A fact appended at
	// DECODE time, which asserted nothing about presence; a run is written
	// only after the fsync, so a mismatch here is an over-claim rather than an
	// unproven claim. The "done in the queue but no fact recorded" arm the old
	// version needed is gone with the class: an article the queue calls done
	// is one a run covers, by construction — that IS the derivation.
	checked := 0
	for _, f := range files {
		if f.Filename == "" {
			t.Fatalf("file %d has no resolved filename in job_files; the partial it "+
				"wrote cannot be located, which is issue #361's shape", f.FileIdx)
		}
		path := filepath.Join(h.JobDir(), f.Filename)
		for _, r := range runs[f.FileIdx] {
			got, present := h.ReadRegionCRC(path, r.Offset, r.Length)
			if !present {
				t.Errorf("file %d has a durable run over articles [%d,%d] but %s is "+
					"shorter than its range [%d,%d) — a claim outlived its data",
					f.FileIdx, r.FirstArtIdx, r.LastArtIdx, path, r.Offset, r.Offset+r.Length)
				continue
			}
			if got != r.CRC32 {
				t.Errorf("file %d's durable run over articles [%d,%d] at [%d,%d) hashes "+
					"to %#x, want %#x — a claim outlived its data",
					f.FileIdx, r.FirstArtIdx, r.LastArtIdx, r.Offset, r.Offset+r.Length,
					got, r.CRC32)
			}
			checked += int(r.LastArtIdx-r.FirstArtIdx) + 1
		}
	}
	if checked == 0 {
		t.Fatal("no run's bytes were actually read back; the assertions above " +
			"never ran against a file")
	}
	t.Logf("checked %d claimed articles against their bytes; %d articles had been served",
		checked, len(servedAtKill))
}

// TestSIGKILL_ReworkStaysWithinTheCheckpointBound is the pin for B1 and L3,
// and for the half of them that matters most: the bound limits how much
// UNACKED work a crash costs, and how much ACKED work it costs is not bounded
// but zero.
//
// Rework is measured at the wire, from the mock server's per-article delivery
// counts — not from a status field, and not from elapsed time. An article is
// rework if and only if the restarted daemon fetched it again having already
// fetched it before the kill.
func TestSIGKILL_ReworkStaysWithinTheCheckpointBound(t *testing.T) {
	opts := killFixture()
	h := newHarness(t, opts)
	jobID := h.AddJob()

	h.WaitForDurableBytes(jobID, 4<<20)
	// The kill must land mid-window or there is nothing to redo and the bound
	// assertion below measures nothing. The first version of this test omitted
	// this and passed on one run and failed on the next, on the "no article was
	// re-fetched at all" grounding — the flake was the fixture, not the daemon.
	h.WaitForUnackedBacklog(jobID, 3)
	servedBefore := h.Server.ArticlesServed()

	// Captured before the kill so the completed file can be compared against
	// it. This pins CONTINUITY — that the file which finishes is the one the
	// crashed run was writing, moved into place by a rename rather than
	// rebuilt beside it. It is NOT the #361 pin; see the file-set assertion at
	// the end of this test for why it cannot be.
	partials := h.PartialPaths()
	if len(partials) != 1 {
		t.Fatalf("expected one partial file before the kill, found %v", partials)
	}
	beforeIno := inodeOf(t, partials[0])

	h.KillAndDropPageCache()

	// The durable set as stable storage recorded it. This is what may not be
	// re-fetched at any cost.
	durableIDs := h.DurableMessageIDs(jobID)
	if len(durableIDs) == 0 {
		t.Fatal("no article was durable at kill time — the zero-loss assertion below " +
			"would hold vacuously")
	}

	h.Restart()
	h.WaitForJobComplete(jobID)
	servedAfter := h.Server.ArticlesServed()

	// Zero tolerance: an article whose bytes a completed fsync covered is
	// work the design promises a crash does not cost.
	var lostAck []string
	for id := range durableIDs {
		if servedAfter[id] > servedBefore[id] {
			lostAck = append(lostAck, id)
		}
	}
	if len(lostAck) > 0 {
		t.Errorf("%d of %d articles that were durable at kill time were fetched again "+
			"after the restart — the bound limits unacked rework, acked rework must be "+
			"zero: %v", len(lostAck), len(durableIDs), lostAck)
	}

	// The bound itself, over everything that was re-fetched.
	rework := 0
	for id, before := range servedBefore {
		if before > 0 && servedAfter[id] > before {
			rework++
		}
	}
	reworkBytes := int64(rework) * int64(h.ArticleSize)

	// Every term is memory the SIGKILL destroyed or work it interrupted, and
	// each is a real cost of the crash rather than slack invented to make the
	// assertion pass:
	//   - CheckpointBytes: B1's own bound, the bytes written since the window
	//     opened.
	//   - WriteCacheBytes: the assembler's coalescing buffer, in-process
	//     memory that dies with the process.
	//   - Connections * ArticleSize: articles in flight on the wire, served
	//     by the mock but never delivered to the assembler.
	bound := opts.CheckpointBytes + opts.WriteCacheBytes + int64(opts.Connections)*int64(h.ArticleSize)
	if reworkBytes > bound {
		t.Errorf("re-fetched %d bytes after the crash, bound is %d (checkpoint %d + write cache %d "+
			"+ %d in flight) — B1 is violated",
			reworkBytes, bound, opts.CheckpointBytes, opts.WriteCacheBytes,
			int64(opts.Connections)*int64(h.ArticleSize))
	}
	// Grounding: with no rework at all the bound assertion says nothing, and
	// a fixture whose kill landed between windows would produce exactly that.
	if rework == 0 {
		t.Error("no article was re-fetched at all — either the kill landed on a window " +
			"boundary or the restart resumed from something it should not trust; " +
			"either way the bound above was not measured")
	}
	t.Logf("re-fetched %d of %d articles (%d bytes, bound %d); %d were durable at kill time, %d of those were re-fetched",
		rework, len(servedBefore), reworkBytes, bound, len(durableIDs), len(lostAck))

	// #361: the resume must continue the partial it left, not orphan it and
	// start "<name>.1.<ext>".
	//
	// Asserted on the job's FILE SET, because the defect's shape is the
	// opposite of the obvious guess and the obvious assertion cannot fail on
	// it. Under #361 the NEW file is the orphan: the original partial keeps
	// its name and its inode and is the one that completes, while
	// "<name>.1.<ext>" is written beside it and abandoned. Probed with the fix
	// (775873ad) reverted — the completed job directory held payload.bin AND
	// payload.1.bin, both 16 MiB, with payload.bin's inode unchanged. An
	// identity check on the surviving file is therefore satisfied BY the
	// defect, which is what the first draft of this test asserted.
	//
	// The whole job directory is renamed into the complete directory by
	// post-processing, so an orphan travels with it and is visible there with
	// no race against the job finishing.
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

	// Continuity, which the file set alone does not give: the file that
	// finished is the one the crashed run was writing, moved by a rename
	// rather than rebuilt. A regression that copied the bytes into a fresh
	// inode would leave the file set correct and fail here.
	final, err := h.findUnder(h.CompleteDir, opts.Files[0].Name)
	if err != nil {
		t.Fatalf("locate the completed file: %v", err)
	}
	if got := inodeOf(t, final); got != beforeIno {
		t.Errorf("the completed file is inode %d but the partial written before the crash "+
			"was inode %d — the restart did not resume into the file it left", got, beforeIno)
	}
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

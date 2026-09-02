package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
)

// This file used to test recordPar2Names, which applied par2's renames during
// download and wrote each new location to JobProgress.Filename. That function
// is gone, and so are its tests.
//
// It existed so that verification — which matched par2 entries by NAME — would
// have corrected names to work with. Verification is content-based now and
// runs before anything moves, so the rename bought the verdict nothing, while
// costing a real defect: JobProgress.Filename cannot hold a path, so a file
// relocated into a subdirectory could not be recorded truthfully, and the
// startup resume sweep then stat'ed a top-level path that does not exist.
// durability.Resume reads a missing file as disproof of every run it holds and
// re-downloads a file that was already complete.
//
// Relocation belongs to post-processing, which is where it was before and
// where stage_quickcheck still does it, ahead of the repair stage that needs
// files at their par2 paths. The idempotency property those tests pinned is
// pinned at the level that owns it now:
// par2.TestIdentify_FindsAnEntryAlreadyAtItsPar2Path.

// TestResolvedName pins the precedence every par2 call site uses: the recorded
// on-disk name wins over the NZB subject, and the subject is the fallback
// until one is recorded.
//
// The precedence is what lets assembledFiles describe a file the assembler
// wrote under its yEnc name rather than its subject, which is the ordinary
// obfuscated case — and it is the reason the verdict can be taken against
// names that are true without anything having been renamed.
func TestResolvedName(t *testing.T) {
	t.Parallel()

	const jobID = "resolved-name-job"
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "subject-name.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}

	snap := q.SnapshotJob(jobID)
	m, err := snap.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	if got := resolvedName(m, snap.Progress(), 0); got != "subject-name.bin" {
		t.Errorf("with nothing recorded, resolvedName = %q, want the subject", got)
	}

	if err := q.SetFileFilename(jobID, 0, "actual-on-disk.bin"); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	after := q.SnapshotJob(jobID)
	if got := resolvedName(m, after.Progress(), 0); got != "actual-on-disk.bin" {
		t.Errorf("with a name recorded, resolvedName = %q, want the recorded name", got)
	}
}

// TestAssembledFiles pins what the queue hands par2: the name each file
// currently has on disk, paired with the CRC recorded for it.
//
// The pairing is the point. A file's CRC is looked up by the name par2
// identification returns, so a list that carried the subject where a resolved
// name exists would hand par2 a name nothing on disk answers to, and every
// obfuscated file would come back unverified.
func TestAssembledFiles(t *testing.T) {
	t.Parallel()

	const jobID = "assembled-files-job"
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "subject-name.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.SetFileFilename(jobID, 0, "7xq6N6P340dCh9Lnih5hY3jsArfSN1"); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	seedFileCRC(t, q, qjob, 0, 0x1068AFA6)

	snap := q.SnapshotJob(jobID)
	m, err := snap.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	files := assembledFiles(m, snap.Progress())
	if len(files) != m.NumFiles() {
		t.Fatalf("got %d assembled files, want %d — one per manifest file", len(files), m.NumFiles())
	}
	if files[0].FileName != "7xq6N6P340dCh9Lnih5hY3jsArfSN1" {
		t.Errorf("FileName = %q, want the recorded on-disk name rather than the subject", files[0].FileName)
	}
	if files[0].CRC32 != 0x1068AFA6 {
		t.Errorf("CRC32 = %08x, want 1068afa6", files[0].CRC32)
	}
	// The par2 volume was never downloaded, so it carries no CRC — which is
	// "unavailable", and must not be confused with a CRC that happens to be 0.
	if files[1].CRC32 != 0 {
		t.Errorf("CRC32 = %08x for a file with no recorded CRC, want 0", files[1].CRC32)
	}
}

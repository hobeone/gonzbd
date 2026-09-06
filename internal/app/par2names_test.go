package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// TestResolvedName pins the precedence every par2 call site uses: the recorded
// on-disk name wins over the NZB subject, and the subject is the fallback
// until one is recorded.
func TestResolvedName(t *testing.T) {
	t.Parallel()

	m := job.NewManifest([]job.JobFile{
		{Subject: "subject-name.bin", Bytes: 100, Articles: []job.JobArticle{{ID: "<1@x>", Bytes: 100}}},
		{Subject: "data.vol000+01.par2", Bytes: 100, IsPar2Recovery: true, Articles: []job.JobArticle{{ID: "<2@x>", Bytes: 100}}},
	})
	j := job.New("resolved-name-job", "resolved-name-job", job.Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	if got := resolvedName(m, j.Progress(), 0); got != "subject-name.bin" {
		t.Errorf("with nothing recorded, resolvedName = %q, want the subject", got)
	}

	if err := j.SetFileFilename(0, "actual-on-disk.bin"); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	if got := resolvedName(m, j.Progress(), 0); got != "actual-on-disk.bin" {
		t.Errorf("with a name recorded, resolvedName = %q, want the recorded name", got)
	}
}

// TestAssembledFiles pins what the queue hands par2: the name each file
// currently has on disk, paired with the CRC recorded for it.
func TestAssembledFiles(t *testing.T) {
	t.Parallel()

	m := job.NewManifest([]job.JobFile{
		{Subject: "subject-name.bin", Bytes: 100, Articles: []job.JobArticle{{ID: "<1@x>", Bytes: 100}}},
		{Subject: "data.vol000+01.par2", Bytes: 100, IsPar2Recovery: true, Articles: []job.JobArticle{{ID: "<2@x>", Bytes: 100}}},
	})
	j := job.New("assembled-files-job", "assembled-files-job", job.Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	if err := j.SetFileFilename(0, "7xq6N6P340dCh9Lnih5hY3jsArfSN1"); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	runs := []durability.Run{
		{FileIdx: 0, FirstArtIdx: 0, LastArtIdx: 0, Offset: 0, Length: 100, CRC32: 0x1068AFA6},
	}
	if _, err := j.SetFileCRC32FromRuns(0, runs); err != nil {
		t.Fatalf("SetFileCRC32FromRuns: %v", err)
	}

	files := assembledFiles(m, j.Progress())
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

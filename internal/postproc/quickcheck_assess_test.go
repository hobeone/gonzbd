package postproc

import (
	"crypto/md5" //nolint:gosec // par2 records MD5; matching it is the point
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/par2"
)

// TestQuickCheckStage_Assess covers the stage's own translation step: turning
// what the queue knows into the assembled-file list par2.Assess consumes.
//
// The interesting case is the one with no manifest. Assessment still has to
// happen — identification and the relocations it implies do not depend on the
// queue at all, and a job whose manifest cannot be read still wants its files
// put where par2 expects them. Skipping assessment there would leave those
// files unmoved AND leave par2 repair unable to find them, turning a missing
// manifest into a failed job rather than an unverified one.
func TestQuickCheckStage_Assess(t *testing.T) {
	t.Parallel()

	newDir := func(t *testing.T) (string, []par2.Set, []byte) {
		t.Helper()
		dir := t.TempDir()
		content := []byte("payload for the assess translation test")
		hash16k := md5.Sum(content) //nolint:gosec // par2 compatibility
		size := uint64(len(content))

		setID := [16]byte{5, 5, 5}
		fileID := [16]byte{77, 88}
		par2Bytes := buildPacket(setID, typeMain, buildMainBody(size, fileID))
		par2Bytes = append(par2Bytes, buildPacket(setID, typeFileDesc,
			buildFileDescBody(fileID, [16]byte{0xCC}, hash16k, size, "payload.bin"))...)
		par2Bytes = append(par2Bytes, buildPacket(setID, typeIFSC,
			buildIFSCBody(fileID, hash16k, crc32.ChecksumIEEE(content)))...)
		if err := os.WriteFile(filepath.Join(dir, "set.par2"), par2Bytes, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "payload.bin"), content, 0o600); err != nil {
			t.Fatal(err)
		}

		sets, err := par2.FindPar2Files(dir)
		if err != nil || len(sets) == 0 {
			t.Fatalf("FindPar2Files: %v (%d sets)", err, len(sets))
		}
		return dir, sets, content
	}

	t.Run("carries the queue's CRCs through to verification", func(t *testing.T) {
		t.Parallel()
		dir, sets, content := newDir(t)
		q := &QuickCheckStage{}
		job := &Job{
			Job:         buildQCJob(t, "assess-ok", "payload.bin", int64(len(content)), crc32.ChecksumIEEE(content)),
			DownloadDir: dir,
		}

		a, err := q.assess(job, sets, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("assess: %v", err)
		}
		if a.CRC.Matched != 1 {
			t.Errorf("Matched = %d, want 1: the CRC the queue holds never reached verification", a.CRC.Matched)
		}
	})

	t.Run("assesses a job with no queue at all", func(t *testing.T) {
		t.Parallel()
		dir, sets, _ := newDir(t)
		q := &QuickCheckStage{}

		a, err := q.assess(&Job{DownloadDir: dir}, sets, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("assess: %v", err)
		}
		// Identification still ran, so par2 repair will find the file.
		if !a.ID.Accounted() {
			t.Errorf("%d entr(y/ies) unaccounted; identification does not need the queue and must not be skipped "+
				"with it", len(a.ID.Unaccounted))
		}
		// But nothing was verified, because there were no CRCs to verify with.
		if a.CRC.Checked != 0 {
			t.Errorf("Checked = %d, want 0: there is no CRC to check against", a.CRC.Checked)
		}
		if a.CRC.Unverified != 1 {
			t.Errorf("Unverified = %d, want 1: an identified file the caller cannot speak to is unverified, "+
				"which is what leaves the stage inconclusive", a.CRC.Unverified)
		}
	})
}

package postproc

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/job/jobtest"
)

// buildQCJob builds a single-file *job.Job whose file has a resolved
// filename, byte count, and assembled CRC32 already set — the shape
// recordVerdict reads via Manifest/Progress accessors.
func buildQCJob(t *testing.T, id, filename string, bytes int64, crc uint32) *job.Job {
	t.Helper()
	return qcJob(t, id, filename, bytes, crc, false)
}

// buildQCJobHolding is buildQCJob with a second file, a recovery volume the
// job still holds back (FetchIfNeeded), so that HasDeferredPar2 reports true.
func buildQCJobHolding(t *testing.T, id, filename string, bytes int64, crc uint32) *job.Job {
	t.Helper()
	return qcJob(t, id, filename, bytes, crc, true)
}

func qcJob(t *testing.T, id, filename string, bytes int64, crc uint32, held bool) *job.Job {
	t.Helper()
	j := job.New(id, id+".nzb", job.Policy{})
	files := []job.JobFile{{
		Subject:  "subject.bin",
		Bytes:    bytes,
		Articles: []job.JobArticle{{ID: id + "-a@t", Bytes: int(bytes)}},
	}}
	if held {
		files = append(files, job.JobFile{
			Subject:  "held.vol000+01.par2",
			Bytes:    1,
			Articles: []job.JobArticle{{ID: id + "-v@t", Bytes: 1}},
		})
	}
	m := job.NewManifest(files)
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if err := j.RecordFileFilename(0, j.Name(), filename); err != nil {
		t.Fatalf("RecordFileFilename: %v", err)
	}
	if crc != 0 {
		jobtest.SeedFileCRC(t, j, 0, crc)
	}
	if held {
		if err := j.SetFileFetchPolicy(1, job.FetchIfNeeded); err != nil {
			t.Fatalf("SetFileFetchPolicy: %v", err)
		}
	}
	return j
}

var (
	typeMain     = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'M', 'a', 'i', 'n', 0x00, 0x00, 0x00, 0x00}
	typeFileDesc = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'F', 'i', 'l', 'e', 'D', 'e', 's', 'c'}
	typeIFSC     = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', 0x00, 'I', 'F', 'S', 'C', 0x00, 0x00, 0x00, 0x00}
)

func buildPacket(setID [16]byte, pktType [16]byte, body []byte) []byte {
	packetLen := uint64(64 + len(body))
	buf := make([]byte, packetLen)
	copy(buf[0:8], "PAR2\x00PKT")
	binary.LittleEndian.PutUint64(buf[8:16], packetLen)
	copy(buf[32:48], setID[:])
	copy(buf[48:64], pktType[:])
	copy(buf[64:], body)

	h := md5.New()
	h.Write(buf[32:48])
	h.Write(buf[48:64])
	h.Write(body)
	copy(buf[16:32], h.Sum(nil))

	return buf
}

func buildMainBody(sliceSize uint64, fileIDs ...[16]byte) []byte {
	body := make([]byte, 12+len(fileIDs)*16)
	binary.LittleEndian.PutUint64(body[0:8], sliceSize)
	// Leave recoverySetCount (body[8:12]) as 0.
	for i, id := range fileIDs {
		copy(body[12+i*16:12+(i+1)*16], id[:])
	}
	return body
}

func buildFileDescBody(fileID [16]byte, fullHash [16]byte, hash16k [16]byte, fileSize uint64, fileName string) []byte {
	nameBytes := []byte(fileName)
	pad := (4 - (len(nameBytes) % 4)) % 4
	body := make([]byte, 16+16+16+8, 16+16+16+8+len(nameBytes)+pad)
	copy(body[0:16], fileID[:])
	copy(body[16:32], fullHash[:])
	copy(body[32:48], hash16k[:])
	binary.LittleEndian.PutUint64(body[48:56], fileSize)
	body = append(body, nameBytes...)
	for range pad {
		body = append(body, 0)
	}
	return body
}

func buildIFSCBody(fileID [16]byte, sliceMD5 [16]byte, sliceCRC uint32) []byte {
	body := make([]byte, 16+20)
	copy(body[0:16], fileID[:])
	copy(body[16:32], sliceMD5[:])
	binary.LittleEndian.PutUint32(body[32:36], sliceCRC)
	return body
}

func TestQuickCheckStage_Disabled(t *testing.T) {
	// A dummy reference to the CRC-verifying method stood here, under no
	// comment, satisfying check_test_alignment without testing anything —
	// the pattern AGENTS.md forbids by name. It is deleted rather than
	// re-pointed at the method's replacement; recordVerdict is covered by
	// TestQuickCheckStage_VerifiesBeforeRenaming and the manifest-absent
	// tests, which call it and assert on what it recorded.

	stage := NewQuickCheckStage()
	stage.SetEnabled(false)

	job := &Job{
		Job: newQueueJob(t, "test-disabled", 0),
	}

	err := stage.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	linesStr := strings.Join(job.OutputLines, "\n")
	if !strings.Contains(linesStr, "Disabled — par2 repair will run") {
		t.Errorf("expected disabled log message, got: %v", job.OutputLines)
	}
}

func TestQuickCheckStage_NoPar2(t *testing.T) {
	stage := NewQuickCheckStage()
	stage.SetEnabled(true)

	job := &Job{
		Job:         newQueueJob(t, "test-no-par2", 0),
		DownloadDir: t.TempDir(),
	}

	err := stage.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	linesStr := strings.Join(job.OutputLines, "\n")
	if !strings.Contains(linesStr, "No par2 files found") {
		t.Errorf("expected no par2 files log message, got: %v", job.OutputLines)
	}
}

func TestQuickCheckStage_Run(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	setID := [16]byte{1, 2, 3, 4}
	fileID := [16]byte{10, 20, 30}
	fullHash := [16]byte{0xAA, 0xBB, 0xCC}

	content := []byte("flat file content for quickcheck stage test")
	hash16k := md5.Sum(content)
	fileName := "Subdir/original.txt"

	sliceSize := uint64(len(content))

	// Build par2 file packets.
	mainPkt := buildPacket(setID, typeMain, buildMainBody(sliceSize, fileID))
	fdPkt := buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, fullHash, hash16k, sliceSize, fileName))
	ifscPkt := buildPacket(setID, typeIFSC, buildIFSCBody(fileID, hash16k, crc32.ChecksumIEEE(content)))

	parContent := make([]byte, 0, len(mainPkt)+len(fdPkt)+len(ifscPkt))
	parContent = append(parContent, mainPkt...)
	parContent = append(parContent, fdPkt...)
	parContent = append(parContent, ifscPkt...)

	if err := os.WriteFile(parPath, parContent, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create flat file.
	flatName := "Subdir_original.txt"
	if err := os.WriteFile(filepath.Join(tmpDir, flatName), content, 0o644); err != nil {
		t.Fatal(err)
	}

	stage := NewQuickCheckStage()
	stage.SetEnabled(true)

	// The queue records the name the file actually has on disk.
	//
	// It recorded "original.txt" here — par2's basename — while the delivered
	// file was "Subdir_original.txt". Production cannot produce that pair:
	// JobProgress.Filename is written from the yEnc header at registration,
	// which is the name the assembler writes the file under. The old
	// verification tolerated it only because it matched the caller's name
	// against par2's BASENAME index rather than against the file on disk, so
	// any name resembling the par2 entry's basename matched whether or not it
	// was what had been delivered.
	//
	// Assess joins the caller's name to the file identification found, so the
	// two must be the same file. When they are not, the entry reads as
	// unverified and the job goes to par2 — the conservative direction.
	job := &Job{
		Job:         buildQCJob(t, "job-qc", flatName, int64(len(content)), crc32.ChecksumIEEE(content)),
		DownloadDir: tmpDir,
	}

	err := stage.Run(t.Context(), job)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if job.quickCheck != QuickCheckClean {
		t.Errorf("QuickCheck = %s, want clean", job.quickCheck)
	}

	linesStr := strings.Join(job.OutputLines, "\n")
	if !strings.Contains(linesStr, "Subdir_original.txt → Subdir/original.txt") {
		t.Errorf("expected relocation line in output, got: %s", linesStr)
	}
	if !strings.Contains(linesStr, "1/1 par2-tracked files verified OK") {
		t.Errorf("expected CRC OK verification line in output, got: %s", linesStr)
	}

	wantVerifiedLine := fmt.Sprintf("%s: CRC verified (%08x)", "original.txt", crc32.ChecksumIEEE(content))
	if !strings.Contains(linesStr, wantVerifiedLine) {
		t.Errorf("expected per-file verified line %q in output, got: %s", wantVerifiedLine, linesStr)
	}
}

func TestQuickCheckStage_CRCErrors(t *testing.T) {
	t.Run("CRC Mismatch", func(t *testing.T) {
		tmpDir := t.TempDir()
		parPath := filepath.Join(tmpDir, "test.par2")

		setID := [16]byte{1, 2, 3, 4}
		fileID := [16]byte{10, 20, 30}
		fullHash := [16]byte{0xAA, 0xBB, 0xCC}

		content := []byte("flat file content for quickcheck stage test")
		hash16k := md5.Sum(content)
		fileName := "original.txt"

		sliceSize := uint64(len(content))

		mainPkt := buildPacket(setID, typeMain, buildMainBody(sliceSize, fileID))
		fdPkt := buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, fullHash, hash16k, sliceSize, fileName))
		ifscPkt := buildPacket(setID, typeIFSC, buildIFSCBody(fileID, hash16k, crc32.ChecksumIEEE(content)))

		parContent := make([]byte, 0, len(mainPkt)+len(fdPkt)+len(ifscPkt))
		parContent = append(parContent, mainPkt...)
		parContent = append(parContent, fdPkt...)
		parContent = append(parContent, ifscPkt...)

		if err := os.WriteFile(parPath, parContent, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if err := os.WriteFile(filepath.Join(tmpDir, fileName), content, 0o644); err != nil {
			t.Fatal(err)
		}

		stage := NewQuickCheckStage()
		stage.SetEnabled(true)

		job := &Job{
			Job:         buildQCJob(t, "job-qc-err", "original.txt", int64(len(content)), crc32.ChecksumIEEE(content)+1), // Mismatch!
			DownloadDir: tmpDir,
		}

		err := stage.Run(t.Context(), job)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}

		if job.quickCheck == QuickCheckClean {
			t.Error("QuickCheck = clean on a CRC mismatch")
		}

		linesStr := strings.Join(job.OutputLines, "\n")
		if !strings.Contains(linesStr, "CRC mismatch") {
			t.Errorf("expected CRC mismatch line, got: %s", linesStr)
		}
	})

	t.Run("No CRC", func(t *testing.T) {
		tmpDir := t.TempDir()
		parPath := filepath.Join(tmpDir, "test.par2")

		setID := [16]byte{1, 2, 3, 4}
		fileID := [16]byte{10, 20, 30}
		fullHash := [16]byte{0xAA, 0xBB, 0xCC}

		content := []byte("flat file content for quickcheck stage test")
		hash16k := md5.Sum(content)
		fileName := "original.txt"

		sliceSize := uint64(len(content))

		mainPkt := buildPacket(setID, typeMain, buildMainBody(sliceSize, fileID))
		fdPkt := buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, fullHash, hash16k, sliceSize, fileName))
		ifscPkt := buildPacket(setID, typeIFSC, buildIFSCBody(fileID, hash16k, crc32.ChecksumIEEE(content)))

		parContent := make([]byte, 0, len(mainPkt)+len(fdPkt)+len(ifscPkt))
		parContent = append(parContent, mainPkt...)
		parContent = append(parContent, fdPkt...)
		parContent = append(parContent, ifscPkt...)

		if err := os.WriteFile(parPath, parContent, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if err := os.WriteFile(filepath.Join(tmpDir, fileName), content, 0o644); err != nil {
			t.Fatal(err)
		}

		stage := NewQuickCheckStage()
		stage.SetEnabled(true)

		job := &Job{
			Job:         buildQCJob(t, "job-qc-nocrc", "original.txt", int64(len(content)), 0), // No CRC!
			DownloadDir: tmpDir,
		}

		err := stage.Run(t.Context(), job)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}

		if job.quickCheck == QuickCheckClean {
			t.Error("QuickCheck = clean on a missing CRC")
		}

		linesStr := strings.Join(job.OutputLines, "\n")
		if !strings.Contains(linesStr, "CRC unavailable") {
			t.Errorf("expected CRC unavailable line, got: %s", linesStr)
		}
	})

	t.Run("Unverified", func(t *testing.T) {
		tmpDir := t.TempDir()
		parPath := filepath.Join(tmpDir, "test.par2")

		setID := [16]byte{1, 2, 3, 4}
		fileID := [16]byte{10, 20, 30}
		fullHash := [16]byte{0xAA, 0xBB, 0xCC}

		content := []byte("flat file content for quickcheck stage test")
		hash16k := md5.Sum(content)
		fileName := "original.txt"

		sliceSize := uint64(len(content))

		mainPkt := buildPacket(setID, typeMain, buildMainBody(sliceSize, fileID))
		fdPkt := buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, fullHash, hash16k, sliceSize, fileName))
		ifscPkt := buildPacket(setID, typeIFSC, buildIFSCBody(fileID, hash16k, crc32.ChecksumIEEE(content)))

		parContent := make([]byte, 0, len(mainPkt)+len(fdPkt)+len(ifscPkt))
		parContent = append(parContent, mainPkt...)
		parContent = append(parContent, fdPkt...)
		parContent = append(parContent, ifscPkt...)

		if err := os.WriteFile(parPath, parContent, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		// The downloaded file's name and content don't match the par2 entry
		// (original.txt) at all, and CRC+size fallback can't rescue it either
		// — so the par2 entry is left unconsumed: a genuine name mismatch.
		stage := NewQuickCheckStage()
		stage.SetEnabled(true)

		job := &Job{
			Job:         buildQCJob(t, "job-qc-unver", "unrelated.dat", 999, 0x99999999),
			DownloadDir: tmpDir,
		}

		err := stage.Run(t.Context(), job)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}

		if job.quickCheck == QuickCheckClean {
			t.Error("QuickCheck = clean despite unverified files")
		}

		linesStr := strings.Join(job.OutputLines, "\n")
		if !strings.Contains(linesStr, fileName) {
			t.Errorf("expected OutputLines to name the unverified file %q, got: %s", fileName, linesStr)
		}
		// "could not be verified", not "not found by name".
		//
		// Nothing is matched by name any more, and Unverified now covers two
		// causes this line cannot tell apart: a par2 entry no delivered file
		// was shown to be, and an identified file the queue supplied no CRC
		// for. An operator reading "not found by name" would go looking for a
		// filename problem that does not exist.
		if !strings.Contains(linesStr, "could not be verified") {
			t.Errorf("expected OutputLines to explain why, got: %s", linesStr)
		}
		if strings.Contains(linesStr, "not found by name") {
			t.Errorf("OutputLines still claim a name match was attempted: %s", linesStr)
		}
	})
}

func TestQuickCheckStage_LayoutBRetryWithLeftoverExtractedFileDefersPar2Set(t *testing.T) {
	t.Parallel()

	// In Layout B, release.rar extracts to feature.bin, and feature.par2 protects feature.bin.
	// On Attempt 2 of a retry, feature.bin (e.g. corrupt from Attempt 1) already sits on disk.
	job, dir := layoutBJob(t, "release.rar")
	leftoverExtracted := filepath.Join(dir, "feature.bin")
	if err := os.WriteFile(leftoverExtracted, []byte("corrupt leftover extracted member"), 0o644); err != nil {
		t.Fatalf("write leftover extracted member: %v", err)
	}

	qc, _, _ := layoutStages()
	if err := qc.Run(t.Context(), job); err != nil {
		t.Fatalf("quickcheck: %v", err)
	}

	if job.quickCheck != QuickCheckUnidentified {
		t.Errorf("quickCheck = %v, want %v (QuickCheckUnidentified)", job.quickCheck, QuickCheckUnidentified)
	}
	if len(job.deferredPar2Sets) != 1 || job.deferredPar2Sets[0] != "feature" {
		t.Errorf("deferredPar2Sets = %v, want [feature]", job.deferredPar2Sets)
	}
}

// An unreadable set outranks a deferred one: Unidentified would skip repair,
// and with it the retry that fetches the set's held volumes (#1008). The job
// is TestQuickCheckStage_LayoutBRetryWithLeftoverExtractedFileDefersPar2Set's.
func TestQuickCheckStage_UnreadableSetBesideADeferredOne(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		unreadable, held bool
		want             QuickCheckOutcome
	}{
		"with an unreadable set and a held volume": {true, true, QuickCheckInconclusive},
		"with an unreadable set and nothing held":  {true, false, QuickCheckUnidentified},
		"alone": {false, false, QuickCheckUnidentified},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			job, dir := layoutBJobHolding(t, "release.rar", tc.held)
			if err := os.WriteFile(filepath.Join(dir, "feature.bin"), []byte("corrupt leftover extracted member"), 0o644); err != nil {
				t.Fatalf("write leftover extracted member: %v", err)
			}
			if tc.unreadable {
				if err := os.WriteFile(filepath.Join(dir, "other.par2"), make([]byte, 4096), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			qc, _, _ := layoutStages()
			if err := qc.Run(t.Context(), job); err != nil {
				t.Fatalf("quickcheck: %v", err)
			}
			if job.quickCheck != tc.want {
				t.Errorf("quickCheck = %v, want %v", job.quickCheck, tc.want)
			}
		})
	}
}

// A job whose only par2 set reads as nothing stays Inconclusive either way.
// With a volume held, repair runs it and fails, which fetches the volume.
// With none held, the set is recorded unverified and the stage says there
// was nothing to verify, so repair has nothing to run.
func TestQuickCheckStage_LoneUnreadableSet(t *testing.T) {
	t.Parallel()

	for name, held := range map[string]bool{"a volume held": true, "nothing held": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "other.par2"), make([]byte, 4096), 0o600); err != nil {
				t.Fatal(err)
			}
			content := []byte("a delivered file no readable set describes")
			if err := os.WriteFile(filepath.Join(dir, "data.bin"), content, 0o600); err != nil {
				t.Fatal(err)
			}
			build := buildQCJob
			if held {
				build = buildQCJobHolding
			}
			job := &Job{
				Job:         build(t, "job-lone-unreadable", "data.bin", int64(len(content)), 0),
				DownloadDir: dir,
			}
			stage := NewQuickCheckStage()
			stage.SetEnabled(true)
			if err := stage.Run(t.Context(), job); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if job.quickCheck != QuickCheckInconclusive {
				t.Errorf("QuickCheck = %s, want %s", job.quickCheck, QuickCheckInconclusive)
			}
			var wantUnverified []string
			if !held {
				wantUnverified = []string{"other"}
			}
			if !slices.Equal(job.unverifiedPar2Sets, wantUnverified) {
				t.Errorf("unverifiedPar2Sets = %q, want %q", job.unverifiedPar2Sets, wantUnverified)
			}
			nothing := slices.ContainsFunc(job.OutputLines, func(l string) bool {
				return strings.Contains(l, "No par2 set is readable") && strings.Contains(l, "nothing to verify")
			})
			if nothing != !held {
				t.Errorf("a 'nothing to verify' line = %v, want %v\n%s", nothing, !held, strings.Join(job.OutputLines, "\n"))
			}
		})
	}
}

// A par2 set that reads as nothing beside an intact one leaves the verdict
// Inconclusive, not Clean, while the job holds a recovery volume, so repair
// runs and its failure fetches the volume (#1008). With none held nothing can
// make the set readable, so the verdict is read over the intact set and the
// unreadable one is reported unverified rather than failing the job.
func TestQuickCheckStage_UnreadableSetBesideACleanOne(t *testing.T) {
	t.Parallel()

	setID := [16]byte{1, 2, 3, 4}
	fileID := [16]byte{10, 20, 30}
	content := []byte("intact file protected by the readable set")
	hash16k := md5.Sum(content)
	sliceSize := uint64(len(content))
	par := slices.Concat(
		buildPacket(setID, typeMain, buildMainBody(sliceSize, fileID)),
		buildPacket(setID, typeFileDesc, buildFileDescBody(fileID, [16]byte{0xAA}, hash16k, sliceSize, "intact.txt")),
		buildPacket(setID, typeIFSC, buildIFSCBody(fileID, hash16k, crc32.ChecksumIEEE(content))),
	)

	for name, tc := range map[string]struct {
		unreadable, held bool
		want             QuickCheckOutcome
	}{
		"with an unreadable set and a held volume": {true, true, QuickCheckInconclusive},
		"with an unreadable set and nothing held":  {true, false, QuickCheckClean},
		"alone": {false, false, QuickCheckClean},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "intact.par2"), par, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "intact.txt"), content, 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.unreadable {
				if err := os.WriteFile(filepath.Join(dir, "other.par2"), make([]byte, 4096), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			stage := NewQuickCheckStage()
			stage.SetEnabled(true)
			build := buildQCJob
			if tc.held {
				build = buildQCJobHolding
			}
			job := &Job{
				Job:         build(t, "job-unreadable", "intact.txt", int64(len(content)), crc32.ChecksumIEEE(content)),
				DownloadDir: dir,
			}
			if err := stage.Run(t.Context(), job); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if job.quickCheck != tc.want {
				t.Errorf("QuickCheck = %s, want %s\n%s", job.quickCheck, tc.want, strings.Join(job.OutputLines, "\n"))
			}
			unverified := slices.ContainsFunc(job.OutputLines, func(l string) bool {
				return strings.Contains(l, "other") && strings.Contains(l, "not verified")
			})
			if want := tc.unreadable && !tc.held; unverified != want {
				t.Errorf("an 'other ... not verified' line = %v, want %v\n%s", unverified, want, strings.Join(job.OutputLines, "\n"))
			}
		})
	}
}

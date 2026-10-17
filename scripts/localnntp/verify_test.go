package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hobeone/gonzbd/internal/job"
)

func createTestDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(dir, "history.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	schema := `
	CREATE TABLE dispatch_jobs (
		id TEXT PRIMARY KEY,
		sort_key INTEGER,
		name TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE job_files (
		job_id TEXT,
		file_index INTEGER,
		filename TEXT NOT NULL DEFAULT '',
		complete INTEGER NOT NULL DEFAULT 0,
		fetch_policy INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(job_id, file_index)
	);
	CREATE TABLE written_articles (
		job_id TEXT,
		file_idx INTEGER,
		art_idx INTEGER,
		offset INTEGER,
		length INTEGER,
		crc32 INTEGER,
		PRIMARY KEY(job_id, file_idx, art_idx)
	);
	CREATE TABLE history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		nzo_id TEXT UNIQUE,
		name TEXT NOT NULL DEFAULT '',
		status TEXT,
		storage TEXT,
		path TEXT,
		fail_message TEXT
	);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	return db
}

func TestVerify_Crash_HappyPath(t *testing.T) {
	tempDir := t.TempDir()
	adminDir := filepath.Join(tempDir, "admin")
	downloadDir := filepath.Join(tempDir, "incomplete")
	if err := os.MkdirAll(adminDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		t.Fatal(err)
	}

	db := createTestDB(t, adminDir)
	jobID := "job-crash-1"
	if _, err := db.Exec(`INSERT INTO dispatch_jobs (id, sort_key) VALUES (?, ?)`, jobID, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO job_files (job_id, file_index, filename, complete) VALUES (?, ?, ?, ?)`,
		jobID, 0, "release.part01.rar", 0); err != nil {
		t.Fatal(err)
	}

	payload := []byte("article payload content for crash verification test")
	crc := crc32.ChecksumIEEE(payload)
	if _, err := db.Exec(`INSERT INTO written_articles (job_id, file_idx, art_idx, offset, length, crc32) VALUES (?, ?, ?, ?, ?, ?)`,
		jobID, 0, 0, 0, len(payload), crc); err != nil {
		t.Fatal(err)
	}

	// Write partial file on disk
	filePath := filepath.Join(downloadDir, "release.part01.rar")
	if err := os.WriteFile(filePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	manifest := &ReleaseManifest{
		Name:     "release",
		PartSize: len(payload),
		Files: []ReleaseFile{
			{
				FileIndex:   0,
				DiskName:    "release.part01.rar",
				NZBFilename: "release.part01.rar",
				Role:        RoleRARVolume,
				TotalSize:   int64(len(payload)),
				Segments: []SegmentMeta{
					{
						MessageID: "<art0@localnntp.test>",
						PartNum:   1,
						Offset:    0,
						Length:    len(payload),
						CRC32:     crc,
					},
				},
			},
		},
	}
	manifestPath := filepath.Join(tempDir, "manifest.json")
	if err := SaveManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}

	// Setup mock server control handler
	mkFilesDir(t, tempDir)
	srv, err := NewServer(ServerConfig{
		Releases:   []ReleaseSource{{Dir: tempDir, Manifest: manifest}},
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	controlSrv := httptest.NewServer(NewControlHandler(srv))
	defer controlSrv.Close()

	cfg := VerifyConfig{
		Mode:             "crash",
		ManifestPath:     manifestPath,
		AdminDir:         adminDir,
		DownloadDir:      downloadDir,
		JobID:            "", // Test auto-discovery
		ServerControlURL: controlSrv.URL,
		SnapshotAs:       "crash1",
	}

	report, err := Verify(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Verify crash failed: %v", err)
	}
	if report.Mode != "crash" {
		t.Errorf("got mode %q, want crash", report.Mode)
	}
	if report.JobID != jobID {
		t.Errorf("got job ID %q, want %q", report.JobID, jobID)
	}
	if report.VerifiedRows != 1 {
		t.Errorf("got %d verified rows, want 1", report.VerifiedRows)
	}
	if len(report.RecordedIDs) != 1 || report.RecordedIDs[0] != "art0@localnntp.test" {
		t.Errorf("unexpected recorded IDs: %v", report.RecordedIDs)
	}

	// Verify snapshot was created on server
	snap, ok := srv.GetSnapshot("crash1")
	if !ok {
		t.Fatal("snapshot crash1 was not created on server")
	}
	if len(snap.RecordedIDs) != 1 || snap.RecordedIDs[0] != "art0@localnntp.test" {
		t.Errorf("unexpected snapshot recorded IDs: %v", snap.RecordedIDs)
	}
}

func TestVerify_Crash_QueueManifestGz(t *testing.T) {
	tempDir := t.TempDir()
	adminDir := filepath.Join(tempDir, "admin")
	downloadDir := filepath.Join(tempDir, "incomplete")
	queueManifestDir := filepath.Join(adminDir, "queue", "manifests")
	if err := os.MkdirAll(queueManifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		t.Fatal(err)
	}

	db := createTestDB(t, adminDir)
	jobID := "job-gz-test"
	if _, err := db.Exec(`INSERT INTO dispatch_jobs (id, sort_key) VALUES (?, ?)`, jobID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO job_files (job_id, file_index, filename, complete) VALUES (?, ?, ?, ?)`,
		jobID, 0, "release.part01.rar", 0); err != nil {
		t.Fatal(err)
	}

	payload := []byte("hello queue manifest gzip")
	crc := crc32.ChecksumIEEE(payload)
	if _, err := db.Exec(`INSERT INTO written_articles (job_id, file_idx, art_idx, offset, length, crc32) VALUES (?, ?, ?, ?, ?, ?)`,
		jobID, 0, 0, 0, len(payload), crc); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(downloadDir, "release.part01.rar"), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	// Write gzipped manifest using internal/job format
	jm := job.NewManifest([]job.JobFile{
		{
			Subject: "release.part01.rar",
			Bytes:   int64(len(payload)),
			Articles: []job.JobArticle{
				{
					ID:     "gz-article-id@test",
					Bytes:  len(payload),
					Number: 1,
				},
			},
		},
	})
	jmData, err := json.Marshal(jm)
	if err != nil {
		t.Fatal(err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(jmData); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(queueManifestDir, jobID+".json.gz"), gzBuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	manifest := &ReleaseManifest{
		Name:     "release",
		PartSize: len(payload),
		Files: []ReleaseFile{
			{
				FileIndex:   0,
				DiskName:    "release.part01.rar",
				NZBFilename: "release.part01.rar",
				Role:        RoleRARVolume,
				TotalSize:   int64(len(payload)),
				Segments: []SegmentMeta{
					{
						MessageID: "<dummy@test>",
						PartNum:   1,
						Offset:    0,
						Length:    len(payload),
						CRC32:     crc,
					},
				},
			},
		},
	}
	manifestPath := filepath.Join(tempDir, "manifest.json")
	if err := SaveManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}

	report, err := Verify(context.Background(), VerifyConfig{
		Mode:         "crash",
		ManifestPath: manifestPath,
		AdminDir:     adminDir,
		DownloadDir:  downloadDir,
		JobID:        jobID,
	})
	if err != nil {
		t.Fatalf("Verify with gz manifest failed: %v", err)
	}
	if len(report.RecordedIDs) != 1 || report.RecordedIDs[0] != "gz-article-id@test" {
		t.Errorf("got recorded IDs %v, want [gz-article-id@test]", report.RecordedIDs)
	}

	// Verify corrupt queue manifest returns an error instead of silently falling back.
	if err := os.WriteFile(filepath.Join(queueManifestDir, jobID+".json.gz"), []byte("not-gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), VerifyConfig{
		Mode:         "crash",
		ManifestPath: manifestPath,
		AdminDir:     adminDir,
		DownloadDir:  downloadDir,
		JobID:        jobID,
	}); err == nil || !strings.Contains(err.Error(), "gzip queue manifest") {
		t.Fatalf("expected error on corrupt gzip queue manifest, got: %v", err)
	}
}

func TestVerify_Crash_Errors(t *testing.T) {
	tempDir := t.TempDir()
	adminDir := filepath.Join(tempDir, "admin")
	downloadDir := filepath.Join(tempDir, "incomplete")
	_ = os.MkdirAll(adminDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	db := createTestDB(t, adminDir)
	jobID := "job-err"
	_, _ = db.Exec(`INSERT INTO dispatch_jobs (id, sort_key) VALUES (?, ?)`, jobID, 1)
	_, _ = db.Exec(`INSERT INTO job_files (job_id, file_index, filename, complete) VALUES (?, ?, ?, ?)`,
		jobID, 0, "file.rar", 0)

	payload := []byte("correct bytes")
	crc := crc32.ChecksumIEEE(payload)
	_, _ = db.Exec(`INSERT INTO written_articles (job_id, file_idx, art_idx, offset, length, crc32) VALUES (?, ?, ?, ?, ?, ?)`,
		jobID, 0, 0, 0, len(payload), crc)

	manifest := &ReleaseManifest{
		Name:     "release",
		PartSize: len(payload),
		Files: []ReleaseFile{
			{
				FileIndex:   0,
				DiskName:    "file.rar",
				NZBFilename: "file.rar",
				Role:        RoleRARVolume,
				TotalSize:   int64(len(payload)),
				Segments:    []SegmentMeta{{MessageID: "<1@test>", Length: len(payload), CRC32: crc}},
			},
		},
	}
	manifestPath := filepath.Join(tempDir, "manifest.json")
	_ = SaveManifest(manifestPath, manifest)

	// Subtest 1: Missing file
	t.Run("missing file", func(t *testing.T) {
		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "crash",
			ManifestPath: manifestPath,
			AdminDir:     adminDir,
			DownloadDir:  downloadDir,
			JobID:        jobID,
		})
		if err == nil || !strings.Contains(err.Error(), "partial file") {
			t.Fatalf("expected missing file error, got: %v", err)
		}
	})

	// Subtest 2: Truncated file
	t.Run("truncated file", func(t *testing.T) {
		_ = os.WriteFile(filepath.Join(downloadDir, "file.rar"), []byte("short"), 0o600)
		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "crash",
			ManifestPath: manifestPath,
			AdminDir:     adminDir,
			DownloadDir:  downloadDir,
			JobID:        jobID,
		})
		if err == nil || !strings.Contains(err.Error(), "truncated") {
			t.Fatalf("expected truncated error, got: %v", err)
		}
	})

	// Subtest 3: CRC mismatch
	t.Run("crc mismatch", func(t *testing.T) {
		_ = os.WriteFile(filepath.Join(downloadDir, "file.rar"), []byte("corrupt bytes"), 0o600)
		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "crash",
			ManifestPath: manifestPath,
			AdminDir:     adminDir,
			DownloadDir:  downloadDir,
			JobID:        jobID,
		})
		if err == nil || !strings.Contains(err.Error(), "crc32 mismatch") {
			t.Fatalf("expected crc mismatch error, got: %v", err)
		}
	})

	// Subtest 4: Unknown mode
	t.Run("unknown mode", func(t *testing.T) {
		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "bogus",
			ManifestPath: manifestPath,
			AdminDir:     adminDir,
			DownloadDir:  downloadDir,
		})
		if err == nil || !strings.Contains(err.Error(), "unknown mode") {
			t.Fatalf("expected unknown mode error, got: %v", err)
		}
	})

	// Subtest 5: No job in DB
	t.Run("no job in db", func(t *testing.T) {
		emptyAdmin := t.TempDir()
		_ = createTestDB(t, emptyAdmin)
		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "crash",
			ManifestPath: manifestPath,
			AdminDir:     emptyAdmin,
			DownloadDir:  downloadDir,
		})
		if err == nil || !strings.Contains(err.Error(), "no job found") {
			t.Fatalf("expected no job found error, got: %v", err)
		}
	})

	// Subtest 6: Missing manifest path or admin dir
	t.Run("missing paths", func(t *testing.T) {
		_, err := Verify(context.Background(), VerifyConfig{})
		if err == nil || !strings.Contains(err.Error(), "manifest path is required") {
			t.Fatalf("expected manifest path required error, got: %v", err)
		}

		_, err = Verify(context.Background(), VerifyConfig{
			ManifestPath: manifestPath,
		})
		if err == nil || !strings.Contains(err.Error(), "admin dir is required") {
			t.Fatalf("expected admin dir required error, got: %v", err)
		}
	})

	// Subtest 7: Partial file in job subdir and nested walk
	t.Run("partial file search variations", func(t *testing.T) {
		jobSubdir := filepath.Join(downloadDir, "job-sub")
		_ = os.MkdirAll(jobSubdir, 0o755)
		_ = os.WriteFile(filepath.Join(jobSubdir, "sub.rar"), []byte("sub"), 0o600)
		p, err := findPartialFile(downloadDir, []string{"job-sub"}, "sub.rar")
		if err != nil || !strings.HasSuffix(p, filepath.Join("job-sub", "sub.rar")) {
			t.Fatalf("findPartialFile jobSubdir failed: p=%s err=%v", p, err)
		}

		nestedSubdir := filepath.Join(downloadDir, "nested", "deep")
		_ = os.MkdirAll(nestedSubdir, 0o755)
		_ = os.WriteFile(filepath.Join(nestedSubdir, "deep.rar"), []byte("deep"), 0o600)
		p, err = findPartialFile(downloadDir, []string{"nested"}, "deep.rar")
		if err != nil || !strings.HasSuffix(p, filepath.Join("nested", "deep", "deep.rar")) {
			t.Fatalf("findPartialFile walk failed: p=%s err=%v", p, err)
		}

		// Stale file in an unrelated sibling job directory must NOT be returned.
		_, err = findPartialFile(downloadDir, []string{"current-job"}, "deep.rar")
		if err == nil {
			t.Fatal("expected error when partial file only exists in unrelated sibling subdir")
		}

		_, err = findPartialFile(downloadDir, nil, "nonexistent.rar")
		if err == nil {
			t.Fatal("expected error for nonexistent partial file")
		}
	})
}

func TestVerify_Crash_MidPar2RepairOnCompleteFile(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	adminDir := filepath.Join(tempDir, "admin")
	downloadDir := filepath.Join(tempDir, "incomplete")
	jobSub := filepath.Join(downloadDir, "ExtMovie")
	if err := os.MkdirAll(adminDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(jobSub, 0o755); err != nil {
		t.Fatal(err)
	}

	db := createTestDB(t, adminDir)
	jobID := "job-mid-repair"
	if _, err := db.Exec(`INSERT INTO dispatch_jobs (id, sort_key, name) VALUES (?, ?, ?)`, jobID, 1, "ExtMovie"); err != nil {
		t.Fatal(err)
	}

	// File 0 (complete=1): external par2 renamed ExtMovie.part01.rar -> ExtMovie.part01.rar.1
	// and truncated ExtMovie.part01.rar to 0 bytes right before SIGKILL.
	// File 1 (complete=1): par2 already repaired a corrupt-payload segment in place so the
	// on-disk bytes match the clean ReleaseManifest CRC32 rather than written_articles' corrupt CRC32.
	// File 2 (complete=1): quickcheck/par2 renamed obfuscated obf03.bin -> ExtMovie.part03.rar.
	cleanBytes := []byte("clean-segment-payload-bytes")
	cleanCRC := crc32.ChecksumIEEE(cleanBytes)
	corruptBytes := []byte("corrupt-segment-payload-xyz")
	corruptCRC := crc32.ChecksumIEEE(corruptBytes)

	for _, row := range []struct {
		idx      int
		filename string
		complete int
	}{
		{0, "ExtMovie.part01.rar", 1},
		{1, "ExtMovie.part02.rar", 1},
		{2, "obf03.bin", 1},
	} {
		if _, err := db.Exec(`INSERT INTO job_files (job_id, file_index, filename, complete) VALUES (?, ?, ?, ?)`,
			jobID, row.idx, row.filename, row.complete); err != nil {
			t.Fatal(err)
		}
	}

	for _, wr := range []struct {
		fileIdx int
		artIdx  int
		length  int
		crc     uint32
	}{
		{0, 0, len(cleanBytes), cleanCRC},
		{1, 1, len(corruptBytes), corruptCRC},
		{2, 2, len(cleanBytes), cleanCRC},
	} {
		if _, err := db.Exec(`INSERT INTO written_articles (job_id, file_idx, art_idx, offset, length, crc32) VALUES (?, ?, ?, ?, ?, ?)`,
			jobID, wr.fileIdx, wr.artIdx, 0, wr.length, wr.crc); err != nil {
			t.Fatal(err)
		}
	}

	// File 0 on disk: truncated 0-byte target + intact .1 backup.
	if err := os.WriteFile(filepath.Join(jobSub, "ExtMovie.part01.rar"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobSub, "ExtMovie.part01.rar.1"), cleanBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// File 1 on disk: repaired in place with cleanBytes, with .1 backup present on disk as evidence repair ran.
	if err := os.WriteFile(filepath.Join(jobSub, "ExtMovie.part02.rar"), cleanBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobSub, "ExtMovie.part02.rar.1"), corruptBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// File 2 on disk: renamed from obf03.bin to DiskName ExtMovie.part03.rar.
	if err := os.WriteFile(filepath.Join(jobSub, "ExtMovie.part03.rar"), cleanBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	manifest := &ReleaseManifest{
		Name:     "ExtMovie",
		PartSize: len(cleanBytes),
		Files: []ReleaseFile{
			{
				FileIndex:   0,
				DiskName:    "ExtMovie.part01.rar",
				NZBFilename: "ExtMovie.part01.rar",
				Role:        RoleRARVolume,
				TotalSize:   int64(len(cleanBytes)),
				Segments:    []SegmentMeta{{MessageID: "<m0@test>", PartNum: 1, Offset: 0, Length: len(cleanBytes), CRC32: cleanCRC}},
			},
			{
				FileIndex:   1,
				DiskName:    "ExtMovie.part02.rar",
				NZBFilename: "ExtMovie.part02.rar",
				Role:        RoleRARVolume,
				TotalSize:   int64(len(cleanBytes)),
				Segments:    []SegmentMeta{{MessageID: "<m1@test>", PartNum: 1, Offset: 0, Length: len(cleanBytes), CRC32: cleanCRC}},
			},
			{
				FileIndex:   2,
				DiskName:    "ExtMovie.part03.rar",
				NZBFilename: "obf03.bin",
				Role:        RoleRARVolume,
				TotalSize:   int64(len(cleanBytes)),
				Segments:    []SegmentMeta{{MessageID: "<m2@test>", PartNum: 1, Offset: 0, Length: len(cleanBytes), CRC32: cleanCRC}},
			},
		},
	}
	manifestPath := filepath.Join(tempDir, "manifest.json")
	if err := SaveManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}

	report, err := Verify(t.Context(), VerifyConfig{
		Mode:         "crash",
		ManifestPath: manifestPath,
		AdminDir:     adminDir,
		DownloadDir:  downloadDir,
		JobID:        jobID,
	})
	if err != nil {
		t.Fatalf("Verify crash on mid-repair complete files failed: %v", err)
	}
	if report.VerifiedRows != 3 {
		t.Errorf("VerifiedRows = %d, want 3", report.VerifiedRows)
	}
	if report.FallbackRows != 3 {
		t.Errorf("FallbackRows = %d, want 3", report.FallbackRows)
	}

	// Without a .1 backup on disk, clean manifest CRC must NOT be accepted when
	// written_articles recorded a different CRC.
	if err := os.Remove(filepath.Join(jobSub, "ExtMovie.part02.rar.1")); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(t.Context(), VerifyConfig{
		Mode:         "crash",
		ManifestPath: manifestPath,
		AdminDir:     adminDir,
		DownloadDir:  downloadDir,
		JobID:        jobID,
	}); err == nil {
		t.Fatal("expected error when clean manifest CRC is used without a .1 repair backup on disk")
	}
	if err := os.WriteFile(filepath.Join(jobSub, "ExtMovie.part02.rar.1"), corruptBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// Split-brain across candidates: if File 0 has two rows where row 0 is only
	// valid in ExtMovie.part01.rar and row 1 is only valid in ExtMovie.part01.rar.1,
	// no single candidate file is whole, so Verify must fail.
	if _, err := db.Exec(`INSERT INTO written_articles (job_id, file_idx, art_idx, offset, length, crc32) VALUES (?, ?, ?, ?, ?, ?)`,
		jobID, 0, 3, len(cleanBytes), len(cleanBytes), cleanCRC); err != nil {
		t.Fatal(err)
	}
	bogusChunk := []byte("bogus-segment-payload-00000")
	primarySplit := append(append([]byte{}, cleanBytes...), bogusChunk...)
	backupSplit := append(append([]byte{}, bogusChunk...), cleanBytes...)
	if err := os.WriteFile(filepath.Join(jobSub, "ExtMovie.part01.rar"), primarySplit, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobSub, "ExtMovie.part01.rar.1"), backupSplit, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(t.Context(), VerifyConfig{
		Mode:         "crash",
		ManifestPath: manifestPath,
		AdminDir:     adminDir,
		DownloadDir:  downloadDir,
		JobID:        jobID,
	}); err == nil {
		t.Fatal("expected error when neither primary nor .1 backup is intact across all rows of File 0")
	}

	// If a complete=1 file has bogus bytes matching neither written_articles nor
	// the clean ReleaseManifest CRC32, Verify must still fail.
	if err := os.WriteFile(filepath.Join(jobSub, "ExtMovie.part02.rar"), []byte("bogus-bytes-not-matching!!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(t.Context(), VerifyConfig{
		Mode:         "crash",
		ManifestPath: manifestPath,
		AdminDir:     adminDir,
		DownloadDir:  downloadDir,
		JobID:        jobID,
	}); err == nil {
		t.Fatal("expected error when complete=1 file matches neither written_articles nor ReleaseManifest CRC32")
	}
}

// completeFixture is a finished job: history row, extracted payload, and a one-segment manifest.
type completeFixture struct {
	tempDir  string
	manifest *ReleaseManifest
	cfg      VerifyConfig // complete mode; no server fields set
}

func newCompleteFixture(t *testing.T) completeFixture {
	t.Helper()
	tempDir := t.TempDir()
	adminDir := filepath.Join(tempDir, "admin")
	downloadDir := filepath.Join(tempDir, "incomplete")
	completeDir := filepath.Join(tempDir, "complete")
	for _, d := range []string{adminDir, downloadDir, completeDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	db := createTestDB(t, adminDir)
	jobID := "job-comp-1"
	jobStorage := filepath.Join(completeDir, "myrelease")
	if err := os.MkdirAll(jobStorage, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO history (nzo_id, status, storage, path, fail_message) VALUES (?, ?, ?, ?, ?)`,
		jobID, "Completed", jobStorage, jobStorage, ""); err != nil {
		t.Fatal(err)
	}

	payloadData := []byte("the complete extracted file content")
	if err := os.WriteFile(filepath.Join(jobStorage, "payload.bin"), payloadData, 0o600); err != nil {
		t.Fatal(err)
	}

	manifest := &ReleaseManifest{
		Name:     "myrelease",
		PartSize: 1024,
		ExpectedOutputs: []ExpectedOutput{
			{
				Name:   "payload.bin",
				Size:   int64(len(payloadData)),
				SHA256: fmt.Sprintf("%x", sha256.Sum256(payloadData)),
			},
		},
		Files: []ReleaseFile{
			{
				FileIndex:   0,
				DiskName:    "release.part01.rar",
				NZBFilename: "release.part01.rar",
				Role:        RoleRARVolume,
				TotalSize:   int64(len(payloadData)),
				Segments:    []SegmentMeta{{MessageID: "<art1@test>", Length: len(payloadData)}},
			},
		},
	}
	manifestPath := filepath.Join(tempDir, "manifest.json")
	if err := SaveManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	return completeFixture{
		tempDir:  tempDir,
		manifest: manifest,
		cfg: VerifyConfig{
			Mode:         "complete",
			ManifestPath: manifestPath,
			AdminDir:     adminDir,
			DownloadDir:  downloadDir,
			CompleteDir:  completeDir,
			JobID:        jobID,
		},
	}
}

func TestVerify_Complete_HappyPath(t *testing.T) {
	fx := newCompleteFixture(t)
	jobID := fx.cfg.JobID

	// Setup mock server
	mkFilesDir(t, fx.tempDir)
	srv, err := NewServer(ServerConfig{
		Releases:   []ReleaseSource{{Dir: fx.tempDir, Manifest: fx.manifest}},
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := srv.TakeSnapshot("crash1", []string{"art1@test"})
	if snap.Name != "crash1" {
		t.Fatal("snapshot creation failed")
	}
	controlSrv := httptest.NewServer(NewControlHandler(srv))
	defer controlSrv.Close()

	cfg := fx.cfg
	cfg.ServerControlURL = controlSrv.URL
	cfg.CompareSnapshot = "crash1"

	report, err := Verify(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Verify complete failed: %v", err)
	}
	if report.Mode != "complete" {
		t.Errorf("got mode %q, want complete", report.Mode)
	}
	if report.JobID != jobID {
		t.Errorf("got job ID %q, want %q", report.JobID, jobID)
	}
	if report.OutputsVerified != 1 {
		t.Errorf("got outputs verified %d, want 1", report.OutputsVerified)
	}
	if len(report.RefetchedIDs) != 0 {
		t.Errorf("unexpected refetched IDs: %v", report.RefetchedIDs)
	}
}

func TestVerify_Complete_Errors(t *testing.T) {
	tempDir := t.TempDir()
	adminDir := filepath.Join(tempDir, "admin")
	downloadDir := filepath.Join(tempDir, "incomplete")
	completeDir := filepath.Join(tempDir, "complete")
	_ = os.MkdirAll(adminDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)
	_ = os.MkdirAll(completeDir, 0o755)

	payloadData := []byte("valid output bytes")
	hash := sha256.Sum256(payloadData)
	hashHex := fmt.Sprintf("%x", hash)

	manifest := &ReleaseManifest{
		Name:     "release",
		PartSize: 1024,
		ExpectedOutputs: []ExpectedOutput{
			{Name: "payload.bin", Size: int64(len(payloadData)), SHA256: hashHex},
		},
	}
	manifestPath := filepath.Join(tempDir, "manifest.json")
	_ = SaveManifest(manifestPath, manifest)

	// Subtest 1: Status Failed
	t.Run("status failed", func(t *testing.T) {
		tDir := t.TempDir()
		aDir := filepath.Join(tDir, "admin")
		_ = os.MkdirAll(aDir, 0o755)
		db := createTestDB(t, aDir)
		_, _ = db.Exec(`INSERT INTO history (nzo_id, status, fail_message) VALUES (?, ?, ?)`,
			"j1", "Failed", "par2 repair failed")

		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "complete",
			ManifestPath: manifestPath,
			AdminDir:     aDir,
			DownloadDir:  downloadDir,
			CompleteDir:  completeDir,
			JobID:        "j1",
		})
		if err == nil || !strings.Contains(err.Error(), "par2 repair failed") {
			t.Fatalf("expected failure message, got: %v", err)
		}
	})

	// Subtest 2: Job still in dispatch_jobs
	t.Run("job still in dispatch", func(t *testing.T) {
		tDir := t.TempDir()
		aDir := filepath.Join(tDir, "admin")
		_ = os.MkdirAll(aDir, 0o755)
		db := createTestDB(t, aDir)
		_, _ = db.Exec(`INSERT INTO history (nzo_id, status, storage) VALUES (?, ?, ?)`, "j2", "Completed", completeDir)
		_, _ = db.Exec(`INSERT INTO dispatch_jobs (id, sort_key) VALUES (?, ?)`, "j2", 1)

		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "complete",
			ManifestPath: manifestPath,
			AdminDir:     aDir,
			DownloadDir:  downloadDir,
			CompleteDir:  completeDir,
			JobID:        "j2",
		})
		if err == nil || !strings.Contains(err.Error(), "still present in dispatch_jobs") {
			t.Fatalf("expected dispatch_jobs error, got: %v", err)
		}
	})

	// Subtest 3: Output SHA256 mismatch
	t.Run("sha256 mismatch", func(t *testing.T) {
		tDir := t.TempDir()
		aDir := filepath.Join(tDir, "admin")
		cDir := filepath.Join(tDir, "complete")
		_ = os.MkdirAll(aDir, 0o755)
		_ = os.MkdirAll(cDir, 0o755)
		db := createTestDB(t, aDir)
		_, _ = db.Exec(`INSERT INTO history (nzo_id, status, storage) VALUES (?, ?, ?)`, "j3", "Completed", cDir)
		_ = os.WriteFile(filepath.Join(cDir, "payload.bin"), []byte("bad content bytes!"), 0o600)

		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "complete",
			ManifestPath: manifestPath,
			AdminDir:     aDir,
			DownloadDir:  downloadDir,
			CompleteDir:  cDir,
			JobID:        "j3",
		})
		if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
			t.Fatalf("expected sha256 mismatch, got: %v", err)
		}
	})

	// Subtest 4: Leftover rar or tmp in complete dir
	t.Run("leftover in complete dir", func(t *testing.T) {
		tDir := t.TempDir()
		aDir := filepath.Join(tDir, "admin")
		cDir := filepath.Join(tDir, "complete")
		_ = os.MkdirAll(aDir, 0o755)
		_ = os.MkdirAll(cDir, 0o755)
		db := createTestDB(t, aDir)
		_, _ = db.Exec(`INSERT INTO history (nzo_id, status, storage) VALUES (?, ?, ?)`, "j4", "Completed", cDir)
		_ = os.WriteFile(filepath.Join(cDir, "payload.bin"), payloadData, 0o600)
		_ = os.WriteFile(filepath.Join(cDir, "leftover.rar"), []byte("rar"), 0o600)

		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "complete",
			ManifestPath: manifestPath,
			AdminDir:     aDir,
			DownloadDir:  downloadDir,
			CompleteDir:  cDir,
			JobID:        "j4",
		})
		if err == nil || !strings.Contains(err.Error(), "hygiene") && !strings.Contains(err.Error(), ".rar") {
			t.Fatalf("expected leftover cleanup error, got: %v", err)
		}
	})

	// Subtest 5: Leftover in downloadDir
	t.Run("leftover in download dir", func(t *testing.T) {
		tDir := t.TempDir()
		aDir := filepath.Join(tDir, "admin")
		cDir := filepath.Join(tDir, "complete")
		dDir := filepath.Join(tDir, "incomplete")
		_ = os.MkdirAll(aDir, 0o755)
		_ = os.MkdirAll(cDir, 0o755)
		_ = os.MkdirAll(filepath.Join(dDir, "release"), 0o755)
		_ = os.WriteFile(filepath.Join(dDir, "release", "junk.part"), []byte("junk"), 0o600)
		db := createTestDB(t, aDir)
		_, _ = db.Exec(`INSERT INTO history (nzo_id, status, storage) VALUES (?, ?, ?)`, "j5", "Completed", cDir)
		_ = os.WriteFile(filepath.Join(cDir, "payload.bin"), payloadData, 0o600)

		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "complete",
			ManifestPath: manifestPath,
			AdminDir:     aDir,
			DownloadDir:  dDir,
			CompleteDir:  cDir,
			JobID:        "j5",
		})
		if err == nil || !strings.Contains(err.Error(), "incomplete directory") {
			t.Fatalf("expected leftover incomplete dir error, got: %v", err)
		}
	})

	// Subtest 7: Output file size mismatch and missing file
	t.Run("output file size mismatch and missing", func(t *testing.T) {
		tDir := t.TempDir()
		aDir := filepath.Join(tDir, "admin")
		cDir := filepath.Join(tDir, "complete")
		_ = os.MkdirAll(aDir, 0o755)
		_ = os.MkdirAll(cDir, 0o755)
		db := createTestDB(t, aDir)
		_, _ = db.Exec(`INSERT INTO history (nzo_id, status, storage) VALUES (?, ?, ?)`, "j-size", "Completed", cDir)
		_ = os.WriteFile(filepath.Join(cDir, "payload.bin"), []byte("short"), 0o600)

		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "complete",
			ManifestPath: manifestPath,
			AdminDir:     aDir,
			DownloadDir:  downloadDir,
			CompleteDir:  cDir,
			JobID:        "j-size",
		})
		if err == nil || !strings.Contains(err.Error(), "size mismatch") {
			t.Fatalf("expected size mismatch error, got: %v", err)
		}

		_ = os.Remove(filepath.Join(cDir, "payload.bin"))
		_, err = Verify(context.Background(), VerifyConfig{
			Mode:         "complete",
			ManifestPath: manifestPath,
			AdminDir:     aDir,
			DownloadDir:  downloadDir,
			CompleteDir:  cDir,
			JobID:        "j-size",
		})
		if err == nil || !strings.Contains(err.Error(), "not found in complete") {
			t.Fatalf("expected not found error, got: %v", err)
		}
	})

	// Subtest 8: Search variations in findExpectedOutputFile
	t.Run("expected output search variations", func(t *testing.T) {
		searchDir := t.TempDir()
		nested := filepath.Join(searchDir, "sub", "dir")
		_ = os.MkdirAll(nested, 0o755)
		_ = os.WriteFile(filepath.Join(nested, "out.bin"), []byte("content"), 0o600)

		p, err := findExpectedOutputFile([]string{"", searchDir}, "out.bin")
		if err != nil || !strings.HasSuffix(p, filepath.Join("sub", "dir", "out.bin")) {
			t.Fatalf("findExpectedOutputFile nested search failed: p=%s, err=%v", p, err)
		}

		_, err = findExpectedOutputFile([]string{searchDir}, "missing.bin")
		if err == nil {
			t.Fatal("expected missing output file error")
		}
	})

	// Subtest 9: Complete dir hygiene with .par2 and .gonzbd-pending-deletions
	t.Run("hygiene violations", func(t *testing.T) {
		tDir := t.TempDir()
		aDir := filepath.Join(tDir, "admin")
		cDir := filepath.Join(tDir, "complete")
		_ = os.MkdirAll(aDir, 0o755)
		_ = os.MkdirAll(cDir, 0o755)
		db := createTestDB(t, aDir)
		_, _ = db.Exec(`INSERT INTO history (nzo_id, status, storage) VALUES (?, ?, ?)`, "j-hyg", "Completed", cDir)
		_ = os.WriteFile(filepath.Join(cDir, "payload.bin"), payloadData, 0o600)
		_ = os.WriteFile(filepath.Join(cDir, "file.par2"), []byte("par2"), 0o600)

		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "complete",
			ManifestPath: manifestPath,
			AdminDir:     aDir,
			DownloadDir:  downloadDir,
			CompleteDir:  cDir,
			JobID:        "j-hyg",
		})
		if err == nil || !strings.Contains(err.Error(), "parity file") {
			t.Fatalf("expected parity file hygiene error, got: %v", err)
		}

		_ = os.Remove(filepath.Join(cDir, "file.par2"))
		_ = os.WriteFile(filepath.Join(cDir, "test.gonzbd-pending-deletions"), []byte("tmp"), 0o600)
		_, err = Verify(context.Background(), VerifyConfig{
			Mode:         "complete",
			ManifestPath: manifestPath,
			AdminDir:     aDir,
			DownloadDir:  downloadDir,
			CompleteDir:  cDir,
			JobID:        "j-hyg",
		})
		if err == nil || !strings.Contains(err.Error(), "temporary artifact") {
			t.Fatalf("expected temporary artifact hygiene error, got: %v", err)
		}
	})

	// Subtest 10: Auto-discovery of job ID in complete mode and query history error
	t.Run("complete auto-discovery and query error", func(t *testing.T) {
		tDir := t.TempDir()
		aDir := filepath.Join(tDir, "admin")
		cDir := filepath.Join(tDir, "complete")
		_ = os.MkdirAll(aDir, 0o755)
		_ = os.MkdirAll(cDir, 0o755)
		db := createTestDB(t, aDir)

		// Test error when history table is empty
		_, err := Verify(context.Background(), VerifyConfig{
			Mode:         "complete",
			ManifestPath: manifestPath,
			AdminDir:     aDir,
			DownloadDir:  downloadDir,
			CompleteDir:  cDir,
		})
		if err == nil || !strings.Contains(err.Error(), "query history") {
			t.Fatalf("expected query history error on empty table, got: %v", err)
		}

		// Insert history row and test auto-discovery (JobID == "")
		_, _ = db.Exec(`INSERT INTO history (nzo_id, status, storage) VALUES (?, ?, ?)`, "j-auto", "Completed", cDir)
		_ = os.WriteFile(filepath.Join(cDir, "payload.bin"), payloadData, 0o600)

		rep, err := Verify(context.Background(), VerifyConfig{
			Mode:         "complete",
			ManifestPath: manifestPath,
			AdminDir:     aDir,
			DownloadDir:  downloadDir,
			CompleteDir:  cDir,
		})
		if err != nil {
			t.Fatalf("expected auto-discovery in complete mode to succeed, got: %v", err)
		}
		if rep.JobID != "j-auto" {
			t.Errorf("got job ID %q, want j-auto", rep.JobID)
		}
	})
}

func TestCLI_Run(t *testing.T) {
	tempDir := t.TempDir()

	// Subtest 1: gen subcommand
	t.Run("gen happy", func(t *testing.T) {
		outDir := filepath.Join(tempDir, "gen-out")
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{
			"gen",
			"--dir", outDir,
			"--size", "1M",
			"--volumes", "2",
			"--part-size", "262144",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run gen returned %d, stderr: %s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"name": "release"`) {
			t.Errorf("stdout missing release manifest JSON: %s", stdout.String())
		}
	})

	// Subtest 2: gen with invalid size
	t.Run("gen invalid size", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"gen", "--dir", tempDir, "--size", "invalid"}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for invalid size")
		}
	})

	// Subtest 3: unknown subcommand
	t.Run("unknown subcommand", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"bogus"}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for unknown subcommand")
		}
	})

	// Subtest 4: missing subcommand
	t.Run("missing subcommand", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), nil, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for missing subcommand")
		}
	})

	// Subtest 5: verify subcommand happy
	t.Run("verify subcommand happy", func(t *testing.T) {
		adminDir := filepath.Join(tempDir, "admin-cli")
		downloadDir := filepath.Join(tempDir, "dl-cli")
		_ = os.MkdirAll(adminDir, 0o755)
		_ = os.MkdirAll(downloadDir, 0o755)
		db := createTestDB(t, adminDir)
		_, _ = db.Exec(`INSERT INTO dispatch_jobs (id, sort_key) VALUES (?, ?)`, "j-cli", 1)
		_, _ = db.Exec(`INSERT INTO job_files (job_id, file_index, filename, complete) VALUES (?, ?, ?, ?)`,
			"j-cli", 0, "file.rar", 0)

		payload := []byte("cli bytes")
		crc := crc32.ChecksumIEEE(payload)
		_, _ = db.Exec(`INSERT INTO written_articles (job_id, file_idx, art_idx, offset, length, crc32) VALUES (?, ?, ?, ?, ?, ?)`,
			"j-cli", 0, 0, 0, len(payload), crc)
		_ = os.WriteFile(filepath.Join(downloadDir, "file.rar"), payload, 0o600)

		manifest := &ReleaseManifest{
			Name:     "release",
			PartSize: len(payload),
			Files: []ReleaseFile{
				{
					FileIndex:   0,
					DiskName:    "file.rar",
					NZBFilename: "file.rar",
					Role:        RoleRARVolume,
					TotalSize:   int64(len(payload)),
					Segments:    []SegmentMeta{{MessageID: "<cli@test>", Length: len(payload), CRC32: crc}},
				},
			},
		}
		manifestPath := filepath.Join(tempDir, "manifest-cli.json")
		_ = SaveManifest(manifestPath, manifest)

		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{
			"verify",
			"--mode", "crash",
			"--manifest", manifestPath,
			"--admin-dir", adminDir,
			"--download-dir", downloadDir,
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run verify returned %d, stderr: %s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"verified_rows": 1`) {
			t.Errorf("stdout missing verified report: %s", stdout.String())
		}
	})

	// Subtest 6: serve with context cancellation
	t.Run("serve canceled", func(t *testing.T) {
		outDir := filepath.Join(tempDir, "serve-release")
		mkFilesDir(t, outDir)
		manifest := &ReleaseManifest{Name: "rel", PartSize: 1024}
		_ = SaveManifest(filepath.Join(outDir, "manifest.json"), manifest)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var stdout, stderr bytes.Buffer
		code := run(ctx, []string{
			"serve",
			"--dir", outDir,
			"--listen", "127.0.0.1:0",
			"--control", "127.0.0.1:0",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run serve returned %d, stderr: %s", code, stderr.String())
		}
	})

	// Subtest 7: CLI runGen errors
	t.Run("gen errors", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"gen", "--dir", ""}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for missing --dir")
		}

		code = run(context.Background(), []string{"gen", "-unknown-flag"}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for invalid flag")
		}

		code = run(context.Background(), []string{"gen", "--dir", tempDir, "--volumes", "-1"}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for invalid volumes")
		}
	})

	// Subtest 8: CLI runServe errors
	t.Run("serve errors", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"serve", "--dir", ""}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for missing --dir")
		}

		code = run(context.Background(), []string{"serve", "--dir", tempDir, "--fault", "invalid-fault"}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for invalid fault spec")
		}

		code = run(context.Background(), []string{"serve", "--dir", filepath.Join(tempDir, "nonexistent-dir")}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for missing manifest")
		}

		outDir := filepath.Join(tempDir, "serve-bad-control")
		_ = os.MkdirAll(outDir, 0o755)
		_ = SaveManifest(filepath.Join(outDir, "manifest.json"), &ReleaseManifest{Name: "rel", PartSize: 1024})
		code = run(context.Background(), []string{
			"serve",
			"--dir", outDir,
			"--listen", "127.0.0.1:0",
			"--control", "invalid:addr:port",
		}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for invalid control listen address")
		}
	})

	// Subtest 9: CLI runVerify errors
	t.Run("verify errors", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"verify", "--mode", ""}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for missing flags")
		}

		code = run(context.Background(), []string{"verify", "-unknown-flag"}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for invalid flag")
		}

		code = run(context.Background(), []string{
			"verify",
			"--mode", "crash",
			"--manifest", filepath.Join(tempDir, "nonexistent.json"),
			"--admin-dir", tempDir,
			"--download-dir", tempDir,
		}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("expected non-zero exit code for verification failure")
		}
	})
}

func TestVerify_ReviewFixesAndEdgeCases(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	manifest := &ReleaseManifest{
		Name:     "myrel",
		PartSize: 4,
		ExpectedOutputs: []ExpectedOutput{
			{Name: "payload.bin", Size: 4, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("data")))},
		},
		Files: []ReleaseFile{
			{
				FileIndex:   0,
				DiskName:    "myrel.part02.rar",
				NZBFilename: "myrel.part02.rar",
				Role:        RoleRARVolume,
				TotalSize:   4,
				Segments:    []SegmentMeta{{MessageID: "seg-p2@test", PartNum: 1, Length: 4}},
			},
			{
				FileIndex:   1,
				DiskName:    "myrel.part01.rar",
				NZBFilename: "myrel.part01.rar",
				Role:        RoleRARVolume,
				TotalSize:   4,
				Segments:    []SegmentMeta{{MessageID: "seg-p1@test", PartNum: 1, Length: 4}},
			},
		},
	}
	manifestPath := filepath.Join(tempDir, "manifest.json")
	if err := SaveManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}

	// 1. sortedManifestFiles puts part01 before part02 using job.SortJobFiles
	sorted := sortedManifestFiles(manifest)
	if len(sorted) != 2 || sorted[0].DiskName != "myrel.part01.rar" {
		t.Fatalf("sortedManifestFiles unexpected order: %+v", sorted)
	}
	if sortedManifestFiles(nil) != nil {
		t.Fatal("expected nil from sortedManifestFiles(nil)")
	}

	// 2. --snapshot-as and --snapshot require --server
	if _, err := Verify(ctx, VerifyConfig{
		Mode:         "crash",
		ManifestPath: manifestPath,
		AdminDir:     tempDir,
		SnapshotAs:   "snap1",
	}); err == nil || !strings.Contains(err.Error(), "--snapshot-as requires --server") {
		t.Fatalf("expected --snapshot-as error, got %v", err)
	}
	if _, err := Verify(ctx, VerifyConfig{
		Mode:            "complete",
		ManifestPath:    manifestPath,
		AdminDir:        tempDir,
		CompareSnapshot: "snap1",
	}); err == nil || !strings.Contains(err.Error(), "--snapshot requires --server") {
		t.Fatalf("expected --snapshot error, got %v", err)
	}

	// 3. resolveExplicitCrashJob & resolveCrashJob edge cases
	adminDir := filepath.Join(tempDir, "admin-multi")
	_ = os.MkdirAll(adminDir, 0o755)
	db := createTestDB(t, adminDir)
	_, _ = db.Exec(`ALTER TABLE dispatch_jobs ADD COLUMN name TEXT DEFAULT ''`)
	_, _ = db.Exec(`INSERT INTO dispatch_jobs (id, sort_key, name) VALUES ('j1', 1, 'myrel'), ('j2', 2, 'myrel')`)

	if _, _, err := resolveExplicitCrashJob(ctx, db, "j1"); err != nil {
		t.Fatalf("resolveExplicitCrashJob j1: %v", err)
	}
	if _, _, err := resolveExplicitCrashJob(ctx, db, "nonexistent"); err == nil {
		t.Fatal("expected error for nonexistent explicit job id")
	}
	_, _ = db.Exec(`INSERT INTO job_files (job_id, file_index, filename, complete) VALUES ('j-files-only', 0, 'a.rar', 0)`)
	if id, _, err := resolveExplicitCrashJob(ctx, db, "j-files-only"); err != nil || id != "j-files-only" {
		t.Fatalf("resolveExplicitCrashJob via job_files failed: id=%q err=%v", id, err)
	}

	// Multiple matching jobs in dispatch_jobs without --job-id fails closed
	if _, _, err := resolveCrashJob(ctx, db, "", "myrel"); err == nil || !strings.Contains(err.Error(), "multiple jobs") {
		t.Fatalf("expected multiple jobs error, got %v", err)
	}
	// Non-matching release name fails closed
	if _, _, err := resolveCrashJob(ctx, db, "", "other-rel"); err == nil || !strings.Contains(err.Error(), "no job matching") {
		t.Fatalf("expected no job matching error, got %v", err)
	}
	// Exact single match succeeds
	_, _ = db.Exec(`DELETE FROM dispatch_jobs WHERE id = 'j2'`)
	if id, name, err := resolveCrashJob(ctx, db, "", "myrel"); err != nil || id != "j1" || name != "myrel" {
		t.Fatalf("resolveCrashJob single match failed: id=%q name=%q err=%v", id, name, err)
	}
	// Single unnamed candidate in filterMatchingCandidates matches as test fixture fallback
	cands := []dispatchJobCandidate{{id: "jr", name: ""}}
	if matched := filterMatchingCandidates(cands, "custom"); len(matched) != 1 || matched[0].id != "jr" {
		t.Fatalf("expected single unnamed candidate fallback, got %+v", matched)
	}
	// Empty dispatch_jobs returns no job found error
	_, _ = db.Exec(`DELETE FROM dispatch_jobs`)
	if _, _, err := resolveCrashJob(ctx, db, "", "myrel"); err == nil || !strings.Contains(err.Error(), "no job found") {
		t.Fatalf("expected no job found error from empty dispatch_jobs, got %v", err)
	}

	// 4. resolveCompleteHistory edge cases
	_, _ = db.Exec(`ALTER TABLE history ADD COLUMN name TEXT DEFAULT ''`)
	_, _ = db.Exec(`INSERT INTO history (nzo_id, name, status, storage) VALUES ('h1', 'myrel', 'Completed', '/tmp/a'), ('h2', 'myrel', 'Completed', '/tmp/b')`)
	if _, err := resolveCompleteHistory(ctx, db, "", "myrel"); err == nil || !strings.Contains(err.Error(), "multiple history rows") {
		t.Fatalf("expected multiple history rows error, got %v", err)
	}
	if _, err := resolveCompleteHistory(ctx, db, "", "unmatched"); err == nil || !strings.Contains(err.Error(), "no history row matching") {
		t.Fatalf("expected no history row matching error, got %v", err)
	}
	_, _ = db.Exec(`DELETE FROM history WHERE nzo_id = 'h2'`)
	if h, err := resolveCompleteHistory(ctx, db, "", "myrel"); err != nil || h.jobID != "h1" {
		t.Fatalf("expected single history match h1, got %+v err=%v", h, err)
	}
	// Single unnamed history row fallback in resolveCompleteHistory
	_, _ = db.Exec(`UPDATE history SET name = '' WHERE nzo_id = 'h1'`)
	if h, err := resolveCompleteHistory(ctx, db, "", "custom"); err != nil || h.jobID != "h1" {
		t.Fatalf("expected single unnamed history row fallback, got %+v err=%v", h, err)
	}

	// 5. candidateCompleteDirs when storage/path point to a file vs directory
	completeDir := filepath.Join(tempDir, "complete-cand")
	_ = os.MkdirAll(completeDir, 0o755)
	fileStorage := filepath.Join(completeDir, "payload.bin")
	_ = os.WriteFile(fileStorage, []byte("data"), 0o600)
	dirs := candidateCompleteDirs(completeDir, "myrel", historyRecord{
		jobID:   "h1",
		name:    "myrel",
		storage: fileStorage,
		path:    fileStorage,
	})
	if len(dirs) == 0 || dirs[0] != completeDir {
		t.Fatalf("expected parent dir of fileStorage, got %v", dirs)
	}

	// 6. SkipArchiveCleanup allows .rar/.par2 in completeDir, while empty incomplete dir still fails
	if err := checkCompleteHygiene([]string{completeDir}, true); err != nil {
		t.Fatalf("checkCompleteHygiene with SkipArchiveCleanup=true failed: %v", err)
	}
	incDir := filepath.Join(tempDir, "inc-empty")
	_ = os.MkdirAll(filepath.Join(incDir, "myrel"), 0o755)
	if err := checkIncompleteHygiene(incDir, "myrel", "myrel", "h1"); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("expected empty incomplete dir to fail hygiene check, got %v", err)
	}
	// When downloadDir's basename equals the job name ("myrel") and the job subdir is gone, verification must pass.
	sameBaseDownloadDir := filepath.Join(tempDir, "dl-base", "myrel")
	_ = os.MkdirAll(sameBaseDownloadDir, 0o755)
	if err := checkIncompleteHygiene(sameBaseDownloadDir, "myrel", "myrel", "h1"); err != nil {
		t.Fatalf("expected checkIncompleteHygiene to pass when downloadDir basename equals job name and job subdir is gone, got %v", err)
	}

	// 7. compareSnapshotRefetches detects faulted re-fetch via Requests map and handles error statuses
	mkFilesDir(t, tempDir)
	srv, err := NewServer(ServerConfig{
		Releases: []ReleaseSource{{Dir: tempDir, Manifest: manifest}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.recordRequest("seg-p1@test")
	srv.TakeSnapshot("snap-req", []string{"seg-p1@test"})
	// Simulate a post-crash re-fetch that hit a fault (so Requests increases while ServedClean does not).
	srv.recordRequest("seg-p1@test")

	ctrlSrv := httptest.NewServer(NewControlHandler(srv))
	defer ctrlSrv.Close()

	refetched, err := compareSnapshotRefetches(ctx, ctrlSrv.URL, "snap-req")
	if err != nil {
		t.Fatalf("compareSnapshotRefetches: %v", err)
	}
	if len(refetched) != 1 || refetched[0] != "seg-p1@test" {
		t.Fatalf("expected seg-p1@test in refetchedIDs, got %v", refetched)
	}
	if _, err := compareSnapshotRefetches(ctx, ctrlSrv.URL, "missing-snap"); err == nil {
		t.Fatal("expected error for missing snapshot")
	}
	if err := postSnapshot(ctx, ctrlSrv.URL, "", []string{"seg-p1@test"}); err == nil {
		t.Fatal("expected error when posting snapshot with empty name")
	}

	// Closed DB error paths for queryDispatchCandidates and queryHistoryRecords
	_ = db.Close()
	if _, err := queryDispatchCandidates(ctx, db); err == nil {
		t.Fatal("expected error from queryDispatchCandidates on closed DB")
	}
	if _, err := queryHistoryRecords(ctx, db, "h1"); err == nil {
		t.Fatal("expected error from queryHistoryRecords on closed DB")
	}
}

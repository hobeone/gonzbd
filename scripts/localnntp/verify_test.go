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
	"time"

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
		sort_key INTEGER
	);
	CREATE TABLE job_files (
		job_id TEXT,
		file_index INTEGER,
		filename TEXT,
		complete INTEGER,
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
	srv, err := NewServer(ServerConfig{
		ReleaseDir: tempDir,
		Manifest:   manifest,
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
		p, err := findPartialFile(downloadDir, "job-sub", "sub.rar")
		if err != nil || !strings.HasSuffix(p, filepath.Join("job-sub", "sub.rar")) {
			t.Fatalf("findPartialFile jobSubdir failed: p=%s err=%v", p, err)
		}

		nestedSubdir := filepath.Join(downloadDir, "nested", "deep")
		_ = os.MkdirAll(nestedSubdir, 0o755)
		_ = os.WriteFile(filepath.Join(nestedSubdir, "deep.rar"), []byte("deep"), 0o600)
		p, err = findPartialFile(downloadDir, "", "deep.rar")
		if err != nil || !strings.HasSuffix(p, filepath.Join("nested", "deep", "deep.rar")) {
			t.Fatalf("findPartialFile walk failed: p=%s err=%v", p, err)
		}

		_, err = findPartialFile(downloadDir, "", "nonexistent.rar")
		if err == nil {
			t.Fatal("expected error for nonexistent partial file")
		}
	})
}

func TestVerify_Complete_HappyPath(t *testing.T) {
	tempDir := t.TempDir()
	adminDir := filepath.Join(tempDir, "admin")
	downloadDir := filepath.Join(tempDir, "incomplete")
	completeDir := filepath.Join(tempDir, "complete")
	_ = os.MkdirAll(adminDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)
	_ = os.MkdirAll(completeDir, 0o755)

	db := createTestDB(t, adminDir)
	jobID := "job-comp-1"
	jobStorage := filepath.Join(completeDir, "myrelease")
	_ = os.MkdirAll(jobStorage, 0o755)

	if _, err := db.Exec(`INSERT INTO history (nzo_id, status, storage, path, fail_message) VALUES (?, ?, ?, ?, ?)`,
		jobID, "Completed", jobStorage, jobStorage, ""); err != nil {
		t.Fatal(err)
	}

	payloadData := []byte("the complete extracted file content")
	hash := sha256.Sum256(payloadData)
	hashHex := fmt.Sprintf("%x", hash)
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
				SHA256: hashHex,
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

	// Setup mock server
	srv, err := NewServer(ServerConfig{
		ReleaseDir: tempDir,
		Manifest:   manifest,
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

	cfg := VerifyConfig{
		Mode:             "complete",
		ManifestPath:     manifestPath,
		AdminDir:         adminDir,
		DownloadDir:      downloadDir,
		CompleteDir:      completeDir,
		JobID:            jobID,
		ServerControlURL: controlSrv.URL,
		CompareSnapshot:  "crash1",
	}

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
		_ = os.MkdirAll(outDir, 0o755)
		manifest := &ReleaseManifest{Name: "rel", PartSize: 1024}
		_ = SaveManifest(filepath.Join(outDir, "manifest.json"), manifest)

		ctx, cancel := context.WithCancel(context.Background())
		// Cancel shortly after starting
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

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

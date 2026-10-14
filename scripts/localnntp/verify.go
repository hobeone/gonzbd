// Package main implements the localnntp release generator, NNTP server, and crash verifier.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/hobeone/gonzbd/internal/job"
)

// VerifyConfig configures crash or completion verification against gonzbd disk state.
type VerifyConfig struct {
	Mode             string
	ManifestPath     string
	AdminDir         string
	DownloadDir      string
	CompleteDir      string
	JobID            string
	ServerControlURL string
	SnapshotAs       string
	CompareSnapshot  string
}

// VerifyReport summarizes the verification results.
type VerifyReport struct {
	Mode            string   `json:"mode"`
	JobID           string   `json:"job_id"`
	VerifiedRows    int      `json:"verified_rows"`
	RecordedIDs     []string `json:"recorded_ids,omitempty"`
	RefetchedIDs    []string `json:"refetched_ids,omitempty"`
	OutputsVerified int      `json:"outputs_verified"`
}

const tblJobFiles = "job_files"

type writtenArtRow struct {
	fileIdx int32
	artIdx  int32
	offset  int64
	length  int64
	crc32   uint32
}

// Verify checks the database and disk state of gonzbd after a crash or upon completion.
func Verify(ctx context.Context, cfg VerifyConfig) (*VerifyReport, error) {
	if cfg.ManifestPath == "" {
		return nil, errors.New("manifest path is required")
	}
	m, err := LoadManifest(cfg.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}

	if cfg.AdminDir == "" {
		return nil, errors.New("admin dir is required")
	}
	dbPath := filepath.Join(cfg.AdminDir, "history.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	defer func() {
		_ = db.Close() //nolint:errcheck // best effort close on exit
	}()

	switch cfg.Mode {
	case "crash":
		return verifyCrash(ctx, db, m, cfg)
	case "complete":
		return verifyComplete(ctx, db, m, cfg)
	default:
		return nil, fmt.Errorf("unknown mode: %q (expected crash or complete)", cfg.Mode)
	}
}

func resolveJobID(ctx context.Context, db *sql.DB, explicitID string) (string, error) {
	if explicitID != "" {
		return explicitID, nil
	}

	var jobID string
	err := db.QueryRowContext(ctx, `SELECT id FROM dispatch_jobs ORDER BY sort_key DESC LIMIT 1`).Scan(&jobID)
	if err == nil && jobID != "" {
		return jobID, nil
	}

	err = db.QueryRowContext(ctx, fmt.Sprintf("SELECT DISTINCT job_id FROM %s LIMIT 1", tblJobFiles)).Scan(&jobID)
	if err == nil && jobID != "" {
		return jobID, nil
	}

	return "", errors.New("no job found in dispatch_jobs or job_files")
}

func resolveArticleIDs(adminDir, jobID string, m *ReleaseManifest) (func(artIdx int) (string, bool), error) {
	manifestFile := filepath.Join(adminDir, "queue", "manifests", jobID+".json.gz")
	f, err := os.Open(manifestFile) //nolint:gosec // manifest path within adminDir
	if err == nil {
		defer func() { _ = f.Close() }() //nolint:errcheck // best effort close
		gz, gzErr := gzip.NewReader(f)
		if gzErr == nil {
			data, readErr := io.ReadAll(gz)
			_ = gz.Close() //nolint:errcheck // best effort close
			if readErr == nil {
				var jm job.Manifest
				if unmarshalErr := json.Unmarshal(data, &jm); unmarshalErr == nil {
					return func(artIdx int) (string, bool) {
						id := jm.ArticleID(artIdx)
						return id, id != ""
					}, nil
				}
			}
		}
	}

	// Fallback: flatten segments from ReleaseManifest in order.
	var fallbackIDs []string
	for _, file := range m.Files {
		for _, seg := range file.Segments {
			fallbackIDs = append(fallbackIDs, seg.MessageID)
		}
	}
	return func(artIdx int) (string, bool) {
		if artIdx >= 0 && artIdx < len(fallbackIDs) {
			return fallbackIDs[artIdx], true
		}
		return "", false
	}, nil
}

func findPartialFile(downloadDir, jobName, filename string) (string, error) {
	// 1. Direct match in downloadDir
	direct := filepath.Join(downloadDir, filename)
	if fi, err := os.Stat(direct); err == nil && !fi.IsDir() {
		return direct, nil
	}

	// 2. Match in downloadDir/jobName
	if jobName != "" {
		jobPath := filepath.Join(downloadDir, jobName, filename)
		if fi, err := os.Stat(jobPath); err == nil && !fi.IsDir() {
			return jobPath, nil
		}
	}

	// 3. Recursive walk of downloadDir
	var foundPath string
	err := filepath.WalkDir(downloadDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && d.Name() == filename {
			foundPath = path
			return fs.SkipAll
		}
		return nil
	})
	if err == nil && foundPath != "" {
		return foundPath, nil
	}

	return "", fmt.Errorf("partial file %q not found in download dir %s", filename, downloadDir)
}

func verifyCrash(ctx context.Context, db *sql.DB, m *ReleaseManifest, cfg VerifyConfig) (*VerifyReport, error) {
	jobID, err := resolveJobID(ctx, db, cfg.JobID)
	if err != nil {
		return nil, fmt.Errorf("resolve job id: %w", err)
	}

	// Read job_files
	fileQuery := fmt.Sprintf("SELECT file_index, COALESCE(filename, '') FROM %s WHERE job_id = ? ORDER BY file_index", tblJobFiles)
	fileRows, err := db.QueryContext(ctx, fileQuery, jobID)
	if err != nil {
		return nil, fmt.Errorf("query job_files: %w", err)
	}
	defer func() { _ = fileRows.Close() }()

	fileNames := make(map[int32]string)
	for fileRows.Next() {
		var idx int32
		var name string
		if err := fileRows.Scan(&idx, &name); err != nil {
			return nil, fmt.Errorf("scan job_files: %w", err)
		}
		fileNames[idx] = name
	}
	if err := fileRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate job_files: %w", err)
	}

	// Read written_articles
	artRows, err := db.QueryContext(ctx, `SELECT file_idx, art_idx, offset, length, crc32 FROM written_articles WHERE job_id = ? ORDER BY file_idx, offset`, jobID)
	if err != nil {
		return nil, fmt.Errorf("query written_articles: %w", err)
	}
	defer func() { _ = artRows.Close() }()

	var written []writtenArtRow
	for artRows.Next() {
		var r writtenArtRow
		if err := artRows.Scan(&r.fileIdx, &r.artIdx, &r.offset, &r.length, &r.crc32); err != nil {
			return nil, fmt.Errorf("scan written_articles: %w", err)
		}
		written = append(written, r)
	}
	if err := artRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate written_articles: %w", err)
	}

	artIDResolver, err := resolveArticleIDs(cfg.AdminDir, jobID, m)
	if err != nil {
		return nil, fmt.Errorf("resolve article ids: %w", err)
	}

	// Open partial files and verify each written article's bytes and CRC32
	openFiles := make(map[int32]*os.File)
	defer func() {
		for _, f := range openFiles {
			_ = f.Close() //nolint:errcheck // best-effort file close
		}
	}()

	var recordedIDs []string
	for _, r := range written {
		f, ok := openFiles[r.fileIdx]
		filename := fileNames[r.fileIdx]
		if filename == "" && int(r.fileIdx) < len(m.Files) {
			filename = m.Files[r.fileIdx].DiskName
		}
		if filename == "" {
			filename = fmt.Sprintf("file_%d", r.fileIdx)
		}

		if !ok {
			filePath, err := findPartialFile(cfg.DownloadDir, m.Name, filename)
			if err != nil {
				return nil, err
			}
			opened, err := os.Open(filePath) //nolint:gosec // partial file within downloadDir
			if err != nil {
				return nil, fmt.Errorf("open partial file %s: %w", filePath, err)
			}
			openFiles[r.fileIdx] = opened
			f = opened
		}

		buf := make([]byte, r.length)
		n, err := f.ReadAt(buf, r.offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("partial file %q read error: %w", filename, err)
		}
		if int64(n) < r.length {
			return nil, fmt.Errorf("partial file %q truncated: read %d bytes at offset %d, want %d", filename, n, r.offset, r.length)
		}

		gotCRC := crc32.ChecksumIEEE(buf)
		if gotCRC != r.crc32 {
			return nil, fmt.Errorf("crc32 mismatch for file %d (%s) art %d offset %d length %d: got %08x, want %08x",
				r.fileIdx, filename, r.artIdx, r.offset, r.length, gotCRC, r.crc32)
		}

		if rawMsgID, ok := artIDResolver(int(r.artIdx)); ok && rawMsgID != "" {
			cleanID := extractMessageID(rawMsgID)
			recordedIDs = append(recordedIDs, cleanID)
		}
	}

	slices.Sort(recordedIDs)
	recordedIDs = slices.Compact(recordedIDs)

	if cfg.ServerControlURL != "" && cfg.SnapshotAs != "" {
		snapshotURL := fmt.Sprintf("%s/snapshot?name=%s", strings.TrimRight(cfg.ServerControlURL, "/"), url.QueryEscape(cfg.SnapshotAs))
		reqBody, err := json.Marshal(map[string]any{
			"name":         cfg.SnapshotAs,
			"recorded_ids": recordedIDs,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal snapshot request: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, snapshotURL, bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("create snapshot request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("post snapshot %s: %w", snapshotURL, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body) //nolint:errcheck // body reading for error message
			return nil, fmt.Errorf("post snapshot returned status %d: %s", resp.StatusCode, string(body))
		}
	}

	return &VerifyReport{
		Mode:         "crash",
		JobID:        jobID,
		VerifiedRows: len(written),
		RecordedIDs:  recordedIDs,
	}, nil
}

func findExpectedOutputFile(searchDirs []string, name string) (string, error) {
	for _, dir := range searchDirs {
		if dir == "" {
			continue
		}
		direct := filepath.Join(dir, name)
		if fi, err := os.Stat(direct); err == nil && !fi.IsDir() {
			return direct, nil
		}

		// Also walk dir if not directly at root
		var found string
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !d.IsDir() && d.Name() == name {
				found = path
				return fs.SkipAll
			}
			return nil
		})
		if err == nil && found != "" {
			return found, nil
		}
	}
	return "", fmt.Errorf("expected output %q not found in complete directories", name)
}

func verifyComplete(ctx context.Context, db *sql.DB, m *ReleaseManifest, cfg VerifyConfig) (*VerifyReport, error) {
	var jobID, status, storage, path, failMessage string
	var err error

	if cfg.JobID != "" {
		err = db.QueryRowContext(ctx, `SELECT nzo_id, status, COALESCE(storage, ''), COALESCE(path, ''), COALESCE(fail_message, '') FROM history WHERE nzo_id = ?`, cfg.JobID).
			Scan(&jobID, &status, &storage, &path, &failMessage)
	} else {
		err = db.QueryRowContext(ctx, `SELECT nzo_id, status, COALESCE(storage, ''), COALESCE(path, ''), COALESCE(fail_message, '') FROM history ORDER BY id DESC LIMIT 1`).
			Scan(&jobID, &status, &storage, &path, &failMessage)
	}
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}

	if status != "Completed" {
		return nil, fmt.Errorf("job %s status is %q (fail message: %s)", jobID, status, failMessage)
	}

	// Verify job is no longer in dispatch_jobs
	var dispatchCount int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dispatch_jobs WHERE id = ?`, jobID).Scan(&dispatchCount)
	if err == nil && dispatchCount > 0 {
		return nil, fmt.Errorf("job %s still present in dispatch_jobs (%d rows)", jobID, dispatchCount)
	}

	// Verify expected outputs
	searchDirs := []string{storage, path, cfg.CompleteDir}
	outputsVerified := 0
	for _, eo := range m.ExpectedOutputs {
		outPath, err := findExpectedOutputFile(searchDirs, eo.Name)
		if err != nil {
			return nil, err
		}

		fi, err := os.Stat(outPath)
		if err != nil {
			return nil, fmt.Errorf("stat output file %s: %w", outPath, err)
		}
		if fi.Size() != eo.Size {
			return nil, fmt.Errorf("output file %s size mismatch: got %d, want %d", outPath, fi.Size(), eo.Size)
		}

		f, err := os.Open(outPath) //nolint:gosec // output file within verified search dir
		if err != nil {
			return nil, fmt.Errorf("open output file %s: %w", outPath, err)
		}
		hasher := sha256.New()
		if _, err := io.Copy(hasher, f); err != nil {
			_ = f.Close() //nolint:errcheck // best-effort close on error
			return nil, fmt.Errorf("hash output file %s: %w", outPath, err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close output file %s: %w", outPath, err)
		}

		gotHash := fmt.Sprintf("%x", hasher.Sum(nil))
		if !strings.EqualFold(gotHash, eo.SHA256) {
			return nil, fmt.Errorf("sha256 mismatch for output %s: got %s, want %s", eo.Name, gotHash, eo.SHA256)
		}
		outputsVerified++
	}

	// Post-processing hygiene checks on complete directory
	if cfg.CompleteDir != "" {
		err := filepath.WalkDir(cfg.CompleteDir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			name := d.Name()
			if strings.Contains(name, ".gonzbd-tmp-") || strings.Contains(name, ".gonzbd-pending-deletions") {
				return fmt.Errorf("hygiene violation: temporary artifact %s found in complete dir", path)
			}
			if !d.IsDir() && (strings.HasSuffix(name, ".rar") || strings.HasSuffix(name, ".par2")) {
				return fmt.Errorf("hygiene violation: archive/parity file %s found in complete dir", path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// Post-processing hygiene checks on download directory
	if cfg.DownloadDir != "" {
		checkDirs := []string{
			filepath.Join(cfg.DownloadDir, m.Name),
			filepath.Join(cfg.DownloadDir, jobID),
		}
		if filepath.Base(cfg.DownloadDir) == m.Name || filepath.Base(cfg.DownloadDir) == jobID {
			checkDirs = append(checkDirs, cfg.DownloadDir)
		}
		for _, dir := range checkDirs {
			entries, err := os.ReadDir(dir)
			if err == nil && len(entries) > 0 {
				return nil, fmt.Errorf("hygiene violation: incomplete directory %s still exists with %d files", dir, len(entries))
			}
		}
	}

	var refetchedIDs []string
	if cfg.ServerControlURL != "" && cfg.CompareSnapshot != "" {
		baseURL := strings.TrimRight(cfg.ServerControlURL, "/")
		client := &http.Client{Timeout: 5 * time.Second}

		// GET snapshot
		snapURL := fmt.Sprintf("%s/snapshot?name=%s", baseURL, url.QueryEscape(cfg.CompareSnapshot))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, snapURL, http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("create snapshot request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch snapshot %s: %w", snapURL, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch snapshot returned status %d", resp.StatusCode)
		}
		var snap Snapshot
		if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
			return nil, fmt.Errorf("decode snapshot: %w", err)
		}

		// GET stats
		statsURL := baseURL + "/stats"
		reqStats, err := http.NewRequestWithContext(ctx, http.MethodGet, statsURL, http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("create stats request: %w", err)
		}
		respStats, err := client.Do(reqStats)
		if err != nil {
			return nil, fmt.Errorf("fetch stats %s: %w", statsURL, err)
		}
		defer func() { _ = respStats.Body.Close() }()
		if respStats.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch stats returned status %d", respStats.StatusCode)
		}
		var stats ServerStats
		if err := json.NewDecoder(respStats.Body).Decode(&stats); err != nil {
			return nil, fmt.Errorf("decode stats: %w", err)
		}

		for _, id := range snap.RecordedIDs {
			cleanID := extractMessageID(id)
			curCount := stats.ServedClean[cleanID]
			snapCount := snap.ServedClean[cleanID]
			if curCount > snapCount {
				refetchedIDs = append(refetchedIDs, cleanID)
			}
		}

		if len(refetchedIDs) > 0 {
			slices.Sort(refetchedIDs)
			return &VerifyReport{
				Mode:            "complete",
				JobID:           jobID,
				OutputsVerified: outputsVerified,
				RefetchedIDs:    refetchedIDs,
			}, fmt.Errorf("refetched already-recorded articles after restart: %v", refetchedIDs)
		}
	}

	return &VerifyReport{
		Mode:            "complete",
		JobID:           jobID,
		OutputsVerified: outputsVerified,
	}, nil
}

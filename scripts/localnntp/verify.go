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

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
)

// VerifyConfig configures crash or completion verification against gonzbd disk state.
type VerifyConfig struct {
	Mode               string
	ManifestPath       string
	AdminDir           string
	DownloadDir        string
	CompleteDir        string
	JobID              string
	ServerControlURL   string
	SnapshotAs         string
	CompareSnapshot    string
	SkipArchiveCleanup bool
}

// VerifyReport summarizes the verification results.
type VerifyReport struct {
	Mode            string   `json:"mode"`
	JobID           string   `json:"job_id"`
	VerifiedRows    int      `json:"verified_rows"`
	FallbackRows    int      `json:"fallback_rows,omitempty"`
	RecordedIDs     []string `json:"recorded_ids,omitempty"`
	RefetchedIDs    []string `json:"refetched_ids,omitempty"`
	OutputsVerified int      `json:"outputs_verified"`
}

// Verify checks the database and disk state of gonzbd after a crash or upon completion.
func Verify(ctx context.Context, cfg VerifyConfig) (*VerifyReport, error) {
	if cfg.ManifestPath == "" {
		return nil, errors.New("manifest path is required")
	}
	if cfg.AdminDir == "" {
		return nil, errors.New("admin dir is required")
	}
	if cfg.SnapshotAs != "" && cfg.ServerControlURL == "" {
		return nil, errors.New("--snapshot-as requires --server")
	}
	if cfg.CompareSnapshot != "" && cfg.ServerControlURL == "" {
		return nil, errors.New("--snapshot requires --server")
	}

	m, err := LoadManifest(cfg.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
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

func sortedManifestFiles(m *ReleaseManifest) []ReleaseFile {
	if m == nil || len(m.Files) == 0 {
		return nil
	}
	jf := make([]job.JobFile, len(m.Files))
	for i, rf := range m.Files {
		subj := rf.NZBFilename
		if subj == "" {
			subj = rf.DiskName
		}
		jf[i] = job.JobFile{
			Subject: subj,
			Bytes:   int64(i),
		}
	}
	job.SortJobFiles(jf)
	out := make([]ReleaseFile, len(m.Files))
	for i, f := range jf {
		out[i] = m.Files[int(f.Bytes)]
	}
	return out
}

type dispatchJobCandidate struct {
	id   string
	name string
}

func queryDispatchCandidates(ctx context.Context, db *sql.DB) ([]dispatchJobCandidate, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name FROM dispatch_jobs ORDER BY sort_key ASC, id ASC`)
	if err != nil {
		rows, err = db.QueryContext(ctx, `SELECT id, '' FROM dispatch_jobs ORDER BY sort_key ASC, id ASC`)
		if err != nil {
			return nil, fmt.Errorf("query dispatch_jobs: %w", err)
		}
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // best-effort close

	var out []dispatchJobCandidate
	for rows.Next() {
		var c dispatchJobCandidate
		var name sql.NullString
		if err := rows.Scan(&c.id, &name); err != nil {
			return nil, fmt.Errorf("scan dispatch_jobs: %w", err)
		}
		c.name = name.String
		out = append(out, c)
	}
	return out, rows.Err()
}

func filterMatchingCandidates(all []dispatchJobCandidate, releaseName string) []dispatchJobCandidate {
	var exact []dispatchJobCandidate
	for _, c := range all {
		if releaseName != "" && c.name == releaseName {
			exact = append(exact, c)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	if len(all) == 1 && all[0].name == "" {
		return all
	}
	return nil
}

func resolveExplicitCrashJob(ctx context.Context, db *sql.DB, explicitID string) (id, name string, err error) {
	candidates, err := queryDispatchCandidates(ctx, db)
	if err != nil {
		return "", "", err
	}
	for _, c := range candidates {
		if c.id == explicitID {
			return c.id, c.name, nil
		}
	}
	fRows, err := durability.NewStore(db).FileRows(ctx, explicitID)
	if err != nil {
		return "", "", fmt.Errorf("check job_files for %q: %w", explicitID, err)
	}
	if len(fRows) > 0 {
		return explicitID, "", nil
	}
	return "", "", fmt.Errorf("job %q not found in dispatch_jobs or job_files", explicitID)
}

func resolveCrashJob(ctx context.Context, db *sql.DB, explicitID, releaseName string) (id, name string, err error) {
	if explicitID != "" {
		return resolveExplicitCrashJob(ctx, db, explicitID)
	}

	candidates, err := queryDispatchCandidates(ctx, db)
	if err != nil {
		return "", "", err
	}
	if len(candidates) == 0 {
		return "", "", errors.New("no job found in dispatch_jobs")
	}
	matched := filterMatchingCandidates(candidates, releaseName)
	if len(matched) == 1 {
		return matched[0].id, matched[0].name, nil
	}
	if len(matched) > 1 {
		return "", "", fmt.Errorf("multiple jobs (%d) match release %q in dispatch_jobs; specify --job-id", len(matched), releaseName)
	}
	return "", "", fmt.Errorf("no job matching release %q found in dispatch_jobs", releaseName)
}

func readGzipQueueManifest(manifestFile string) (*job.Manifest, error) {
	f, err := os.Open(manifestFile) //nolint:gosec // manifest path within adminDir
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best effort close

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("open gzip queue manifest %s: %w", manifestFile, err)
	}
	data, readErr := io.ReadAll(gz)
	_ = gz.Close() //nolint:errcheck // best effort close
	if readErr != nil {
		return nil, fmt.Errorf("read gzip queue manifest %s: %w", manifestFile, readErr)
	}
	var jm job.Manifest
	if err := json.Unmarshal(data, &jm); err != nil {
		return nil, fmt.Errorf("unmarshal queue manifest %s: %w", manifestFile, err)
	}
	return &jm, nil
}

func resolveArticleIDs(adminDir, jobID string, m *ReleaseManifest) (func(artIdx int) (string, bool), error) {
	manifestFile := filepath.Join(adminDir, "queue", "manifests", jobID+".json.gz")
	jm, err := readGzipQueueManifest(manifestFile)
	if err == nil {
		return func(artIdx int) (string, bool) {
			if artIdx < 0 || artIdx >= jm.NumArticles() {
				return "", false
			}
			id := jm.ArticleID(artIdx)
			return id, id != ""
		}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("open queue manifest %s: %w", manifestFile, err)
	}

	sortedFiles := sortedManifestFiles(m)
	var fallbackIDs []string
	for _, file := range sortedFiles {
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

func walkDirForFile(root, filename string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && d.Name() == filename {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("walk %s: %w", root, err)
	}
	return found, nil
}

func findPartialFile(downloadDir string, subdirs []string, filename string) (string, error) {
	direct := filepath.Join(downloadDir, filename)
	if fi, err := os.Stat(direct); err == nil && !fi.IsDir() {
		return direct, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat %s: %w", direct, err)
	}

	seen := make(map[string]bool)
	for _, sub := range subdirs {
		if sub == "" || seen[sub] {
			continue
		}
		seen[sub] = true
		subRoot := filepath.Join(downloadDir, sub)
		candidate := filepath.Join(subRoot, filename)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate, nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat %s: %w", candidate, err)
		}
		found, err := walkDirForFile(subRoot, filename)
		if err != nil {
			return "", err
		}
		if found != "" {
			return found, nil
		}
	}

	return "", fmt.Errorf("partial file %q not found in download dir %s", filename, downloadDir)
}

func verifyWrittenRowOnDisk(f *os.File, filename string, r durability.WrittenRow) error {
	buf := make([]byte, r.Length)
	n, err := f.ReadAt(buf, r.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("partial file %q read error: %w", filename, err)
	}
	if int64(n) < r.Length {
		return fmt.Errorf("partial file %q truncated: read %d bytes at offset %d, want %d", filename, n, r.Offset, r.Length)
	}

	gotCRC := crc32.ChecksumIEEE(buf)
	if gotCRC != r.CRC32 {
		return fmt.Errorf("crc32 mismatch for file %d (%s) art %d offset %d length %d: got %08x, want %08x",
			r.FileIdx, filename, r.ArtIdx, r.Offset, r.Length, gotCRC, r.CRC32)
	}
	return nil
}

func postSnapshot(ctx context.Context, controlURL, snapshotName string, recordedIDs []string) error {
	snapshotURL := fmt.Sprintf("%s/snapshot?name=%s", strings.TrimRight(controlURL, "/"), url.QueryEscape(snapshotName))
	reqBody, err := json.Marshal(map[string]any{
		"name":         snapshotName,
		"recorded_ids": recordedIDs,
	})
	if err != nil {
		return fmt.Errorf("marshal snapshot request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, snapshotURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create snapshot request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post snapshot %s: %w", snapshotURL, err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close of HTTP response body
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body) //nolint:errcheck // body reading for error message
		return fmt.Errorf("post snapshot returned status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func manifestFileAt(sortedFiles []ReleaseFile, idx int) *ReleaseFile {
	if idx >= 0 && idx < len(sortedFiles) {
		return &sortedFiles[idx]
	}
	return nil
}

func resolveRowFilename(fr durability.FileRow, rf *ReleaseFile, idx int) string {
	if fr.Filename != "" {
		return fr.Filename
	}
	if rf != nil && rf.DiskName != "" {
		return rf.DiskName
	}
	return fmt.Sprintf("file_%d", idx)
}

func openCachedPartialFile(
	downloadDir string,
	candidateSubdirs []string,
	openFiles map[int]*os.File,
	fileIdx int,
	filename string,
) (*os.File, error) {
	if f, ok := openFiles[fileIdx]; ok {
		return f, nil
	}
	filePath, err := findPartialFile(downloadDir, candidateSubdirs, filename)
	if err != nil {
		return nil, err
	}
	opened, err := os.Open(filePath) //nolint:gosec // partial file within downloadDir
	if err != nil {
		return nil, fmt.Errorf("open partial file %s: %w", filePath, err)
	}
	openFiles[fileIdx] = opened
	return opened, nil
}

func postProcCandidateNames(filename string, rf *ReleaseFile) []string {
	names := []string{filename, filename + ".1"}
	if rf != nil && rf.DiskName != "" && rf.DiskName != filename {
		names = append(names, rf.DiskName, rf.DiskName+".1")
	}
	return names
}

func indexSegmentMetaByOffset(rf *ReleaseFile) map[int64]SegmentMeta {
	if rf == nil || len(rf.Segments) == 0 {
		return nil
	}
	byOffset := make(map[int64]SegmentMeta, len(rf.Segments))
	for _, seg := range rf.Segments {
		byOffset[seg.Offset] = seg
	}
	return byOffset
}

func hasPar2BackupCandidate(downloadDir string, candidateSubdirs []string, filename string, rf *ReleaseFile) bool {
	if _, err := findPartialFile(downloadDir, candidateSubdirs, filename+".1"); err == nil {
		return true
	}
	if rf != nil && rf.DiskName != "" && rf.DiskName != filename {
		if _, err := findPartialFile(downloadDir, candidateSubdirs, rf.DiskName+".1"); err == nil {
			return true
		}
	}
	return false
}

func allowedRowCRCs(r durability.WrittenRow, segByOffset map[int64]SegmentMeta, hasPar2Backup bool) []uint32 {
	crcs := []uint32{r.CRC32}
	if !hasPar2Backup {
		return crcs
	}
	if seg, ok := segByOffset[r.Offset]; ok && int64(seg.Length) == r.Length && seg.CRC32 != r.CRC32 {
		crcs = append(crcs, seg.CRC32)
	}
	return crcs
}

func verifyAllRowsAgainstCandidate(
	f *os.File,
	rows []durability.WrittenRow,
	segByOffset map[int64]SegmentMeta,
	hasPar2Backup bool,
) bool {
	for _, r := range rows {
		buf := make([]byte, r.Length)
		n, err := f.ReadAt(buf, r.Offset)
		if (err != nil && !errors.Is(err, io.EOF)) || int64(n) < r.Length {
			return false
		}
		got := crc32.ChecksumIEEE(buf)
		crcs := allowedRowCRCs(r, segByOffset, hasPar2Backup)
		if !slices.Contains(crcs, got) {
			return false
		}
	}
	return true
}

func resolveCompletePostProcFile(
	downloadDir string,
	candidateSubdirs []string,
	openFiles map[int]*os.File,
	fileIdx int,
	filename string,
	rf *ReleaseFile,
	rows []durability.WrittenRow,
) bool {
	segByOffset := indexSegmentMetaByOffset(rf)
	hasBackup := hasPar2BackupCandidate(downloadDir, candidateSubdirs, filename, rf)
	for _, cand := range postProcCandidateNames(filename, rf) {
		filePath, err := findPartialFile(downloadDir, candidateSubdirs, cand)
		if err != nil {
			continue
		}
		candFile, err := os.Open(filePath) //nolint:gosec // verified candidate inside downloadDir
		if err != nil {
			continue
		}
		if verifyAllRowsAgainstCandidate(candFile, rows, segByOffset, hasBackup) {
			if prev := openFiles[fileIdx]; prev != nil {
				_ = prev.Close() //nolint:errcheck // best-effort close of superseded handle
			}
			openFiles[fileIdx] = candFile
			return true
		}
		_ = candFile.Close() //nolint:errcheck // read-only close
	}
	return false
}

func verifyCrashFileRows(
	downloadDir string,
	candidateSubdirs []string,
	openFiles map[int]*os.File,
	fileIdx int,
	fr durability.FileRow,
	rf *ReleaseFile,
	rows []durability.WrittenRow,
) (bool, error) {
	filename := resolveRowFilename(fr, rf, fileIdx)
	f, err := openCachedPartialFile(downloadDir, candidateSubdirs, openFiles, fileIdx, filename)
	if err == nil {
		for _, r := range rows {
			if rowErr := verifyWrittenRowOnDisk(f, filename, r); rowErr != nil {
				err = rowErr
				break
			}
		}
	}
	if err == nil {
		return false, nil
	}
	if fr.Complete && resolveCompletePostProcFile(downloadDir, candidateSubdirs, openFiles, fileIdx, filename, rf, rows) {
		return true, nil
	}
	return false, err
}

func verifyCrash(ctx context.Context, db *sql.DB, m *ReleaseManifest, cfg VerifyConfig) (*VerifyReport, error) {
	jobID, jobName, err := resolveCrashJob(ctx, db, cfg.JobID, m.Name)
	if err != nil {
		return nil, fmt.Errorf("resolve job id: %w", err)
	}

	store := durability.NewStore(db)
	fRows, err := store.FileRows(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("query job_files: %w", err)
	}
	fileRows := make(map[int]durability.FileRow, len(fRows))
	for _, fr := range fRows {
		fileRows[fr.FileIndex] = fr
	}

	written, err := store.WrittenRows(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("query written_articles: %w", err)
	}

	artIDResolver, err := resolveArticleIDs(cfg.AdminDir, jobID, m)
	if err != nil {
		return nil, fmt.Errorf("resolve article ids: %w", err)
	}

	sortedFiles := sortedManifestFiles(m)
	candidateSubdirs := []string{jobName, m.Name, jobID}

	openFiles := make(map[int]*os.File)
	defer func() {
		for _, f := range openFiles {
			_ = f.Close() //nolint:errcheck // best-effort file close
		}
	}()

	var fileOrder []int
	rowsByFile := make(map[int][]durability.WrittenRow)
	var recordedIDs []string
	for _, r := range written {
		if _, seen := rowsByFile[r.FileIdx]; !seen {
			fileOrder = append(fileOrder, r.FileIdx)
		}
		rowsByFile[r.FileIdx] = append(rowsByFile[r.FileIdx], r)
		if rawMsgID, ok := artIDResolver(int(r.ArtIdx)); ok && rawMsgID != "" {
			recordedIDs = append(recordedIDs, extractMessageID(rawMsgID))
		}
	}

	var fallbackRows int
	for _, fileIdx := range fileOrder {
		rf := manifestFileAt(sortedFiles, fileIdx)
		usedFallback, err := verifyCrashFileRows(cfg.DownloadDir, candidateSubdirs, openFiles, fileIdx, fileRows[fileIdx], rf, rowsByFile[fileIdx])
		if err != nil {
			return nil, err
		}
		if usedFallback {
			fallbackRows += len(rowsByFile[fileIdx])
		}
	}

	slices.Sort(recordedIDs)
	recordedIDs = slices.Compact(recordedIDs)

	if cfg.ServerControlURL != "" && cfg.SnapshotAs != "" {
		if err := postSnapshot(ctx, cfg.ServerControlURL, cfg.SnapshotAs, recordedIDs); err != nil {
			return nil, err
		}
	}

	return &VerifyReport{
		Mode:         "crash",
		JobID:        jobID,
		VerifiedRows: len(written),
		FallbackRows: fallbackRows,
		RecordedIDs:  recordedIDs,
	}, nil
}

func findExpectedOutputFile(searchDirs []string, name string) (string, error) {
	seen := make(map[string]bool)
	for _, dir := range searchDirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		direct := filepath.Join(dir, name)
		if fi, err := os.Stat(direct); err == nil && !fi.IsDir() {
			return direct, nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat %s: %w", direct, err)
		}

		found, err := walkDirForFile(dir, name)
		if err != nil {
			return "", err
		}
		if found != "" {
			return found, nil
		}
	}
	return "", fmt.Errorf("expected output %q not found in complete directories", name)
}

type historyRecord struct {
	jobID       string
	name        string
	status      string
	storage     string
	path        string
	failMessage string
}

func queryHistoryRecords(ctx context.Context, db *sql.DB, explicitID string) ([]historyRecord, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if explicitID != "" {
		rows, err = db.QueryContext(ctx,
			`SELECT nzo_id, COALESCE(name, ''), status, COALESCE(storage, ''), COALESCE(path, ''), COALESCE(fail_message, '') FROM history WHERE nzo_id = ?`,
			explicitID)
		if err != nil {
			rows, err = db.QueryContext(ctx,
				`SELECT nzo_id, '', status, COALESCE(storage, ''), COALESCE(path, ''), COALESCE(fail_message, '') FROM history WHERE nzo_id = ?`,
				explicitID)
		}
	} else {
		rows, err = db.QueryContext(ctx,
			`SELECT nzo_id, COALESCE(name, ''), status, COALESCE(storage, ''), COALESCE(path, ''), COALESCE(fail_message, '') FROM history ORDER BY id DESC`)
		if err != nil {
			rows, err = db.QueryContext(ctx,
				`SELECT nzo_id, '', status, COALESCE(storage, ''), COALESCE(path, ''), COALESCE(fail_message, '') FROM history ORDER BY id DESC`)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // best-effort close

	var out []historyRecord
	for rows.Next() {
		var r historyRecord
		if err := rows.Scan(&r.jobID, &r.name, &r.status, &r.storage, &r.path, &r.failMessage); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func resolveCompleteHistory(ctx context.Context, db *sql.DB, explicitID, releaseName string) (historyRecord, error) {
	records, err := queryHistoryRecords(ctx, db, explicitID)
	if err != nil {
		return historyRecord{}, err
	}
	if len(records) == 0 {
		return historyRecord{}, fmt.Errorf("query history: %w", sql.ErrNoRows)
	}
	if explicitID != "" {
		return records[0], nil
	}

	var exact []historyRecord
	for _, r := range records {
		if releaseName != "" && r.name == releaseName {
			exact = append(exact, r)
		}
	}
	if len(exact) == 1 {
		return exact[0], nil
	}
	if len(exact) > 1 {
		return historyRecord{}, fmt.Errorf("multiple history rows (%d) match release %q; specify --job-id", len(exact), releaseName)
	}

	if len(records) == 1 && records[0].name == "" {
		return records[0], nil
	}
	return historyRecord{}, fmt.Errorf("no history row matching release %q (found %d rows); specify --job-id", releaseName, len(records))
}

func candidateCompleteDirs(completeDir, releaseName string, h historyRecord) []string {
	var dirs []string
	if h.storage != "" {
		if fi, err := os.Stat(h.storage); err == nil && !fi.IsDir() {
			dirs = append(dirs, filepath.Dir(h.storage))
		} else {
			dirs = append(dirs, h.storage)
		}
	}
	if h.path != "" {
		if fi, err := os.Stat(h.path); err == nil && !fi.IsDir() {
			dirs = append(dirs, filepath.Dir(h.path))
		} else {
			dirs = append(dirs, h.path)
		}
	}
	if completeDir != "" {
		if h.name != "" {
			dirs = append(dirs, filepath.Join(completeDir, h.name))
		}
		if releaseName != "" {
			dirs = append(dirs, filepath.Join(completeDir, releaseName))
		}
		if h.storage == "" && h.path == "" {
			dirs = append(dirs, completeDir)
		}
	}
	return dirs
}

func verifyExpectedOutput(searchDirs []string, eo ExpectedOutput) error {
	outPath, err := findExpectedOutputFile(searchDirs, eo.Name)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(outPath) //nolint:gosec // output file within verified search dir
	if err != nil {
		return fmt.Errorf("read output file %s: %w", outPath, err)
	}
	if int64(len(data)) != eo.Size {
		return fmt.Errorf("output file %s size mismatch: got %d, want %d", outPath, len(data), eo.Size)
	}

	gotHash := fmt.Sprintf("%x", sha256.Sum256(data))
	if !strings.EqualFold(gotHash, eo.SHA256) {
		return fmt.Errorf("sha256 mismatch for output %s: got %s, want %s", eo.Name, gotHash, eo.SHA256)
	}
	return nil
}

func checkCompleteHygiene(dirs []string, skipArchiveCleanup bool) error {
	seen := make(map[string]bool)
	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			name := d.Name()
			if strings.Contains(name, ".gonzbd-tmp-") || strings.Contains(name, ".gonzbd-pending-deletions") || strings.Contains(name, ".par2engine-") {
				return fmt.Errorf("hygiene violation: temporary artifact %s found in complete dir", path)
			}
			if !skipArchiveCleanup && !d.IsDir() && (strings.HasSuffix(name, ".rar") || strings.HasSuffix(name, ".par2")) {
				return fmt.Errorf("hygiene violation: archive/parity file %s found in complete dir", path)
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func checkIncompleteHygiene(downloadDir, releaseName, historyName, jobID string) error {
	if downloadDir == "" {
		return nil
	}
	names := []string{historyName, releaseName, jobID}
	var checkDirs []string
	seen := make(map[string]bool)
	for _, n := range names {
		if n == "" {
			continue
		}
		cand := filepath.Join(downloadDir, n)
		if !seen[cand] {
			seen[cand] = true
			checkDirs = append(checkDirs, cand)
		}
	}
	for _, dir := range checkDirs {
		_, err := os.Stat(dir)
		if err == nil {
			return fmt.Errorf("hygiene violation: incomplete directory %s still exists", dir)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat incomplete dir %s: %w", dir, err)
		}
	}
	return nil
}

func compareSnapshotRefetches(ctx context.Context, controlURL, snapshotName string) ([]string, error) {
	baseURL := strings.TrimRight(controlURL, "/")
	client := &http.Client{Timeout: 5 * time.Second}

	snapURL := fmt.Sprintf("%s/snapshot?name=%s", baseURL, url.QueryEscape(snapshotName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, snapURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create snapshot request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch snapshot %s: %w", snapURL, err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close of HTTP response body
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch snapshot returned status %d", resp.StatusCode)
	}
	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}

	statsURL := baseURL + "/stats"
	reqStats, err := http.NewRequestWithContext(ctx, http.MethodGet, statsURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create stats request: %w", err)
	}
	respStats, err := client.Do(reqStats)
	if err != nil {
		return nil, fmt.Errorf("fetch stats %s: %w", statsURL, err)
	}
	defer func() { _ = respStats.Body.Close() }() //nolint:errcheck // best-effort close of HTTP response body
	if respStats.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch stats returned status %d", respStats.StatusCode)
	}
	var stats ServerStats
	if err := json.NewDecoder(respStats.Body).Decode(&stats); err != nil {
		return nil, fmt.Errorf("decode stats: %w", err)
	}

	useRequests := len(stats.Requests) > 0 || len(snap.Requests) > 0
	var refetchedIDs []string
	for _, id := range snap.RecordedIDs {
		cleanID := extractMessageID(id)
		var curCount, snapCount int
		if useRequests {
			curCount = stats.Requests[cleanID]
			snapCount = snap.Requests[cleanID]
		} else {
			curCount = stats.ServedClean[cleanID]
			snapCount = snap.ServedClean[cleanID]
		}
		if curCount > snapCount {
			refetchedIDs = append(refetchedIDs, cleanID)
		}
	}
	slices.Sort(refetchedIDs)
	return refetchedIDs, nil
}

func verifyComplete(ctx context.Context, db *sql.DB, m *ReleaseManifest, cfg VerifyConfig) (*VerifyReport, error) {
	h, err := resolveCompleteHistory(ctx, db, cfg.JobID, m.Name)
	if err != nil {
		return nil, err
	}

	if h.status != "Completed" {
		return nil, fmt.Errorf("job %s status is %q (fail message: %s)", h.jobID, h.status, h.failMessage)
	}

	var dispatchCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dispatch_jobs WHERE id = ?`, h.jobID).Scan(&dispatchCount); err != nil {
		return nil, fmt.Errorf("query dispatch_jobs: %w", err)
	}
	if dispatchCount > 0 {
		return nil, fmt.Errorf("job %s still present in dispatch_jobs (%d rows)", h.jobID, dispatchCount)
	}

	searchDirs := candidateCompleteDirs(cfg.CompleteDir, m.Name, h)
	outputsVerified := 0
	for _, eo := range m.ExpectedOutputs {
		if err := verifyExpectedOutput(searchDirs, eo); err != nil {
			return nil, err
		}
		outputsVerified++
	}

	if cfg.CompleteDir != "" {
		if err := checkCompleteHygiene(searchDirs, cfg.SkipArchiveCleanup); err != nil {
			return nil, err
		}
	}

	if err := checkIncompleteHygiene(cfg.DownloadDir, m.Name, h.name, h.jobID); err != nil {
		return nil, err
	}

	if cfg.ServerControlURL != "" && cfg.CompareSnapshot != "" {
		refetchedIDs, err := compareSnapshotRefetches(ctx, cfg.ServerControlURL, cfg.CompareSnapshot)
		if err != nil {
			return nil, err
		}
		if len(refetchedIDs) > 0 {
			return &VerifyReport{
				Mode:            "complete",
				JobID:           h.jobID,
				OutputsVerified: outputsVerified,
				RefetchedIDs:    refetchedIDs,
			}, fmt.Errorf("refetched already-recorded articles after restart: %v", refetchedIDs)
		}
	}

	return &VerifyReport{
		Mode:            "complete",
		JobID:           h.jobID,
		OutputsVerified: outputsVerified,
	}, nil
}

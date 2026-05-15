package directunpack

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeMockUnrar creates a shell script that mimics unrar -vp behavior.
// It reads MOCK_VOLUMES env var to know how many volumes to prompt for.
// On each [C]ontinue prompt, it writes a "dummy" extracted file.
func writeMockUnrar(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "mock_unrar")
	content := `#!/bin/bash
# Mock unrar that simulates -vp behavior.
# Expects: mock_unrar x -vp ... <archive> <outdir>/
# Uses MOCK_VOLUMES env (default 1) and MOCK_ERROR env.
set -e
VOLUMES="${MOCK_VOLUMES:-1}"
OUTDIR="${@: -1}"  # last argument is outdir
ARCHIVE="${@: -2:1}" # second-to-last is archive

# Create output directory if it doesn't exist.
mkdir -p "$OUTDIR"

echo "Extracting from $ARCHIVE"

# Extract a file in vol 1.
echo "Extracting  testfile.txt"
echo "test content from vol 1" > "${OUTDIR}testfile.txt"

for ((vol=2; vol<=VOLUMES; vol++)); do
    # Prompt for next volume (no trailing newline, just like real unrar).
    printf "[C]ontinue, [Q]uit "
    read -r response
    if [ "$response" = "Q" ] || [ "$response" = "q" ]; then
        exit 0
    fi
    echo ""
    echo "Extracting from volume $vol"
    echo "Extracting  testfile_part${vol}.txt"
    echo "test content from vol $vol" >> "${OUTDIR}testfile.txt"
done

# Check for simulated error.
if [ -n "$MOCK_ERROR" ]; then
    echo "$MOCK_ERROR"
    exit 1
fi

echo ""
echo "All OK"
exit 0
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// writeMockUnrarRetryAbort creates a mock that immediately sends [R]etry, [A]bort.
func writeMockUnrarRetryAbort(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "mock_unrar_retry")
	content := `#!/bin/bash
echo "Extracting from $1"
printf "[R]etry, [A]bort "
read -r response
echo ""
exit 1
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestDirectUnpack_SingleSet_HappyPath(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	downloadDir := filepath.Join(dir, "download")
	_ = os.MkdirAll(extractDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	mockBin := writeMockUnrar(t, dir)

	du := New(
		testLogger(t),
		"test-job",
		downloadDir,
		extractDir,
		Options{UnrarCommand: mockBin},
	)

	// Simulate 3 files in the job.
	du.SetAllFilenames([]string{
		"movie.part01.rar",
		"movie.part02.rar",
		"movie.part03.rar",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create dummy RAR files on disk (mock doesn't read them).
	for i := 1; i <= 3; i++ {
		name := filepath.Join(downloadDir, fmtPart(i))
		_ = os.WriteFile(name, []byte("fake-rar"), 0o644)
	}

	// Set MOCK_VOLUMES so the mock knows to prompt 3 times.
	t.Setenv("MOCK_VOLUMES", "3")

	// Feed all volumes. The DU's waitForNextVolume checks completedVols
	// before blocking, so pre-registering all volumes is safe.
	du.Add(ctx, "movie.part01.rar", filepath.Join(downloadDir, "movie.part01.rar"))
	du.Add(ctx, "movie.part02.rar", filepath.Join(downloadDir, "movie.part02.rar"))
	du.Add(ctx, "movie.part03.rar", filepath.Join(downloadDir, "movie.part03.rar"))

	du.Wait()

	results := du.Results()
	if len(results) != 1 {
		t.Fatalf("expected 1 success set, got %d: %+v", len(results), results)
	}
	ss, ok := results["movie"]
	if !ok {
		t.Fatalf("expected set 'movie' in results, got: %v", results)
	}
	if len(ss.RarParts) == 0 {
		t.Error("expected non-empty RarParts")
	}

	// Verify the extracted file exists.
	extracted := filepath.Join(extractDir, "testfile.txt")
	if _, err := os.Stat(extracted); os.IsNotExist(err) {
		t.Errorf("extracted file not found: %s", extracted)
	}
}

func TestDirectUnpack_OutOfOrder(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	downloadDir := filepath.Join(dir, "download")
	_ = os.MkdirAll(extractDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	mockBin := writeMockUnrar(t, dir)

	du := New(
		testLogger(t),
		"test-job",
		downloadDir,
		extractDir,
		Options{UnrarCommand: mockBin},
	)

	du.SetAllFilenames([]string{
		"movie.part01.rar",
		"movie.part02.rar",
		"movie.part03.rar",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 1; i <= 3; i++ {
		name := filepath.Join(downloadDir, fmtPart(i))
		_ = os.WriteFile(name, []byte("fake-rar"), 0o644)
	}

	t.Setenv("MOCK_VOLUMES", "3")

	// Feed vol 3 first, then vol 1, then vol 2. All are pre-registered;
	// waitForNextVolume finds them in completedVols without blocking.
	du.Add(ctx, "movie.part03.rar", filepath.Join(downloadDir, "movie.part03.rar"))
	du.Add(ctx, "movie.part01.rar", filepath.Join(downloadDir, "movie.part01.rar"))
	du.Add(ctx, "movie.part02.rar", filepath.Join(downloadDir, "movie.part02.rar"))

	du.Wait()

	results := du.Results()
	if len(results) != 1 {
		t.Fatalf("expected 1 success set, got %d", len(results))
	}
}

func TestDirectUnpack_ErrorPattern(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	downloadDir := filepath.Join(dir, "download")
	_ = os.MkdirAll(extractDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	mockBin := writeMockUnrar(t, dir)

	du := New(
		testLogger(t),
		"test-job",
		downloadDir,
		extractDir,
		Options{UnrarCommand: mockBin},
	)

	du.SetAllFilenames([]string{"movie.part01.rar"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = os.WriteFile(filepath.Join(downloadDir, "movie.part01.rar"), []byte("fake"), 0o644)

	// Tell mock to emit an error.
	t.Setenv("MOCK_VOLUMES", "1")
	t.Setenv("MOCK_ERROR", "CRC failed in movie.part01.rar")

	du.Add(ctx, "movie.part01.rar", filepath.Join(downloadDir, "movie.part01.rar"))
	du.Wait()

	results := du.Results()
	if len(results) != 0 {
		t.Fatalf("expected 0 success sets after error, got %d", len(results))
	}
}

func TestDirectUnpack_Abort(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	downloadDir := filepath.Join(dir, "download")
	_ = os.MkdirAll(extractDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	mockBin := writeMockUnrar(t, dir)

	// Use OnLine to detect when the subprocess is running.
	onLine, started := waitForStarted()

	du := New(
		testLogger(t),
		"test-job",
		downloadDir,
		extractDir,
		Options{UnrarCommand: mockBin, OnLine: onLine},
	)

	du.SetAllFilenames([]string{
		"movie.part01.rar",
		"movie.part02.rar",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 1; i <= 2; i++ {
		_ = os.WriteFile(filepath.Join(downloadDir, fmtPart(i)), []byte("fake"), 0o644)
	}

	t.Setenv("MOCK_VOLUMES", "2")

	// Start with vol 1 — subprocess will block waiting for vol 2.
	du.Add(ctx, "movie.part01.rar", filepath.Join(downloadDir, "movie.part01.rar"))

	// Wait for subprocess output instead of sleeping.
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("subprocess never started")
	}

	// Abort instead of feeding vol 2.
	du.Abort()

	// Wait should return quickly.
	done := make(chan struct{})
	go func() {
		du.Wait()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not return after Abort()")
	}

	// Results should be empty after abort.
	results := du.Results()
	if len(results) != 0 {
		t.Fatalf("expected empty results after abort, got %d", len(results))
	}
}

func TestDirectUnpack_ContextCancel(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	downloadDir := filepath.Join(dir, "download")
	_ = os.MkdirAll(extractDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	mockBin := writeMockUnrar(t, dir)

	// Use OnLine to detect when the subprocess is running.
	onLine, started := waitForStarted()

	du := New(
		testLogger(t),
		"test-job",
		downloadDir,
		extractDir,
		Options{UnrarCommand: mockBin, OnLine: onLine},
	)

	du.SetAllFilenames([]string{
		"movie.part01.rar",
		"movie.part02.rar",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	for i := 1; i <= 2; i++ {
		_ = os.WriteFile(filepath.Join(downloadDir, fmtPart(i)), []byte("fake"), 0o644)
	}

	t.Setenv("MOCK_VOLUMES", "2")

	du.Add(ctx, "movie.part01.rar", filepath.Join(downloadDir, "movie.part01.rar"))

	// Wait for subprocess output instead of sleeping.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("subprocess never started")
	}

	// Cancel the context — should unblock waitForNextVolume.
	cancel()

	done := make(chan struct{})
	go func() {
		du.Wait()
		close(done)
	}()

	select {
	case <-done:
		// OK — exited due to context cancellation
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not return after context cancel")
	}
}

func TestDirectUnpack_RetryAbortPrompt(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	downloadDir := filepath.Join(dir, "download")
	_ = os.MkdirAll(extractDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	mockBin := writeMockUnrarRetryAbort(t, dir)

	du := New(
		testLogger(t),
		"test-job",
		downloadDir,
		extractDir,
		Options{UnrarCommand: mockBin},
	)

	du.SetAllFilenames([]string{"movie.part01.rar"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = os.WriteFile(filepath.Join(downloadDir, "movie.part01.rar"), []byte("fake"), 0o644)
	t.Setenv("MOCK_VOLUMES", "1")

	du.Add(ctx, "movie.part01.rar", filepath.Join(downloadDir, "movie.part01.rar"))
	du.Wait()

	results := du.Results()
	if len(results) != 0 {
		t.Fatalf("expected 0 success sets after retry/abort, got %d", len(results))
	}
}

func TestDirectUnpack_WaitNotStarted(t *testing.T) {
	du := New(testLogger(t), "test", "/tmp", "/tmp", Options{})
	// Wait should return immediately when not started.
	done := make(chan struct{})
	go func() {
		du.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait() blocked on unstarted DirectUnpacker")
	}
}

func TestDirectUnpack_AddNonRar(t *testing.T) {
	du := New(testLogger(t), "test", "/tmp", "/tmp", Options{})
	ctx := context.Background()
	// Adding a non-RAR file should be a no-op.
	du.Add(ctx, "readme.nfo", "/tmp/readme.nfo")
	if du.started {
		t.Error("should not have started for non-RAR file")
	}
}

func TestDirectUnpack_AddAfterKilled(t *testing.T) {
	du := New(testLogger(t), "test", "/tmp", "/tmp", Options{})
	du.killed = true
	close(du.done) // so Abort() doesn't block
	ctx := context.Background()
	du.Add(ctx, "movie.part01.rar", "/tmp/movie.part01.rar")
	if du.started {
		t.Error("should not start after killed")
	}
}

func TestDirectUnpack_ErrorPattern_RecordsFailure(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	downloadDir := filepath.Join(dir, "download")
	_ = os.MkdirAll(extractDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	mockBin := writeMockUnrar(t, dir)

	du := New(
		testLogger(t),
		"test-job",
		downloadDir,
		extractDir,
		Options{UnrarCommand: mockBin},
	)

	du.SetAllFilenames([]string{"movie.part01.rar"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = os.WriteFile(filepath.Join(downloadDir, "movie.part01.rar"), []byte("fake"), 0o644)

	t.Setenv("MOCK_VOLUMES", "1")
	t.Setenv("MOCK_ERROR", "CRC failed in movie.part01.rar")

	du.Add(ctx, "movie.part01.rar", filepath.Join(downloadDir, "movie.part01.rar"))
	du.Wait()

	results := du.Results()
	if len(results) != 0 {
		t.Fatalf("expected 0 success sets, got %d", len(results))
	}

	failures := du.Failures()
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(failures), failures)
	}
	f, ok := failures["movie"]
	if !ok {
		t.Fatalf("expected failure for set 'movie', got: %v", failures)
	}
	if !strings.Contains(f.Reason, "CRC failed") {
		t.Errorf("expected failure reason to mention CRC, got: %q", f.Reason)
	}
}

func TestDirectUnpack_RetryAbort_RecordsFailure(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	downloadDir := filepath.Join(dir, "download")
	_ = os.MkdirAll(extractDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	mockBin := writeMockUnrarRetryAbort(t, dir)

	du := New(
		testLogger(t),
		"test-job",
		downloadDir,
		extractDir,
		Options{UnrarCommand: mockBin},
	)

	du.SetAllFilenames([]string{"movie.part01.rar"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = os.WriteFile(filepath.Join(downloadDir, "movie.part01.rar"), []byte("fake"), 0o644)
	t.Setenv("MOCK_VOLUMES", "1")

	du.Add(ctx, "movie.part01.rar", filepath.Join(downloadDir, "movie.part01.rar"))
	du.Wait()

	failures := du.Failures()
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(failures), failures)
	}
	f := failures["movie"]
	if !strings.Contains(f.Reason, "I/O") && !strings.Contains(f.Reason, "read error") {
		t.Errorf("expected I/O error reason, got: %q", f.Reason)
	}
}

func TestDirectUnpack_Abort_RecordsFailures(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	downloadDir := filepath.Join(dir, "download")
	_ = os.MkdirAll(extractDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	mockBin := writeMockUnrar(t, dir)

	// Use OnLine to detect when the subprocess is running.
	onLine, started := waitForStarted()

	du := New(
		testLogger(t),
		"test-job",
		downloadDir,
		extractDir,
		Options{UnrarCommand: mockBin, OnLine: onLine},
	)

	du.SetAllFilenames([]string{
		"movie.part01.rar",
		"movie.part02.rar",
		"subs.part01.rar",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 1; i <= 2; i++ {
		_ = os.WriteFile(filepath.Join(downloadDir, fmtPart(i)), []byte("fake"), 0o644)
	}

	t.Setenv("MOCK_VOLUMES", "2")

	// Start with vol 1 of movie — subprocess will block for vol 2.
	du.Add(ctx, "movie.part01.rar", filepath.Join(downloadDir, "movie.part01.rar"))

	// Queue subs set (vol 1 not ready, so it goes to nextSets).
	du.Add(ctx, "subs.part01.rar", filepath.Join(downloadDir, "subs.part01.rar"))

	// Wait for subprocess output instead of sleeping.
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("subprocess never started")
	}

	// Abort — should record failures for both "movie" (active) and "subs" (queued).
	du.Abort()

	failures := du.Failures()
	if len(failures) != 2 {
		t.Fatalf("expected 2 failures (movie + subs), got %d: %+v", len(failures), failures)
	}

	if f, ok := failures["movie"]; !ok {
		t.Error("expected failure for 'movie'")
	} else if !strings.Contains(f.Reason, "aborted") && !strings.Contains(f.Reason, "killed") && !strings.Contains(f.Reason, "cancel") {
		t.Errorf("expected abort/kill-related reason for movie, got: %q", f.Reason)
	}

	if f, ok := failures["subs"]; !ok {
		t.Error("expected failure for 'subs'")
	} else if !strings.Contains(f.Reason, "aborted") {
		t.Errorf("expected 'aborted' reason for subs, got: %q", f.Reason)
	}

	// Results should be empty after abort.
	results := du.Results()
	if len(results) != 0 {
		t.Fatalf("expected empty results after abort, got %d", len(results))
	}
}

func TestDirectUnpack_ContextCancel_RecordsFailure(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	downloadDir := filepath.Join(dir, "download")
	_ = os.MkdirAll(extractDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	mockBin := writeMockUnrar(t, dir)

	// Use OnLine to detect when the subprocess is running.
	onLine, started := waitForStarted()

	du := New(
		testLogger(t),
		"test-job",
		downloadDir,
		extractDir,
		Options{UnrarCommand: mockBin, OnLine: onLine},
	)

	du.SetAllFilenames([]string{
		"movie.part01.rar",
		"movie.part02.rar",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	for i := 1; i <= 2; i++ {
		_ = os.WriteFile(filepath.Join(downloadDir, fmtPart(i)), []byte("fake"), 0o644)
	}

	t.Setenv("MOCK_VOLUMES", "2")

	du.Add(ctx, "movie.part01.rar", filepath.Join(downloadDir, "movie.part01.rar"))

	// Wait for subprocess output instead of sleeping.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("subprocess never started")
	}

	// Cancel the context.
	cancel()

	done := make(chan struct{})
	go func() {
		du.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not return after context cancel")
	}

	failures := du.Failures()
	if len(failures) == 0 {
		t.Fatal("expected at least 1 failure after context cancel")
	}
	f := failures["movie"]
	if !strings.Contains(f.Reason, "cancel") {
		t.Errorf("expected cancel-related reason, got: %q", f.Reason)
	}
}

func TestDirectUnpack_MultiSet(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	downloadDir := filepath.Join(dir, "download")
	_ = os.MkdirAll(extractDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	mockBin := writeMockUnrar(t, dir)

	du := New(
		testLogger(t),
		"test-job",
		downloadDir,
		extractDir,
		Options{UnrarCommand: mockBin},
	)

	du.SetAllFilenames([]string{
		"movie.part01.rar",
		"subs.part01.rar",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create all files on disk.
	_ = os.WriteFile(filepath.Join(downloadDir, "movie.part01.rar"), []byte("fake"), 0o644)
	_ = os.WriteFile(filepath.Join(downloadDir, "subs.part01.rar"), []byte("fake"), 0o644)

	// Single volume each — both sets complete without prompting.
	t.Setenv("MOCK_VOLUMES", "1")

	// Feed movie vol 1 → starts subprocess.
	du.Add(ctx, "movie.part01.rar", filepath.Join(downloadDir, "movie.part01.rar"))

	// Feed subs vol 1 → queued as next set.
	du.Add(ctx, "subs.part01.rar", filepath.Join(downloadDir, "subs.part01.rar"))

	du.Wait()

	results := du.Results()
	if _, ok := results["movie"]; !ok {
		t.Errorf("expected 'movie' in results, got: %v", results)
	}
	if _, ok := results["subs"]; !ok {
		t.Errorf("expected 'subs' in results, got: %v", results)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 success sets, got %d: %v", len(results), results)
	}
}

func TestDirectUnpack_OnLineCallback(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	downloadDir := filepath.Join(dir, "download")
	_ = os.MkdirAll(extractDir, 0o755)
	_ = os.MkdirAll(downloadDir, 0o755)

	mockBin := writeMockUnrar(t, dir)

	var lines []string
	du := New(
		testLogger(t),
		"test-job",
		downloadDir,
		extractDir,
		Options{
			UnrarCommand: mockBin,
			OnLine: func(line string) {
				lines = append(lines, line)
			},
		},
	)

	du.SetAllFilenames([]string{"movie.part01.rar"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = os.WriteFile(filepath.Join(downloadDir, "movie.part01.rar"), []byte("fake"), 0o644)
	t.Setenv("MOCK_VOLUMES", "1")

	du.Add(ctx, "movie.part01.rar", filepath.Join(downloadDir, "movie.part01.rar"))
	du.Wait()

	if len(lines) == 0 {
		t.Error("expected OnLine callback to receive output lines")
	}
}

// --- helpers ---

// waitForStarted returns an OnLine callback and a channel that is closed
// when the first line of unrar output is received, indicating the subprocess
// has started and is processing. This replaces time.Sleep for test
// synchronization.
func waitForStarted() (func(string), <-chan struct{}) {
	ch := make(chan struct{})
	var once sync.Once
	return func(string) {
		once.Do(func() { close(ch) })
	}, ch
}

func fmtPart(n int) string {
	return fmt.Sprintf("movie.part%02d.rar", n)
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})).
		With("test", t.Name())
}

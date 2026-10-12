package postproc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeTree creates each slash-separated rel under dir, with the given
// content.
func writeTree(t *testing.T, dir, content string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// filesJob returns a job whose DownloadDir is a fresh directory holding
// rels, destined for finalDir. Unlike stageJob it records no file list: the
// real FinalizeStage must.
func filesJob(t *testing.T, finalDir string, flat bool, rels ...string) *Job {
	t.Helper()
	j, dir := stageJob(t)
	j.outputFiles, j.outputFilesSet = nil, false
	writeTree(t, dir, "x", rels...)
	j.FinalDir = finalDir
	j.FlatLayout = flat
	return j
}

// runFinalizeThenScript runs the real finalize stage and then a script that
// writes $SAB_FILES to a file, returning the raw variable and the script
// stage's error. A finalize error fails the test.
func runFinalizeThenScript(t *testing.T, j *Job) (string, error) {
	t.Helper()
	if err := NewFinalizeStage().Run(t.Context(), j); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	scriptDir := t.TempDir()
	out := filepath.Join(scriptDir, "files.json")
	writeScript(t, filepath.Join(scriptDir, "files.sh"),
		fmt.Appendf(nil, "#!/bin/sh\nprintf '%%s' \"$SAB_FILES\" > %s\n", out))
	j.Script = "files.sh"
	err := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "").Run(t.Context(), j)
	data, readErr := os.ReadFile(out)
	if readErr != nil && err == nil {
		t.Fatalf("script wrote no output: %v", readErr)
	}
	return string(data), err
}

func decodeFiles(t *testing.T, raw string) []string {
	t.Helper()
	var got []string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("SAB_FILES %q is not a JSON array: %v", raw, err)
	}
	return got
}

func TestSABFiles_PerJobNestedSorted(t *testing.T) {
	t.Parallel()
	final := filepath.Join(t.TempDir(), "complete", "Rel")
	// "a.txt" must sort before "a/z.txt" ('.' < '/'), the reverse of the
	// order a directory walk visits them in, so only a sort over the full
	// relative paths yields the want below.
	j := filesJob(t, final, false, "b.mkv", "a/z.txt", "a.txt", "sub/deep/c.nfo", "sub/d.srt")
	if err := os.MkdirAll(filepath.Join(j.DownloadDir, "emptydir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b.mkv", filepath.Join(j.DownloadDir, "link.mkv")); err != nil {
		t.Fatal(err)
	}

	raw, err := runFinalizeThenScript(t, j)
	if err != nil {
		t.Fatalf("script stage: %v", err)
	}
	want := []string{"a.txt", "a/z.txt", "b.mkv", "sub/d.srt", "sub/deep/c.nfo"}
	if got := decodeFiles(t, raw); !slices.Equal(got, want) {
		t.Errorf("SAB_FILES = %v, want %v", got, want)
	}
}

func TestSABFiles_EmptyDir(t *testing.T) {
	t.Parallel()
	j := filesJob(t, filepath.Join(t.TempDir(), "complete", "Rel"), false)
	raw, err := runFinalizeThenScript(t, j)
	if err != nil {
		t.Fatalf("script stage: %v", err)
	}
	if raw != "[]" {
		t.Errorf("SAB_FILES = %q, want %q", raw, "[]")
	}
}

// A flat category's directory is shared, so each job's list must be the files
// finalize moved for it, not whatever the directory holds. "common.nfo" is
// delivered by both jobs to one path: the second overwrites the first (the
// move is os.Rename), and each job's list still names the path it produced.
func TestSABFiles_FlatLayoutListsOnlyOwnFiles(t *testing.T) {
	t.Parallel()
	shared := filepath.Join(t.TempDir(), "complete", "movies")
	writeTree(t, shared, "x", "earlier-job.mkv")

	a := filesJob(t, shared, true, "a.mkv", "sub/a.nfo", "common.nfo")
	b := filesJob(t, shared, true, "b.mkv", "sub/b.nfo", "common.nfo")

	gotA, err := runFinalizeThenScript(t, a)
	if err != nil {
		t.Fatalf("job a script stage: %v", err)
	}
	gotB, err := runFinalizeThenScript(t, b)
	if err != nil {
		t.Fatalf("job b script stage: %v", err)
	}
	if want := []string{"a.mkv", "common.nfo", "sub/a.nfo"}; !slices.Equal(decodeFiles(t, gotA), want) {
		t.Errorf("job a SAB_FILES = %s, want %v", gotA, want)
	}
	if want := []string{"b.mkv", "common.nfo", "sub/b.nfo"}; !slices.Equal(decodeFiles(t, gotB), want) {
		t.Errorf("job b SAB_FILES = %s, want %v", gotB, want)
	}
	for _, rel := range []string{"earlier-job.mkv", "a.mkv", "b.mkv", "sub/a.nfo", "sub/b.nfo", "common.nfo"} {
		if _, err := os.Stat(filepath.Join(shared, filepath.FromSlash(rel))); err != nil {
			t.Errorf("shared dir lacks %s: %v", rel, err)
		}
	}
}

// A flat job's file removed by pending deletions (extracted archives) is not
// delivered and so is not listed.
func TestSABFiles_FlatLayoutSkipsPendingDeletions(t *testing.T) {
	t.Parallel()
	shared := filepath.Join(t.TempDir(), "complete", "movies")
	j := filesJob(t, shared, true, "movie.mkv", "movie.rar")
	j.PendingDeletions = []string{"movie.rar"}

	raw, err := runFinalizeThenScript(t, j)
	if err != nil {
		t.Fatalf("script stage: %v", err)
	}
	if want := []string{"movie.mkv"}; !slices.Equal(decodeFiles(t, raw), want) {
		t.Errorf("SAB_FILES = %s, want %v", raw, want)
	}
}

// A failed job's files stay in its own DownloadDir, which is what the script
// is handed, so that is what is listed.
func TestSABFiles_FailedJobListsDownloadDir(t *testing.T) {
	t.Parallel()
	j := filesJob(t, filepath.Join(t.TempDir(), "complete", "Rel"), true, "left.bin")
	j.UnpackError = true

	raw, err := runFinalizeThenScript(t, j)
	if err != nil {
		t.Fatalf("script stage: %v", err)
	}
	if want := []string{"left.bin"}; !slices.Equal(decodeFiles(t, raw), want) {
		t.Errorf("SAB_FILES = %s, want %v", raw, want)
	}
}

func TestSABFiles_WalkErrorFailsStage(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 directory, so no walk error can be provoked")
	}
	final := filepath.Join(t.TempDir(), "complete", "Rel")
	j := filesJob(t, final, false, "ok.mkv", "locked/f")
	// The directory renames fine and stays usable as the script's working
	// directory; only listing it can fail.
	if err := os.Chmod(filepath.Join(j.DownloadDir, "locked"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(final, "locked"), 0o750) }) //nolint:errcheck // best-effort so TempDir cleanup can remove it

	if err := NewFinalizeStage().Run(t.Context(), j); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	scriptDir := t.TempDir()
	ran := filepath.Join(scriptDir, "ran")
	writeScript(t, filepath.Join(scriptDir, "files.sh"), fmt.Appendf(nil, "#!/bin/sh\ntouch %s\n", ran))
	j.Script = "files.sh"

	if err := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "").Run(t.Context(), j); err == nil {
		t.Fatal("expected the script stage to report the failed listing")
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("script ran despite the failed listing")
	}
}

func TestSABFiles_TooLargeFailsStage(t *testing.T) {
	t.Parallel()
	rels := make([]string, 300)
	for i := range rels {
		rels[i] = strings.Repeat("d", 200) + "/" + fmt.Sprintf("%s%03d", strings.Repeat("n", 197), i)
	}
	j := filesJob(t, filepath.Join(t.TempDir(), "complete", "Rel"), false, rels...) // ~400 bytes each, over 100 KiB
	if _, err := runFinalizeThenScript(t, j); err == nil {
		t.Fatal("expected an error when SAB_FILES would exceed the per-variable limit")
	}
}

// A job finalize never ran for must not export an empty list as if it were
// the real one.
func TestSABFiles_UnrecordedListFailsStage(t *testing.T) {
	t.Parallel()
	j, _ := stageJob(t)
	j.outputFiles, j.outputFilesSet = nil, false
	scriptDir := t.TempDir()
	writeScript(t, filepath.Join(scriptDir, "s.sh"), []byte("#!/bin/sh\nexit 0\n"))
	j.Script = "s.sh"
	if err := NewScriptStage(scriptDir, "/tmp/complete", "test", "", "").Run(t.Context(), j); err == nil {
		t.Fatal("expected an error for a job with no recorded file list")
	}
}

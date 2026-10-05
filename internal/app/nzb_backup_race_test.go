package app

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
)

func readBackup(t *testing.T, dir, name string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("open backup %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip %s: %v", name, err)
	}
	b, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestWriteNZBBackup_ConcurrentWritersOfOneFilenameGetDistinctFiles pins that
// choosing a backup name and creating the file are one step. The hook runs a
// complete second write between the first one's choice and its publish, so
// both choose the same free name; the second publishes it first, and the
// first must take the next suffix rather than replace it.
func TestWriteNZBBackup_ConcurrentWritersOfOneFilenameGetDistinctFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var fired atomic.Bool
	var innerName string
	var innerErr error
	chosen := func(name string) {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		innerName, innerErr = writeNZBBackup(dir, "Show.nzb", []byte("<nzb>second</nzb>"), nil)
	}
	outerName, err := writeNZBBackup(dir, "Show.nzb", []byte("<nzb>first</nzb>"), chosen)
	if err != nil {
		t.Fatalf("writeNZBBackup(first): %v", err)
	}
	if innerErr != nil {
		t.Fatalf("setup: the racing write failed: %v", innerErr)
	}
	if outerName == innerName {
		t.Fatalf("both writers were given %q", outerName)
	}
	if got := readBackup(t, dir, innerName); got != "<nzb>second</nzb>" {
		t.Errorf("%s holds %q, want the second writer's NZB", innerName, got)
	}
	if got := readBackup(t, dir, outerName); got != "<nzb>first</nzb>" {
		t.Errorf("%s holds %q, want the first writer's NZB", outerName, got)
	}
}

// TestWriteNZBBackup_LeavesNothingButTheBackup pins that the staging file the
// exclusive publish uses is not left in admin/nzb beside the backup.
func TestWriteNZBBackup_LeavesNothingButTheBackup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	name, err := writeNZBBackup(dir, "Show.nzb", []byte("<nzb>x</nzb>"), nil)
	if err != nil {
		t.Fatalf("writeNZBBackup: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		t.Errorf("admin/nzb holds %v, want only %s", entries, name)
	}
}

// TestAddJob_AFailedIngestDoesNotRemoveAnotherIngestsBackup pins the cleanup
// half: two ingests of one filename, the second registered while the first is
// between choosing its backup name and publishing it, and the first then
// refused. Its cleanup removes the backup it created and nothing else, so the
// registered job's backup, which a retry reads, is intact.
func TestAddJob_AFailedIngestDoesNotRemoveAnotherIngestsBackup(t *testing.T) {
	t.Parallel()
	application, _ := newNameRaceApp(t)
	nzbDir := filepath.Join(application.config.GetGeneral().AdminDir, "nzb")

	first, hf, _ := buildNamedIngestJob(t, application, "first", "same")
	second, hs, _ := buildNamedIngestJob(t, application, "second", "same")

	var fired atomic.Bool
	var secondErr error
	application.nzbBackupChosenHook = func(string) {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		secondErr = application.AddJob(t.Context(), second, hs, []byte("<nzb>second</nzb>"), true)
		// Refuses the first ingest's registration, after its backup is written.
		if err := application.dispatcher.Stop(); err != nil {
			t.Errorf("setup: Stop: %v", err)
		}
	}
	if err := application.AddJob(t.Context(), first, hf, []byte("<nzb>first</nzb>"), true); err == nil {
		t.Fatal("AddJob(first) succeeded, want the stopped dispatcher to refuse it")
	}
	if secondErr != nil {
		t.Fatalf("setup: AddJob(second): %v", secondErr)
	}

	backups, err := os.ReadDir(nzbDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make([]string, 0, len(backups))
	for _, b := range backups {
		names = append(names, b.Name())
	}
	row, ok := application.dispatcher.Row(second.ID())
	if !ok {
		t.Fatal("the second job is not registered")
	}
	if !slices.Equal(names, []string{row.Header.NZBBackup}) {
		t.Fatalf("admin/nzb holds %v, want only the registered job's backup %q", names, row.Header.NZBBackup)
	}
	if got := readBackup(t, nzbDir, row.Header.NZBBackup); got != "<nzb>second</nzb>" {
		t.Errorf("the registered job's backup holds %q, want its own NZB", got)
	}
}
